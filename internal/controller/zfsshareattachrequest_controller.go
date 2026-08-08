package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/nvmeauth"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// zfsShareAttachRequestFinalizer lets the aggregation reconciler recompute (and
// possibly tear down) a volume's ZfsShare when an attach request is deleted,
// before the object disappears. The ZfsShare is ref-counted by the set of attach
// requests, so it is not garbage-collected via owner references.
const zfsShareAttachRequestFinalizer = "storage.simple-zfs-csi.io/zfsshareattachrequest"

// attachRequeue is how often an attach request that is not yet ready is
// re-checked, as a fallback to the ZfsShare watch.
const attachRequeue = 3 * time.Second

// ZfsShareAttachRequestReconciler aggregates per-(volume, node) attach requests
// into a single lazily-managed ZfsShare per volume (ADR-0010). It runs in the
// operator manager (leader-elected). As long as at least one request exists for
// a volume it ensures a ZfsShare whose allow-list is the resolved set of
// requesting nodes; when the last request is removed it deletes the ZfsShare so
// the export is torn down. Each request's status reflects whether the export is
// live for its node, which the CSI controller waits on before returning from
// ControllerPublishVolume.
type ZfsShareAttachRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Namespace is the driver's release namespace, where per-attach DH-CHAP
	// Secrets are created (must be readable by the nvmet aggregator and csi-node).
	Namespace string
	// DHChapEnabled turns on NVMe-oF in-band DH-CHAP authentication (ADR-0011).
	// NQN allow-listing is applied regardless.
	DHChapEnabled bool
	// DHChapSecretKey is the data key under which the DH-CHAP secret is stored in
	// the Secret and recorded on the NetworkExport. Empty defaults to "dhchap-key".
	DHChapSecretKey string
	// APIReader reads straight from the API server, bypassing the manager's
	// informer cache. SetupWithManager wires it; it is used only where a stale
	// read would tear down or move a live export (see gateReader).
	APIReader client.Reader
}

// gateReader returns the reader used for decisions whose blast radius is a live
// export rather than a delayed reconcile: tearing the ZfsShare down because no
// attach request remains, picking the single node a zvol is exported to, and
// (ADR-0031) deciding a local attach needs no ZfsShare at all. A stale cached
// read of ZfsPool.status.currentNode here could wrongly conclude "still local"
// after the pool has actually moved elsewhere, skipping the export a node now
// needs. Attach requests are authored by the CSI controller with an uncached
// client, and a ZfsShare-triggered reconcile has no ordering relationship to the
// attach-request informer at all, so a cache that has not caught up can show an
// empty (or older-entry-missing) set and unexport a volume a node is actively
// using (known-pitfalls.md #19).
//
// Falls back to the cached client when no APIReader is wired (tests).
func (r *ZfsShareAttachRequestReconciler) gateReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsshareattachrequests,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsshareattachrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsshares,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsdatasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete

// Reconcile ensures the aggregated ZfsShare for the request's volume and reflects
// its readiness back into the request status.
func (r *ZfsShareAttachRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ar storagev1alpha1.ZfsShareAttachRequest
	if err := r.Get(ctx, req.NamespacedName, &ar); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	volume := ar.Spec.VolumeName

	// Deletion: recompute the volume's share without this (terminating) request,
	// then release the finalizer.
	if !ar.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&ar, zfsShareAttachRequestFinalizer) {
			if _, _, _, err := r.reconcileVolume(ctx, volume); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&ar, zfsShareAttachRequestFinalizer)
			if err := r.Update(ctx, &ar); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&ar, zfsShareAttachRequestFinalizer) {
		controllerutil.AddFinalizer(&ar, zfsShareAttachRequestFinalizer)
		if err := r.Update(ctx, &ar); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	share, exported, localOnly, err := r.reconcileVolume(ctx, volume)
	if err != nil {
		// The backing ZfsDataset may not exist yet, or a node IP is not resolvable;
		// surface it and retry.
		return ctrl.Result{RequeueAfter: attachRequeue}, r.setStatus(ctx, &ar, false, volume, "Pending", err.Error())
	}

	// This request is ready only when its own node is the exported one — for a
	// single-node zvol a losing racer is deliberately not exported — and either the
	// attach is local (no ZfsShare needed at all, ADR-0031) or the export is live
	// for the current generation.
	nodeExported := slices.Contains(exported, ar.Spec.NodeName)
	ready := nodeExported && (localOnly || shareReadyForGeneration(share))
	reason, msg := "Exported", fmt.Sprintf("export live on the current generation for %q", volume)
	switch {
	case !nodeExported:
		reason, msg = "Waiting", fmt.Sprintf("volume %q is exported to another node; waiting for it to detach", volume)
	case localOnly:
		reason, msg = "Local", fmt.Sprintf("volume %q is local to its own node; no network export needed", volume)
	case !ready:
		reason, msg = "Exporting", "waiting for the aggregated share to be exported"
	}
	if err := r.setStatus(ctx, &ar, ready, volume, reason, msg); err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return ctrl.Result{RequeueAfter: attachRequeue}, nil
	}
	return ctrl.Result{}, nil
}

// reconcileVolume ensures (or deletes) the ZfsShare for a volume from the current
// set of active attach requests. It returns the resulting ZfsShare (nil when
// none remain, including when the sole attach is local — see localOnly), the
// nodes actually exported to, and whether this is a local-only NVMe-oF attach
// (ADR-0031): the winning node is also the pool's own current node, so the node
// plugin will read the zvol's local device path directly
// (internal/csi/node.go's publishLocalZvol) and no ZfsShare/NetworkExport is
// created at all. A ZfsShare's existence should always mean "this volume is
// actually exported over the network" — never sometimes silently a no-op — so
// the decision not to export lives here, before any ZfsShare would be written,
// rather than downstream in the translator.
func (r *ZfsShareAttachRequestReconciler) reconcileVolume(ctx context.Context, volume string) (share *storagev1alpha1.ZfsShare, exported []string, localOnly bool, err error) {
	logger := log.FromContext(ctx)

	nodes, err := r.activeNodes(ctx, r.Client, volume)
	if err != nil {
		return nil, nil, false, err
	}

	shareKey := client.ObjectKey{Name: volume}

	// An empty set is the one answer that destroys something, so never act on the
	// cache's word for it: confirm against the API server first. Costs one live
	// LIST on the teardown path only.
	if len(nodes) == 0 {
		if nodes, err = r.activeNodes(ctx, r.gateReader(), volume); err != nil {
			return nil, nil, false, err
		}
	}

	// No consumers left: tear the share down (its NetworkExport is GC'd with it),
	// and remove any per-attach DH-CHAP secret.
	if len(nodes) == 0 {
		share := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: volume}}
		if err := r.Delete(ctx, share); err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, false, err
		}
		if err := r.deleteDHChapSecret(ctx, volume); err != nil {
			return nil, nil, false, err
		}
		return nil, nil, false, nil
	}

	var ds storagev1alpha1.ZfsDataset
	if err := r.Get(ctx, shareKey, &ds); err != nil {
		return nil, nil, false, fmt.Errorf("get ZfsDataset %q: %w", volume, err)
	}
	protocol, err := protocolForDatasetType(ds.Spec.Type)
	if err != nil {
		return nil, nil, false, err
	}

	// exported is the set of nodes the share is actually exported to. For NFS
	// (RWX) that is every requesting node; for a zvol (NVMe-oF, single-node) it is
	// exactly one node so the block device is never attached read-write to two
	// nodes at once, even if a rare attach race leaves several attach requests.
	exported = nodes

	var nfsClients []storagev1alpha1.NFSClient
	var nvmeSpec *storagev1alpha1.NVMeoFExportSpec
	switch protocol {
	case storagev1alpha1.ProtocolNFS:
		var pool storagev1alpha1.ZfsPool
		if err := r.gateReader().Get(ctx, client.ObjectKey{Name: zpool.ResourceName(ds.Spec.PoolGUID)}, &pool); err != nil {
			return nil, nil, false, fmt.Errorf("get ZfsPool %q: %w", ds.Spec.PoolGUID, err)
		}

		if r.isSingleNodeVolume(ctx, volume) {
			// Single-node (RWO) NFS: "oldest wins" safety + optional local passthrough
			// (ADR-0031 Phase 2). Only one node may hold the NFS mount at a time; in
			// the rare case of multiple requests (e.g. a forced pod move that attaches
			// to the new node before the old one detaches), export to the oldest only.
			node, err := r.oldestAttachNode(ctx, volume)
			if err != nil {
				return nil, nil, false, err
			}
			if len(nodes) > 1 {
				logger.Info("NFS single-node volume has attach requests from multiple nodes; exporting only to the oldest",
					"volume", volume, "nodes", nodes, "chosen", node)
			}
			exported = []string{node}

			// Local passthrough: if the winning node IS the pool's current node, skip
			// the ZfsShare entirely — the node plugin bind-mounts the dataset directly.
			// A ZfsShare's existence means "exported over the network" (ADR-0031); we
			// must not create one for a local-only case.
			if pool.Status.CurrentNode != "" && pool.Status.CurrentNode == node {
				share := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: volume}}
				if err := r.Delete(ctx, share); err != nil && !apierrors.IsNotFound(err) {
					return nil, nil, false, err
				}
				return nil, exported, true, nil
			}

			// Remote single-node: NFS export to the one winning node only.
			nfsClients, err = r.nfsClientsForNodes(ctx, exported)
			if err != nil {
				return nil, nil, false, err
			}
		} else {
			// Multi-node (RWX): all requesting nodes get NFS access. No local
			// passthrough ever: mixing bind-mounts and NFS mounts of the same directory
			// on different nodes uses different lock domains (POSIX vs. lockd) and
			// breaks file-locking semantics (ADR-0031).
			nfsClients, err = r.nfsClientsForNodes(ctx, nodes)
			if err != nil {
				return nil, nil, false, err
			}
		}
	case storagev1alpha1.ProtocolNVMeoF:
		// Single-node safety (defense in depth; the CSI controller already rejects a
		// second publish in the normal flow): export to exactly one node. The oldest
		// attach request wins so an established export is never stolen by a racing
		// newcomer; the losing request stays not-ready (see Reconcile), so its
		// ControllerPublish times out and retries until it either wins or gives up.
		node, err := r.oldestAttachNode(ctx, volume)
		if err != nil {
			return nil, nil, false, err
		}
		if len(nodes) > 1 {
			logger.Info("zvol has attach requests from multiple nodes; exporting only to the oldest",
				"volume", volume, "nodes", nodes, "chosen", node)
		}
		exported = []string{node}

		var pool storagev1alpha1.ZfsPool
		if err := r.gateReader().Get(ctx, client.ObjectKey{Name: zpool.ResourceName(ds.Spec.PoolGUID)}, &pool); err != nil {
			return nil, nil, false, fmt.Errorf("get ZfsPool %q: %w", ds.Spec.PoolGUID, err)
		}
		if pool.Status.CurrentNode != "" && pool.Status.CurrentNode == node {
			share := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: volume}}
			if err := r.Delete(ctx, share); err != nil && !apierrors.IsNotFound(err) {
				return nil, nil, false, err
			}
			if err := r.deleteDHChapSecret(ctx, volume); err != nil {
				return nil, nil, false, err
			}
			return nil, exported, true, nil
		}

		nvmeSpec, err = r.nvmeExportSpec(ctx, volume, exported)
		if err != nil {
			return nil, nil, false, err
		}
	}

	shareObj := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: volume}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, shareObj, func() error {
		shareObj.Spec.PoolGUID = ds.Spec.PoolGUID
		shareObj.Spec.Dataset = ds.Spec.Dataset
		shareObj.Spec.Protocol = protocol
		switch protocol {
		case storagev1alpha1.ProtocolNFS:
			shareObj.Spec.NFS = &storagev1alpha1.NFSExportSpec{Clients: nfsClients}
			shareObj.Spec.NVMeoF = nil
		case storagev1alpha1.ProtocolNVMeoF:
			// NVMe-oF is single-node (ADR-0010): default-deny by the attached node's
			// derived host NQN, plus optional per-attach DH-CHAP (ADR-0011).
			shareObj.Spec.NVMeoF = nvmeSpec
			shareObj.Spec.NFS = nil
		}
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("aggregated ZfsShare", "op", op, "volume", volume, "nodes", exported)
	}

	// `shareObj` is what the API server returned from the create/update above, so its
	// Generation and Status are authoritative. Re-reading it through the manager's
	// cache here would be a read-your-own-write: an informer still holding the
	// pre-update copy reports the *old* Generation alongside the *old*
	// ObservedGeneration, which match, so shareReadyForGeneration would report the
	// attach ready before the node has applied the new allow-list
	// (known-pitfalls.md #19).
	return shareObj, exported, false, nil
}

// oldestAttachNode returns the node whose non-terminating attach request for the
// volume was created first (tie-break by object name for determinism). It is the
// winner for a single-node (zvol) volume when more than one node races to attach.
func (r *ZfsShareAttachRequestReconciler) oldestAttachNode(ctx context.Context, volume string) (string, error) {
	var list storagev1alpha1.ZfsShareAttachRequestList
	// Authoritative: a cached list can only ever *omit* entries, and omitting the
	// oldest one hands the export to a different node — moving a block device that
	// may already be mounted.
	if err := r.gateReader().List(ctx, &list); err != nil {
		return "", err
	}
	var oldest *storagev1alpha1.ZfsShareAttachRequest
	for i := range list.Items {
		it := &list.Items[i]
		if it.Spec.VolumeName != volume || !it.DeletionTimestamp.IsZero() {
			continue
		}
		switch {
		case oldest == nil:
			oldest = it
		case it.CreationTimestamp.Equal(&oldest.CreationTimestamp):
			if it.Name < oldest.Name {
				oldest = it
			}
		case it.CreationTimestamp.Before(&oldest.CreationTimestamp):
			oldest = it
		}
	}
	if oldest == nil {
		return "", fmt.Errorf("no active attach request for volume %q", volume)
	}
	return oldest.Spec.NodeName, nil
}

// activeNodes returns the sorted, de-duplicated set of node names that currently
// hold a non-terminating attach request for the volume, as seen by the given
// reader (the cached client for routine work, the API server directly when the
// answer decides whether to tear an export down).
func (r *ZfsShareAttachRequestReconciler) activeNodes(ctx context.Context, reader client.Reader, volume string) ([]string, error) {
	var list storagev1alpha1.ZfsShareAttachRequestList
	if err := reader.List(ctx, &list); err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var nodes []string
	for i := range list.Items {
		it := &list.Items[i]
		if it.Spec.VolumeName != volume || !it.DeletionTimestamp.IsZero() {
			continue
		}
		if _, ok := seen[it.Spec.NodeName]; ok {
			continue
		}
		seen[it.Spec.NodeName] = struct{}{}
		nodes = append(nodes, it.Spec.NodeName)
	}
	sort.Strings(nodes)
	return nodes, nil
}

// nfsClientsForNodes resolves each node name to its internal IP and builds a
// stable NFS allow-list. Options are left empty so the NFS backend applies its
// safe default set.
func (r *ZfsShareAttachRequestReconciler) nfsClientsForNodes(ctx context.Context, nodes []string) ([]storagev1alpha1.NFSClient, error) {
	clients := make([]storagev1alpha1.NFSClient, 0, len(nodes))
	for _, name := range nodes {
		ip, err := r.nodeInternalIP(ctx, name)
		if err != nil {
			return nil, err
		}
		clients = append(clients, storagev1alpha1.NFSClient{Client: ip})
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("no NFS clients resolved from nodes %v", nodes)
	}
	return clients, nil
}

// nodeInternalIP returns a node's first InternalIP address.
func (r *ZfsShareAttachRequestReconciler) nodeInternalIP(ctx context.Context, nodeName string) (string, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return "", fmt.Errorf("get node %q: %w", nodeName, err)
	}
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("node %q has no InternalIP", nodeName)
}

// nvmeExportSpec builds the NVMe-oF export spec for a single-node attach:
// default-deny to the node's derived per-attach host NQN (ADR-0011), plus an
// optional per-attach DH-CHAP secret when in-band auth is enabled.
func (r *ZfsShareAttachRequestReconciler) nvmeExportSpec(ctx context.Context, volume string, nodes []string) (*storagev1alpha1.NVMeoFExportSpec, error) {
	// NVMe-oF is single-node; the requesting node is the sole allowed host.
	node := nodes[0]
	hostNQN, _ := nvmeauth.HostIdentity(node, volume)
	spec := &storagev1alpha1.NVMeoFExportSpec{AllowedHosts: []string{hostNQN}}

	if r.DHChapEnabled {
		name, err := r.ensureDHChapSecret(ctx, volume)
		if err != nil {
			return nil, err
		}
		spec.DHChapSecretName = name
		spec.DHChapSecretNamespace = r.Namespace
		spec.DHChapSecretKey = nvmeauth.ResolveSecretKey(r.DHChapSecretKey)
	}
	return spec, nil
}

// ensureDHChapSecret creates (once) the per-attach DH-CHAP Secret for a volume
// and returns its name. The key is generated only on first creation, so it is
// stable for the life of the attachment and rotates only when the volume is
// fully detached (the Secret is deleted) and later reattached.
func (r *ZfsShareAttachRequestReconciler) ensureDHChapSecret(ctx context.Context, volume string) (string, error) {
	if r.Namespace == "" {
		return "", fmt.Errorf("operator namespace is unknown; cannot manage DH-CHAP secret")
	}
	name := dhchapSecretName(volume)
	var sec corev1.Secret
	err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, &sec)
	switch {
	case apierrors.IsNotFound(err):
		key, gerr := nvmeauth.GenerateDHChapKey()
		if gerr != nil {
			return "", gerr
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{nvmeauth.ResolveSecretKey(r.DHChapSecretKey): []byte(key)},
		}
		if err := r.Create(ctx, &sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", err
		}
		return name, nil
	case err != nil:
		return "", err
	default:
		return name, nil
	}
}

// deleteDHChapSecret best-effort removes a volume's per-attach DH-CHAP Secret.
func (r *ZfsShareAttachRequestReconciler) deleteDHChapSecret(ctx context.Context, volume string) error {
	if r.Namespace == "" {
		return nil
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: dhchapSecretName(volume), Namespace: r.Namespace}}
	if err := r.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// dhchapSecretName is the deterministic Secret name for a volume's DH-CHAP key.
func dhchapSecretName(volume string) string { return "dhchap-" + volume }

// isSingleNodeVolume reports whether the volume was provisioned with a
// single-node access mode (SingleNode=true on the attach request). It uses the
// authoritative API-server reader so a stale cache cannot produce a false
// positive that skips an NFS export prematurely. All attach requests for the
// same PV carry the same SingleNode value (it is derived from the PV's fixed
// access mode); checking any one non-terminating request is sufficient.
func (r *ZfsShareAttachRequestReconciler) isSingleNodeVolume(ctx context.Context, volume string) bool {
	var list storagev1alpha1.ZfsShareAttachRequestList
	if err := r.gateReader().List(ctx, &list); err != nil {
		return false
	}
	for i := range list.Items {
		it := &list.Items[i]
		if it.Spec.VolumeName != volume || !it.DeletionTimestamp.IsZero() {
			continue
		}
		return it.Spec.SingleNode
	}
	return false
}

// setStatus patches the attach request status subresource.
func (r *ZfsShareAttachRequestReconciler) setStatus(ctx context.Context, ar *storagev1alpha1.ZfsShareAttachRequest, ready bool, shareName, reason, message string) error {
	patched := ar.DeepCopy()
	patched.Status.Ready = ready
	patched.Status.ShareName = shareName
	patched.Status.ObservedGeneration = ar.Generation
	patched.Status.Message = message

	condStatus := metav1.ConditionFalse
	if ready {
		condStatus = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&patched.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ar.Generation,
	})

	return r.Status().Patch(ctx, patched, client.MergeFrom(ar))
}

// requestsForShare maps a ZfsShare event to reconcile requests for every attach
// request that references its volume, so pending requests re-check readiness when
// the aggregated share's status changes.
func (r *ZfsShareAttachRequestReconciler) requestsForShare(ctx context.Context, obj client.Object) []reconcile.Request {
	share, ok := obj.(*storagev1alpha1.ZfsShare)
	if !ok {
		return nil
	}
	var list storagev1alpha1.ZfsShareAttachRequestList
	if err := r.List(ctx, &list); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.VolumeName == share.Name {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

// requestsForPool maps a ZfsPool event to reconcile requests for every attach
// request whose volume lives on that pool, so a pool takeover (import onto a
// different node) re-evaluates whether each NVMe-oF attach is still local
// (ADR-0031). A local-only attach has no ZfsShare of its own for the usual
// Owns/Watches(ZfsShare) path to drive through, so this direct ZfsPool watch is
// the only way it learns the pool moved.
func (r *ZfsShareAttachRequestReconciler) requestsForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*storagev1alpha1.ZfsPool)
	if !ok {
		return nil
	}
	guid := pool.Status.GUID
	if guid == "" {
		guid = strings.TrimPrefix(pool.Name, "zpool-")
	}

	var dsList storagev1alpha1.ZfsDatasetList
	if err := r.List(ctx, &dsList); err != nil {
		return nil
	}
	volumes := make(map[string]bool, len(dsList.Items))
	for i := range dsList.Items {
		if dsList.Items[i].Spec.PoolGUID == guid {
			volumes[dsList.Items[i].Name] = true
		}
	}
	if len(volumes) == 0 {
		return nil
	}

	var arList storagev1alpha1.ZfsShareAttachRequestList
	if err := r.List(ctx, &arList); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for i := range arList.Items {
		if volumes[arList.Items[i].Spec.VolumeName] {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&arList.Items[i])})
		}
	}
	return reqs
}

// SetupWithManager wires the reconciler into the manager.
//
// MaxConcurrentReconciles is 1 to avoid write conflicts on the shared per-volume
// ZfsShare when several of a volume's attach requests reconcile together. It is a
// simplification, not a correctness requirement: single-node export safety comes
// from oldestAttachNode selecting the winner deterministically from the full
// attach-request list and nvmeExportSpec allowing exactly one host NQN, both of
// which are concurrency-safe on their own.
func (r *ZfsShareAttachRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsShareAttachRequest{}).
		Watches(&storagev1alpha1.ZfsShare{}, handler.EnqueueRequestsFromMapFunc(r.requestsForShare)).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.requestsForPool)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfsshareattachrequest").
		Complete(r)
}

// shareReadyForGeneration reports whether a share is exported for its current
// spec generation. The generation gate rejects a stale "Bound" from before an
// allow-list change, so a node never sees ready before its authorization is live.
func shareReadyForGeneration(s *storagev1alpha1.ZfsShare) bool {
	if s == nil {
		return false
	}
	return s.Status.Phase == storagev1alpha1.SharePhaseBound && s.Status.ObservedGeneration >= s.Generation
}

// protocolForDatasetType maps a ZFS dataset type to its sharing protocol.
func protocolForDatasetType(t storagev1alpha1.DatasetType) (storagev1alpha1.Protocol, error) {
	switch t {
	case storagev1alpha1.DatasetTypeFilesystem:
		return storagev1alpha1.ProtocolNFS, nil
	case storagev1alpha1.DatasetTypeVolume:
		return storagev1alpha1.ProtocolNVMeoF, nil
	default:
		return "", fmt.Errorf("unknown dataset type %q", t)
	}
}

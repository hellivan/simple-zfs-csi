package controller

import (
	"context"
	"fmt"
	"sort"
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
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsshareattachrequests,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsshareattachrequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsshares,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfsdatasets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

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
			if _, err := r.reconcileVolume(ctx, volume); err != nil {
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

	share, err := r.reconcileVolume(ctx, volume)
	if err != nil {
		// The backing ZfsDataset may not exist yet, or a node IP is not resolvable;
		// surface it and retry.
		return ctrl.Result{RequeueAfter: attachRequeue}, r.setStatus(ctx, &ar, false, volume, "Pending", err.Error())
	}

	ready := shareReadyForGeneration(share)
	reason, msg := "Exported", fmt.Sprintf("export live on the current generation for %q", volume)
	if !ready {
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
// none remain).
func (r *ZfsShareAttachRequestReconciler) reconcileVolume(ctx context.Context, volume string) (*storagev1alpha1.ZfsShare, error) {
	logger := log.FromContext(ctx)

	nodes, err := r.activeNodes(ctx, volume)
	if err != nil {
		return nil, err
	}

	shareKey := client.ObjectKey{Name: volume}

	// No consumers left: tear the share down (its NetworkExport is GC'd with it).
	if len(nodes) == 0 {
		share := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: volume}}
		if err := r.Delete(ctx, share); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, nil
	}

	var ds storagev1alpha1.ZfsDataset
	if err := r.Get(ctx, shareKey, &ds); err != nil {
		return nil, fmt.Errorf("get ZfsDataset %q: %w", volume, err)
	}
	protocol, err := protocolForDatasetType(ds.Spec.Type)
	if err != nil {
		return nil, err
	}

	var nfsClients []storagev1alpha1.NFSClient
	if protocol == storagev1alpha1.ProtocolNFS {
		nfsClients, err = r.nfsClientsForNodes(ctx, nodes)
		if err != nil {
			return nil, err
		}
	}

	share := &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: volume}}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, share, func() error {
		share.Spec.PoolGUID = ds.Spec.PoolGUID
		share.Spec.Dataset = ds.Spec.Dataset
		share.Spec.Protocol = protocol
		switch protocol {
		case storagev1alpha1.ProtocolNFS:
			share.Spec.NFS = &storagev1alpha1.NFSExportSpec{Clients: nfsClients}
			share.Spec.NVMeoF = nil
		case storagev1alpha1.ProtocolNVMeoF:
			// NVMe-oF is single-node (ADR-0010): the export exists only while the one
			// consumer is attached (temporal zero-trust). Host-NQN allow-listing is a
			// later refinement, so allowed hosts stay empty here.
			share.Spec.NVMeoF = &storagev1alpha1.NVMeoFExportSpec{}
			share.Spec.NFS = nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if op != controllerutil.OperationResultNone {
		logger.Info("aggregated ZfsShare", "op", op, "volume", volume, "nodes", nodes)
	}

	fresh := &storagev1alpha1.ZfsShare{}
	if err := r.Get(ctx, shareKey, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

// activeNodes returns the sorted, de-duplicated set of node names that currently
// hold a non-terminating attach request for the volume.
func (r *ZfsShareAttachRequestReconciler) activeNodes(ctx context.Context, volume string) ([]string, error) {
	var list storagev1alpha1.ZfsShareAttachRequestList
	if err := r.List(ctx, &list); err != nil {
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

// SetupWithManager wires the reconciler into the manager.
func (r *ZfsShareAttachRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsShareAttachRequest{}).
		Watches(&storagev1alpha1.ZfsShare{}, handler.EnqueueRequestsFromMapFunc(r.requestsForShare)).
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

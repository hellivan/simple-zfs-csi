// Package controller contains the reconcilers for the two NetworkExport backends.
// Each reconciler runs as its own binary/DaemonSet, acts only on exports pinned
// to its own node, and reconciles the full desired state on every event so the
// node configuration is always level-driven and self-healing.
package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// networkExportFinalizer guards a NetworkExport so its owning node's exporter
// (NFSReconciler or NVMeoFReconciler — whichever matches Spec.Protocol) can
// un-export it from the host before the object actually disappears.
//
// Without it, a plain owner-reference cascade delete (ZfsShare -> NetworkExport,
// ADR-0010) removes the object from etcd immediately and irrevocably. If the
// exporter pod for that node happens to be down at that exact moment, its next
// startup List will simply never see the object again — there is nothing left
// to generate the event that would tell it to drop the corresponding
// /etc/exports line or nvmet configfs subsystem, so the stale export could
// survive until some unrelated NetworkExport event happens to touch that node
// again, which may be never.
//
// The finalizer closes that: the API server keeps the object present (with
// DeletionTimestamp set) until it is removed below, so even across a restart
// the exporter's next List still returns it, triggers a reconcile, and
// listOwnedExports already excludes anything with a DeletionTimestamp from the
// desired render — so that reconcile correctly converges the host state to
// exclude it before the finalizer is removed and deletion actually proceeds.
const networkExportFinalizer = "storage.simple-zfs-csi.io/networkexport"

// nodeProtocolPredicate limits the reconcilers' work queue to exports pinned to
// this node and using the given protocol.
func nodeProtocolPredicate(nodeName string, protocol storagev1alpha1.Protocol) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		export, ok := obj.(*storagev1alpha1.NetworkExport)
		if !ok {
			return false
		}
		return export.Spec.NodeName == nodeName && export.Spec.Protocol == protocol
	})
}

// listOwnedExports returns the exports assigned to this node for the protocol,
// excluding those being deleted.
func listOwnedExports(ctx context.Context, c client.Client, nodeName string, protocol storagev1alpha1.Protocol) ([]storagev1alpha1.NetworkExport, error) {
	var list storagev1alpha1.NetworkExportList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]storagev1alpha1.NetworkExport, 0, len(list.Items))
	for i := range list.Items {
		s := list.Items[i]
		if s.Spec.NodeName != nodeName || s.Spec.Protocol != protocol {
			continue
		}
		if !s.DeletionTimestamp.IsZero() {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// updateStatus patches an export's status subresource with the given phase,
// message, condition and (for nvmeof) effective NQN.
func updateStatus(ctx context.Context, c client.Client, export *storagev1alpha1.NetworkExport, phase storagev1alpha1.NetworkExportPhase, reason, message, nqn string) error {
	patched := export.DeepCopy()
	patched.Status.Phase = phase
	patched.Status.ObservedGeneration = export.Generation
	patched.Status.Message = message
	if nqn != "" {
		patched.Status.NQN = nqn
	}

	condStatus := metav1.ConditionTrue
	condType := "Exported"
	if phase != storagev1alpha1.PhaseExported {
		condStatus = metav1.ConditionFalse
	}
	meta.SetStatusCondition(&patched.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: export.Generation,
	})

	return c.Status().Patch(ctx, patched, client.MergeFrom(export))
}

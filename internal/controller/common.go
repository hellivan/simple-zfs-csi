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

	storagev1alpha1 "github.com/hellivan/zfs-shares/api/v1alpha1"
)

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

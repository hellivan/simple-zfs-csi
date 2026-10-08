package controller

import (
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// poolChanged drops ZfsPool updates in which only status.lastUpdated moved. The
// discovery loop rewrites that timestamp every ~30s; without this filter each
// tick re-reconciles every object that watches the pool.
func poolChanged() builder.WatchesOption {
	return builder.WithPredicates(predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPool, ok1 := e.ObjectOld.(*storagev1alpha1.ZfsPool)
			newPool, ok2 := e.ObjectNew.(*storagev1alpha1.ZfsPool)
			if !ok1 || !ok2 {
				return true
			}
			return !samePoolIgnoringHeartbeat(oldPool, newPool)
		},
	})
}

func samePoolIgnoringHeartbeat(a, b *storagev1alpha1.ZfsPool) bool {
	if a.DeletionTimestamp.IsZero() != b.DeletionTimestamp.IsZero() ||
		!equality.Semantic.DeepEqual(a.Spec, b.Spec) ||
		!equality.Semantic.DeepEqual(a.Labels, b.Labels) {
		return false
	}
	sa, sb := a.Status.DeepCopy(), b.Status.DeepCopy()
	sa.LastUpdated, sb.LastUpdated = nil, nil
	return equality.Semantic.DeepEqual(sa, sb)
}

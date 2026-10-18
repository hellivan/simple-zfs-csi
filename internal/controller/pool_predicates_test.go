package controller

import (
	"context"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

func TestSamePoolIgnoringHeartbeat(t *testing.T) {
	a := onlinePool()
	b := a.DeepCopy()
	now := metav1.NewTime(time.Now())
	b.Status.LastUpdated = &now
	if !samePoolIgnoringHeartbeat(a, b) {
		t.Fatal("lastUpdated-only change must be ignored")
	}
	b.Status.CurrentNode = "node-b"
	if samePoolIgnoringHeartbeat(a, b) {
		t.Fatal("node change must pass")
	}
	c := a.DeepCopy()
	c.Status.Health = storagev1alpha1.PoolHealthNodeOffline
	if samePoolIgnoringHeartbeat(a, c) {
		t.Fatal("health change must pass")
	}
}

func TestDeleteShareIfPresent_NoDeleteCallWhenAbsent(t *testing.T) {
	deletes := 0
	c := fake.NewClientBuilder().WithScheme(newTestScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			deletes++
			return cl.Delete(ctx, obj, opts...)
		}}).Build()
	r := &ZfsShareAttachRequestReconciler{Client: c}
	if err := r.deleteShareIfPresent(context.Background(), "v"); err != nil || deletes != 0 {
		t.Fatalf("err=%v deletes=%d", err, deletes)
	}
	_ = c.Create(context.Background(), &storagev1alpha1.ZfsShare{ObjectMeta: metav1.ObjectMeta{Name: "v"}})
	if err := r.deleteShareIfPresent(context.Background(), "v"); err != nil || deletes != 1 {
		t.Fatalf("err=%v deletes=%d", err, deletes)
	}
}

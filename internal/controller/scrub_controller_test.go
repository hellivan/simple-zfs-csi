package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

const scrubTestConfig = `
enabled: true
defaultSchedule: "0 3 * * 0"
pools:
  - guid: "123"
    schedule: "0 4 * * 6"
template:
  spec:
    schedule: "PLACEHOLDER"
    jobTemplate:
      spec:
        template:
          spec:
            containers:
              - name: scrub
                image: discovery:latest
                args: ["--zpool-bin=zpool"]
`

func scrubConfigMap(data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scrub", Namespace: "sys"},
		Data:       map[string]string{"config.yaml": data},
	}
}

func onlinePoolObj(guid, node string, health storagev1alpha1.ZfsPoolHealth) *storagev1alpha1.ZfsPool {
	return &storagev1alpha1.ZfsPool{
		ObjectMeta: metav1.ObjectMeta{Name: "zpool-" + guid},
		Status:     storagev1alpha1.ZfsPoolStatus{GUID: guid, CurrentNode: node, Health: health},
	}
}

func reconcileScrub(t *testing.T, r *ScrubReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "sys", Name: "scrub"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func TestScrubReconcile_PinsAndPrunes(t *testing.T) {
	scheme := newAttachScheme(t)
	cm := scrubConfigMap(scrubTestConfig)
	pool := onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthOnline)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, pool).Build()
	r := &ScrubReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}

	reconcileScrub(t, r)

	cj := &batchv1.CronJob{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "scrub-123"}, cj); err != nil {
		t.Fatalf("expected CronJob scrub-123: %v", err)
	}
	if cj.Spec.Schedule != "0 4 * * 6" {
		t.Errorf("schedule = %q, want per-pool override", cj.Spec.Schedule)
	}
	if cj.Spec.Suspend == nil || *cj.Spec.Suspend {
		t.Errorf("online pool should not be suspended")
	}
	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	aff := pod.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("expected node affinity")
	}
	req := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0]
	if req.Key != "kubernetes.io/hostname" || len(req.Values) != 1 || req.Values[0] != "node-a" {
		t.Errorf("affinity = %+v, want hostname node-a", req)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Args[len(pod.Containers[0].Args)-1] != "--pool-guid=123" {
		t.Errorf("container args = %+v, want trailing --pool-guid=123", pod.Containers[0].Args)
	}
	if cj.Labels[scrubPoolGUIDLabel] != "123" {
		t.Errorf("missing pool-guid label: %+v", cj.Labels)
	}

	// Reconfigure to a different pool -> the old CronJob is pruned.
	cm2 := scrubConfigMap(`
enabled: true
defaultSchedule: "0 3 * * 0"
pools:
  - guid: "999"
template:
  spec:
    schedule: "x"
    jobTemplate:
      spec:
        template:
          spec:
            containers:
              - name: scrub
                image: discovery:latest
`)
	if err := c.Update(context.Background(), cm2); err != nil {
		t.Fatalf("update cm: %v", err)
	}
	reconcileScrub(t, r)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "scrub-123"}, &batchv1.CronJob{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected scrub-123 pruned, got err=%v", err)
	}
}

func TestScrubReconcile_SuspendWhenOffline(t *testing.T) {
	scheme := newAttachScheme(t)
	cm := scrubConfigMap(scrubTestConfig)
	pool := onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthNodeOffline)

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm, pool).Build()
	r := &ScrubReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	reconcileScrub(t, r)

	cj := &batchv1.CronJob{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "scrub-123"}, cj); err != nil {
		t.Fatalf("get CronJob: %v", err)
	}
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Errorf("offline pool should be suspended")
	}
}

func TestScrubReconcile_DisabledDeletesAll(t *testing.T) {
	scheme := newAttachScheme(t)
	pool := onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthOnline)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scrubConfigMap(scrubTestConfig), pool).Build()
	r := &ScrubReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	reconcileScrub(t, r)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "scrub-123"}, &batchv1.CronJob{}); err != nil {
		t.Fatalf("expected CronJob before disable: %v", err)
	}

	if err := c.Update(context.Background(), scrubConfigMap("enabled: false\n")); err != nil {
		t.Fatalf("update cm: %v", err)
	}
	reconcileScrub(t, r)
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "scrub-123"}, &batchv1.CronJob{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected all scrub CronJobs deleted when disabled, got err=%v", err)
	}
}

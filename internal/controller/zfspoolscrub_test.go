package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

const scrubTestTemplate = `
template:
  spec:
    schedule: "0 3 * * 0"
    startingDeadlineSeconds: 100
    jobTemplate:
      spec:
        backoffLimit: 0
        activeDeadlineSeconds: 86400
        template:
          spec:
            containers:
              - name: scrub
                image: discovery:latest
                args: ["--zpool-bin=zpool", "--timeout=24h"]
`

func scrubTemplateCM(data string) *corev1.ConfigMap {
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

func scrubReq(name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "sys", Name: name}}
}

func newScrubClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(newAttachScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&storagev1alpha1.ZfsPoolScrubJob{}, &storagev1alpha1.ZfsPoolScrubCronJob{}).
		Build()
}

func podArgs(t *testing.T, spec batchv1.JobSpec) []string {
	t.Helper()
	if len(spec.Template.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(spec.Template.Spec.Containers))
	}
	return spec.Template.Spec.Containers[0].Args
}

func pinnedNode(spec batchv1.JobSpec) string {
	aff := spec.Template.Spec.Affinity
	if aff == nil || aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	return aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Values[0]
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// --- ZfsPoolScrubJob ---

func newScrubJobCR(spec storagev1alpha1.ZfsPoolScrubJobSpec) *storagev1alpha1.ZfsPoolScrubJob {
	return &storagev1alpha1.ZfsPoolScrubJob{
		ObjectMeta: metav1.ObjectMeta{Name: "run1", Namespace: "sys", UID: "uid-1"},
		Spec:       spec,
	}
}

func TestScrubJob_CreatesPinnedJobAndSucceeds(t *testing.T) {
	timeout := metav1.Duration{Duration: 2 * time.Hour}
	deadline := int64(9000)
	ttl := int32(600)
	cr := newScrubJobCR(storagev1alpha1.ZfsPoolScrubJobSpec{
		PoolGUID: "123", Timeout: &timeout, ActiveDeadlineSeconds: &deadline, TTLSecondsAfterFinished: &ttl,
	})
	c := newScrubClient(t, scrubTemplateCM(scrubTestTemplate), onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthOnline), cr)
	r := &ZfsPoolScrubJobReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	ctx := context.Background()

	if _, err := r.Reconcile(ctx, scrubReq("run1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "sys", Name: "run1"}, job); err != nil {
		t.Fatalf("expected Job: %v", err)
	}
	if got := pinnedNode(job.Spec); got != "node-a" {
		t.Errorf("pinned node = %q, want node-a", got)
	}
	args := podArgs(t, job.Spec)
	for _, want := range []string{"--pool-guid=123", "--timeout=2h0m0s", "--zpool-bin=zpool"} {
		if !hasArg(args, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
	if hasArg(args, "--timeout=24h") {
		t.Errorf("template timeout should be replaced: %v", args)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 9000 {
		t.Errorf("activeDeadlineSeconds = %v, want 9000", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 {
		t.Errorf("ttl = %v, want 600", job.Spec.TTLSecondsAfterFinished)
	}
	if !metav1.IsControlledBy(job, cr) {
		t.Errorf("Job should be controlled by the CR")
	}

	got := &storagev1alpha1.ZfsPoolScrubJob{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(cr), got)
	if got.Status.Phase != storagev1alpha1.ScrubPhaseRunning || got.Status.JobName != "run1" {
		t.Fatalf("status = %+v, want Running with jobName", got.Status)
	}

	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatalf("update job status: %v", err)
	}
	if _, err := r.Reconcile(ctx, scrubReq("run1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(cr), got)
	if got.Status.Phase != storagev1alpha1.ScrubPhaseSucceeded || got.Status.CompletionTime == nil {
		t.Errorf("status = %+v, want Succeeded with completionTime", got.Status)
	}
}

func TestScrubJob_FailureCopiesPodMessage(t *testing.T) {
	cr := newScrubJobCR(storagev1alpha1.ZfsPoolScrubJobSpec{PoolGUID: "123"})
	c := newScrubClient(t, scrubTemplateCM(scrubTestTemplate), onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthOnline), cr)
	r := &ZfsPoolScrubJobReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, scrubReq("run1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	job := &batchv1.Job{}
	_ = c.Get(ctx, client.ObjectKey{Namespace: "sys", Name: "run1"}, job)
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatalf("update job status: %v", err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "run1-abc", Namespace: "sys", Labels: map[string]string{"job-name": "run1"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "scrub",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: "cannot scrub tank: currently scrubbing",
			}},
		}}},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	if _, err := r.Reconcile(ctx, scrubReq("run1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &storagev1alpha1.ZfsPoolScrubJob{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(cr), got)
	if got.Status.Phase != storagev1alpha1.ScrubPhaseFailed || !strings.Contains(got.Status.Message, "currently scrubbing") {
		t.Errorf("status = %+v, want Failed with pod message", got.Status)
	}
}

func TestScrubJob_FailsWhenPoolUnavailable(t *testing.T) {
	for name, pool := range map[string]*storagev1alpha1.ZfsPool{
		"offline": onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthNodeOffline),
		"missing": nil,
	} {
		t.Run(name, func(t *testing.T) {
			cr := newScrubJobCR(storagev1alpha1.ZfsPoolScrubJobSpec{PoolGUID: "123"})
			objs := []client.Object{scrubTemplateCM(scrubTestTemplate), cr}
			if pool != nil {
				objs = append(objs, pool)
			}
			c := newScrubClient(t, objs...)
			r := &ZfsPoolScrubJobReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
			if _, err := r.Reconcile(context.Background(), scrubReq("run1")); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := &storagev1alpha1.ZfsPoolScrubJob{}
			_ = c.Get(context.Background(), client.ObjectKeyFromObject(cr), got)
			if got.Status.Phase != storagev1alpha1.ScrubPhaseFailed {
				t.Errorf("phase = %q, want Failed", got.Status.Phase)
			}
			if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "run1"}, &batchv1.Job{}); err == nil {
				t.Errorf("no Job should be created for an unavailable pool")
			}
		})
	}
}

func TestScrubJob_PendingWithoutTemplate(t *testing.T) {
	cr := newScrubJobCR(storagev1alpha1.ZfsPoolScrubJobSpec{PoolGUID: "123"})
	c := newScrubClient(t, onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthOnline), cr)
	r := &ZfsPoolScrubJobReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	res, err := r.Reconcile(context.Background(), scrubReq("run1"))
	if err != nil || res.RequeueAfter == 0 {
		t.Fatalf("res=%+v err=%v, want requeue", res, err)
	}
	got := &storagev1alpha1.ZfsPoolScrubJob{}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cr), got)
	if got.Status.Phase != storagev1alpha1.ScrubPhasePending {
		t.Errorf("phase = %q, want Pending", got.Status.Phase)
	}
}

func TestScrubJob_TerminalIsNotRerun(t *testing.T) {
	cr := newScrubJobCR(storagev1alpha1.ZfsPoolScrubJobSpec{PoolGUID: "123"})
	cr.Status.Phase = storagev1alpha1.ScrubPhaseSucceeded
	c := newScrubClient(t, scrubTemplateCM(scrubTestTemplate), onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthOnline), cr)
	r := &ZfsPoolScrubJobReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	if _, err := r.Reconcile(context.Background(), scrubReq("run1")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "run1"}, &batchv1.Job{}); err == nil {
		t.Errorf("a finished run must not create a new Job (e.g. after TTL deletion)")
	}
}

// --- ZfsPoolScrubCronJob ---

func newScrubCronCR(spec storagev1alpha1.ZfsPoolScrubCronJobSpec) *storagev1alpha1.ZfsPoolScrubCronJob {
	return &storagev1alpha1.ZfsPoolScrubCronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "scrub-123", Namespace: "sys", UID: "uid-2"},
		Spec:       spec,
	}
}

func TestScrubCronJob_PinsRepinsAndSuspends(t *testing.T) {
	startDL := int64(7200)
	cr := newScrubCronCR(storagev1alpha1.ZfsPoolScrubCronJobSpec{PoolGUID: "123", Schedule: "0 4 * * 6", StartingDeadlineSeconds: &startDL})
	pool := onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthOnline)
	c := newScrubClient(t, scrubTemplateCM(scrubTestTemplate), pool, cr)
	r := &ZfsPoolScrubCronJobReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	ctx := context.Background()

	reconcile := func() *batchv1.CronJob {
		t.Helper()
		if _, err := r.Reconcile(ctx, scrubReq("scrub-123")); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		cj := &batchv1.CronJob{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: "sys", Name: "scrub-123"}, cj); err != nil {
			t.Fatalf("expected CronJob: %v", err)
		}
		return cj
	}

	cj := reconcile()
	if cj.Spec.Schedule != "0 4 * * 6" {
		t.Errorf("schedule = %q", cj.Spec.Schedule)
	}
	if cj.Spec.StartingDeadlineSeconds == nil || *cj.Spec.StartingDeadlineSeconds != 7200 {
		t.Errorf("startingDeadlineSeconds = %v, want CR override 7200", cj.Spec.StartingDeadlineSeconds)
	}
	if cj.Spec.Suspend == nil || *cj.Spec.Suspend {
		t.Errorf("online pool must not be suspended")
	}
	if got := pinnedNode(cj.Spec.JobTemplate.Spec); got != "node-a" {
		t.Errorf("pinned node = %q, want node-a", got)
	}
	if args := podArgs(t, cj.Spec.JobTemplate.Spec); !hasArg(args, "--pool-guid=123") {
		t.Errorf("args %v missing --pool-guid=123", args)
	}
	if cj.Labels[scrubPoolGUIDLabel] != "123" || !metav1.IsControlledBy(cj, cr) {
		t.Errorf("labels/owner wrong: %+v", cj.ObjectMeta)
	}

	// Takeover: pool moves to node-b.
	pool.Status.CurrentNode = "node-b"
	if err := c.Update(ctx, pool); err != nil {
		t.Fatalf("update pool: %v", err)
	}
	if got := pinnedNode(reconcile().Spec.JobTemplate.Spec); got != "node-b" {
		t.Errorf("after takeover pinned node = %q, want node-b", got)
	}
	// Reconcile must not accumulate args.
	if args := podArgs(t, reconcile().Spec.JobTemplate.Spec); countPrefix(args, "--pool-guid=") != 1 {
		t.Errorf("duplicate --pool-guid in %v", args)
	}

	// Offline: suspended.
	pool.Status.Health = storagev1alpha1.PoolHealthNodeOffline
	if err := c.Update(ctx, pool); err != nil {
		t.Fatalf("update pool: %v", err)
	}
	if cj := reconcile(); cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Errorf("offline pool must be suspended")
	}
	got := &storagev1alpha1.ZfsPoolScrubCronJob{}
	_ = c.Get(ctx, client.ObjectKeyFromObject(cr), got)
	if !got.Status.Suspended || !strings.Contains(got.Status.Message, "offline") {
		t.Errorf("status = %+v, want suspended with reason", got.Status)
	}
}

func TestScrubCronJob_SpecSuspend(t *testing.T) {
	cr := newScrubCronCR(storagev1alpha1.ZfsPoolScrubCronJobSpec{PoolGUID: "123", Schedule: "0 4 * * 6", Suspend: boolPtr(true)})
	c := newScrubClient(t, scrubTemplateCM(scrubTestTemplate), onlinePoolObj("123", "node-a", storagev1alpha1.PoolHealthOnline), cr)
	r := &ZfsPoolScrubCronJobReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	if _, err := r.Reconcile(context.Background(), scrubReq("scrub-123")); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	cj := &batchv1.CronJob{}
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: "sys", Name: "scrub-123"}, cj)
	if cj.Spec.Suspend == nil || !*cj.Spec.Suspend {
		t.Errorf("spec.suspend must suspend the CronJob")
	}
}

func TestScrubCronJob_RequestsForPool(t *testing.T) {
	a := newScrubCronCR(storagev1alpha1.ZfsPoolScrubCronJobSpec{PoolGUID: "123", Schedule: "x"})
	b := newScrubCronCR(storagev1alpha1.ZfsPoolScrubCronJobSpec{PoolGUID: "999", Schedule: "x"})
	b.Name = "other"
	c := newScrubClient(t, a, b)
	r := &ZfsPoolScrubCronJobReconciler{Client: c, Namespace: "sys", ConfigMapName: "scrub"}
	reqs := r.requestsForPool(context.Background(), onlinePoolObj("123", "n", storagev1alpha1.PoolHealthOnline))
	if len(reqs) != 1 || reqs[0].Name != "scrub-123" {
		t.Errorf("requests = %+v, want only scrub-123", reqs)
	}
}

func countPrefix(args []string, prefix string) int {
	n := 0
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			n++
		}
	}
	return n
}

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// scrubTemplateRetry is how long a reconciler waits before retrying while the
// template ConfigMap is missing.
const scrubTemplateRetry = 30 * time.Second

// maxScrubMessage bounds the failure text copied into a status message.
const maxScrubMessage = 1024

// ZfsPoolScrubJobReconciler runs one scrub per ZfsPoolScrubJob as a Kubernetes
// Job pinned to the pool's node, and mirrors the Job's outcome into the CR's
// status (design-decisions ADR-0044). A run is never retried: Succeeded and
// Failed are terminal.
type ZfsPoolScrubJobReconciler struct {
	client.Client
	// APIReader reads Pods uncached (they are only needed to fetch a failed
	// run's output). Falls back to Client when nil.
	APIReader client.Reader
	// Namespace is the release namespace where the template ConfigMap, the CRs
	// and the Jobs live.
	Namespace string
	// ConfigMapName is the scrub template ConfigMap.
	ConfigMapName string
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspoolscrubjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspoolscrubjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspoolscrubjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile advances one scrub run.
func (r *ZfsPoolScrubJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr storagev1alpha1.ZfsPoolScrubJob
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if cr.Status.Phase == storagev1alpha1.ScrubPhaseSucceeded || cr.Status.Phase == storagev1alpha1.ScrubPhaseFailed {
		return ctrl.Result{}, nil
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: cr.Namespace, Name: cr.Name}, job)
	switch {
	case apierrors.IsNotFound(err):
		if cr.Status.Phase == storagev1alpha1.ScrubPhaseRunning {
			return ctrl.Result{}, r.finish(ctx, &cr, storagev1alpha1.ScrubPhaseFailed, "the scrub Job disappeared before it finished")
		}
		return r.start(ctx, &cr)
	case err != nil:
		return ctrl.Result{}, err
	}
	if !metav1.IsControlledBy(job, &cr) {
		return ctrl.Result{}, r.finish(ctx, &cr, storagev1alpha1.ScrubPhaseFailed,
			fmt.Sprintf("a Job named %q already exists and is not owned by this scrub", job.Name))
	}
	return ctrl.Result{}, r.syncFromJob(ctx, &cr, job)
}

// start creates the Job, or fails the run when the pool cannot be scrubbed now.
func (r *ZfsPoolScrubJobReconciler) start(ctx context.Context, cr *storagev1alpha1.ZfsPoolScrubJob) (ctrl.Result, error) {
	tmpl, err := loadScrubTemplate(ctx, r.Client, r.Namespace, r.ConfigMapName)
	if err != nil {
		if errors.Is(err, errScrubTemplateMissing) {
			return ctrl.Result{RequeueAfter: scrubTemplateRetry}, r.setPending(ctx, cr, err.Error())
		}
		return ctrl.Result{}, err
	}

	pools, err := poolsByGUID(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	node, unavailable := scrubPoolHost(pools[cr.Spec.PoolGUID])
	if unavailable != "" {
		return ctrl.Result{}, r.finish(ctx, cr, storagev1alpha1.ScrubPhaseFailed,
			fmt.Sprintf("cannot scrub pool %s: %s", cr.Spec.PoolGUID, unavailable))
	}

	ov := scrubJobOverrides{
		activeDeadlineSeconds:   cr.Spec.ActiveDeadlineSeconds,
		ttlSecondsAfterFinished: cr.Spec.TTLSecondsAfterFinished,
	}
	if cr.Spec.Timeout != nil {
		ov.timeout = cr.Spec.Timeout.Duration.String()
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name,
			Namespace: cr.Namespace,
			Labels: map[string]string{
				scrubComponentLabel: scrubComponentValue,
				scrubPoolGUIDLabel:  cr.Spec.PoolGUID,
			},
		},
		Spec: buildScrubJobSpec(tmpl, cr.Spec.PoolGUID, node, ov),
	}
	if err := controllerutil.SetControllerReference(cr, job, r.Scheme()); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, r.finish(ctx, cr, storagev1alpha1.ScrubPhaseFailed, fmt.Sprintf("create scrub Job: %v", err))
	}
	log.FromContext(ctx).Info("started scrub", "pool", cr.Spec.PoolGUID, "node", node, "job", job.Name)
	cr.Status.JobName = job.Name
	return ctrl.Result{}, r.setPhase(ctx, cr, storagev1alpha1.ScrubPhaseRunning, "scrub started on node "+node)
}

// syncFromJob mirrors the Job's state into the CR.
func (r *ZfsPoolScrubJobReconciler) syncFromJob(ctx context.Context, cr *storagev1alpha1.ZfsPoolScrubJob, job *batchv1.Job) error {
	cr.Status.JobName = job.Name
	cr.Status.StartTime = job.Status.StartTime
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return r.finish(ctx, cr, storagev1alpha1.ScrubPhaseSucceeded, "scrub completed cleanly")
		case batchv1.JobFailed:
			msg := r.failureMessage(ctx, job)
			if msg == "" {
				msg = fmt.Sprintf("%s: %s", c.Reason, c.Message)
			}
			return r.finish(ctx, cr, storagev1alpha1.ScrubPhaseFailed, msg)
		}
	}
	return r.setPhase(ctx, cr, storagev1alpha1.ScrubPhaseRunning, cr.Status.Message)
}

// failureMessage returns the terminated container's message (the tail of the
// scrub output, via FallbackToLogsOnError) from the Job's pods.
func (r *ZfsPoolScrubJobReconciler) failureMessage(ctx context.Context, job *batchv1.Job) string {
	var reader client.Reader = r.Client
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{"job-name": job.Name}); err != nil {
		return ""
	}
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
				msg := strings.TrimSpace(t.Message)
				if msg == "" {
					msg = fmt.Sprintf("scrub container exited with code %d", t.ExitCode)
				}
				return truncateScrubMessage(msg)
			}
		}
	}
	return ""
}

func truncateScrubMessage(s string) string {
	if len(s) <= maxScrubMessage {
		return s
	}
	return "…" + s[len(s)-maxScrubMessage:]
}

func (r *ZfsPoolScrubJobReconciler) setPending(ctx context.Context, cr *storagev1alpha1.ZfsPoolScrubJob, msg string) error {
	return r.setPhase(ctx, cr, storagev1alpha1.ScrubPhasePending, msg)
}

func (r *ZfsPoolScrubJobReconciler) setPhase(ctx context.Context, cr *storagev1alpha1.ZfsPoolScrubJob, phase storagev1alpha1.ZfsPoolScrubPhase, msg string) error {
	if cr.Status.Phase == phase && cr.Status.Message == msg {
		return nil
	}
	cr.Status.Phase = phase
	cr.Status.Message = truncateScrubMessage(msg)
	return r.Status().Update(ctx, cr)
}

// finish records a terminal phase and the completion time.
func (r *ZfsPoolScrubJobReconciler) finish(ctx context.Context, cr *storagev1alpha1.ZfsPoolScrubJob, phase storagev1alpha1.ZfsPoolScrubPhase, msg string) error {
	now := metav1.Now()
	cr.Status.CompletionTime = &now
	cr.Status.Phase = phase
	cr.Status.Message = truncateScrubMessage(msg)
	return r.Status().Update(ctx, cr)
}

// SetupWithManager reconciles on ZfsPoolScrubJobs, their Jobs, and the template
// ConfigMap (to retry runs that were waiting for it).
func (r *ZfsPoolScrubJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configMapPred := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.Namespace && obj.GetName() == r.ConfigMapName
	})
	pendingOnConfig := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list storagev1alpha1.ZfsPoolScrubJobList
		if err := r.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			if list.Items[i].Status.Phase == storagev1alpha1.ScrubPhasePending || list.Items[i].Status.Phase == "" {
				out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
			}
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsPoolScrubJob{}).
		Owns(&batchv1.Job{}).
		Watches(&corev1.ConfigMap{}, pendingOnConfig, builder.WithPredicates(configMapPred)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfspoolscrubjob").
		Complete(r)
}

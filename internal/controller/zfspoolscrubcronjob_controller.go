package controller

import (
	"context"
	"errors"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// ZfsPoolScrubCronJobReconciler maintains one Kubernetes CronJob per
// ZfsPoolScrubCronJob (design-decisions ADR-0044). The CronJob carries the CR's
// name, is pinned by node affinity to the pool's current host, re-pinned when
// the pool moves, and suspended while the pool has no reachable node. It is
// garbage-collected with the CR.
type ZfsPoolScrubCronJobReconciler struct {
	client.Client
	// Namespace is the release namespace where the template ConfigMap, the CRs
	// and the CronJobs live.
	Namespace string
	// ConfigMapName is the scrub template ConfigMap.
	ConfigMapName string
}

// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspoolscrubcronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspoolscrubcronjobs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspoolscrubcronjobs/finalizers,verbs=update
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch

// Reconcile converges the CronJob for one ZfsPoolScrubCronJob.
func (r *ZfsPoolScrubCronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr storagev1alpha1.ZfsPoolScrubCronJob
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	tmpl, err := loadScrubTemplate(ctx, r.Client, r.Namespace, r.ConfigMapName)
	if err != nil {
		if errors.Is(err, errScrubTemplateMissing) {
			cr.Status.Message = err.Error()
			if uerr := r.Status().Update(ctx, &cr); uerr != nil {
				return ctrl.Result{}, uerr
			}
			return ctrl.Result{RequeueAfter: scrubTemplateRetry}, nil
		}
		return ctrl.Result{}, err
	}

	pools, err := poolsByGUID(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	node, unavailable := scrubPoolHost(pools[cr.Spec.PoolGUID])

	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: cr.Name, Namespace: cr.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		cj.Spec = *tmpl.Spec.DeepCopy()
		cj.Labels = map[string]string{}
		for k, v := range tmpl.Labels {
			cj.Labels[k] = v
		}
		cj.Labels[scrubComponentLabel] = scrubComponentValue
		cj.Labels[scrubPoolGUIDLabel] = cr.Spec.PoolGUID

		cj.Spec.Schedule = cr.Spec.Schedule
		if v := cr.Spec.StartingDeadlineSeconds; v != nil {
			cj.Spec.StartingDeadlineSeconds = v
		}
		if v := cr.Spec.SuccessfulJobsHistoryLimit; v != nil {
			cj.Spec.SuccessfulJobsHistoryLimit = v
		}
		if v := cr.Spec.FailedJobsHistoryLimit; v != nil {
			cj.Spec.FailedJobsHistoryLimit = v
		}

		// With no reachable host the pod is left unpinned and the CronJob is
		// suspended.
		ov := scrubJobOverrides{activeDeadlineSeconds: cr.Spec.ActiveDeadlineSeconds}
		if cr.Spec.Timeout != nil {
			ov.timeout = cr.Spec.Timeout.Duration.String()
		}
		cj.Spec.JobTemplate.Spec = buildScrubJobSpec(tmpl, cr.Spec.PoolGUID, node, ov)
		cj.Spec.Suspend = boolPtr(unavailable != "" || (cr.Spec.Suspend != nil && *cr.Spec.Suspend))
		return controllerutil.SetControllerReference(&cr, cj, r.Scheme())
	}); err != nil {
		return ctrl.Result{}, err
	}

	cr.Status.CronJobName = cj.Name
	cr.Status.Suspended = cj.Spec.Suspend != nil && *cj.Spec.Suspend
	cr.Status.CurrentNode = node
	cr.Status.LastScheduleTime = cj.Status.LastScheduleTime
	cr.Status.Message = ""
	if unavailable != "" {
		cr.Status.Message = "suspended: " + unavailable
	}
	return ctrl.Result{}, r.Status().Update(ctx, &cr)
}

// requestsForPool maps a ZfsPool event to the CRs targeting its GUID.
func (r *ZfsPoolScrubCronJobReconciler) requestsForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*storagev1alpha1.ZfsPool)
	if !ok {
		return nil
	}
	guid := zfsPoolGUID(pool)
	var list storagev1alpha1.ZfsPoolScrubCronJobList
	if err := r.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
		return nil
	}
	var out []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.PoolGUID == guid {
			out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}

// requestsForAll maps a template ConfigMap event to every CR.
func (r *ZfsPoolScrubCronJobReconciler) requestsForAll(ctx context.Context, _ client.Object) []reconcile.Request {
	var list storagev1alpha1.ZfsPoolScrubCronJobList
	if err := r.List(ctx, &list, client.InNamespace(r.Namespace)); err != nil {
		return nil
	}
	out := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
	}
	return out
}

// SetupWithManager reconciles on the CRs, their CronJobs (self-heal), any
// ZfsPool (takeover/health) and the template ConfigMap.
func (r *ZfsPoolScrubCronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configMapPred := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.Namespace && obj.GetName() == r.ConfigMapName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&storagev1alpha1.ZfsPoolScrubCronJob{}).
		Owns(&batchv1.CronJob{}).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.requestsForPool)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.requestsForAll), builder.WithPredicates(configMapPred)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("zfspoolscrubcronjob").
		Complete(r)
}

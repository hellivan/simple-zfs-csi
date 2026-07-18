package controller

import (
	"context"
	"fmt"
	"strings"

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
	"sigs.k8s.io/yaml"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

const (
	// scrubComponentLabel marks the CronJobs this reconciler owns.
	scrubComponentLabel = "app.kubernetes.io/component"
	scrubComponentValue = "scrub"
	// scrubPoolGUIDLabel records which pool GUID a scrub CronJob targets.
	scrubPoolGUIDLabel = "maintenance.simple-zfs-csi.io/pool-guid"
	// scrubConfigKey is the ConfigMap data key holding the scrub config YAML.
	scrubConfigKey = "config.yaml"
)

// ScrubConfig is the operator's scrub configuration, rendered by Helm into a
// ConfigMap. Template is a base CronJob (image, host-exec, volumes, SA); the
// reconciler clones it per pool and sets the name, schedule, node affinity and
// the --pool-guid argument.
type ScrubConfig struct {
	Enabled         bool              `json:"enabled"`
	DefaultSchedule string            `json:"defaultSchedule"`
	Pools           []ScrubPoolConfig `json:"pools"`
	Template        *batchv1.CronJob  `json:"template"`
}

// ScrubPoolConfig is one pool's scrub schedule.
type ScrubPoolConfig struct {
	GUID     string `json:"guid"`
	Schedule string `json:"schedule,omitempty"`
}

// ScrubReconciler reconciles per-pool scrub CronJobs from the scrub ConfigMap and
// the live ZfsPool set (design-decisions ADR-0012). It runs in the operator
// (leader-elected): for each configured pool it ensures a CronJob pinned via node
// affinity to the pool's current host, re-targets on takeover, suspends the
// CronJob when the pool is offline, and prunes CronJobs for pools removed from
// the config.
type ScrubReconciler struct {
	client.Client
	// Namespace is the release namespace where the ConfigMap and CronJobs live.
	Namespace string
	// ConfigMapName is the scrub ConfigMap the reconciler reads.
	ConfigMapName string
}

// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=storage.simple-zfs-csi.io,resources=zfspools,verbs=get;list;watch

// Reconcile rebuilds the full set of scrub CronJobs from the config + ZfsPools.
func (r *ScrubReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var managed batchv1.CronJobList
	if err := r.List(ctx, &managed, client.InNamespace(r.Namespace),
		client.MatchingLabels{scrubComponentLabel: scrubComponentValue}); err != nil {
		return ctrl.Result{}, err
	}

	cfg, err := r.loadConfig(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cfg == nil || !cfg.Enabled || cfg.Template == nil {
		// Scrubbing disabled or unconfigured: remove everything we own.
		return ctrl.Result{}, r.deleteAll(ctx, managed.Items)
	}

	pools, err := r.poolsByGUID(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	want := make(map[string]struct{}, len(cfg.Pools))
	for _, pc := range cfg.Pools {
		if pc.GUID == "" {
			continue
		}
		want[pc.GUID] = struct{}{}
		schedule := pc.Schedule
		if schedule == "" {
			schedule = cfg.DefaultSchedule
		}
		if err := r.ensureCronJob(ctx, cfg.Template, pc.GUID, schedule, pools[pc.GUID]); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Prune CronJobs for pools no longer configured.
	for i := range managed.Items {
		cj := &managed.Items[i]
		if _, ok := want[cj.Labels[scrubPoolGUIDLabel]]; ok {
			continue
		}
		if err := r.Delete(ctx, cj); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		logger.Info("pruned scrub CronJob", "cronjob", cj.Name)
	}
	return ctrl.Result{}, nil
}

// loadConfig reads and parses the scrub ConfigMap, returning nil when it is absent.
func (r *ScrubReconciler) loadConfig(ctx context.Context) (*ScrubConfig, error) {
	if r.ConfigMapName == "" {
		return nil, nil
	}
	var cm corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: r.ConfigMapName}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	raw := cm.Data[scrubConfigKey]
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var cfg ScrubConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parse scrub config: %w", err)
	}
	return &cfg, nil
}

// poolsByGUID indexes the live ZfsPools by GUID.
func (r *ScrubReconciler) poolsByGUID(ctx context.Context) (map[string]*storagev1alpha1.ZfsPool, error) {
	var list storagev1alpha1.ZfsPoolList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make(map[string]*storagev1alpha1.ZfsPool, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		guid := p.Status.GUID
		if guid == "" {
			guid = strings.TrimPrefix(p.Name, "zpool-")
		}
		out[guid] = p
	}
	return out, nil
}

// ensureCronJob clones the template into a per-pool CronJob, pinning it to the
// pool's current node (or suspending it when the pool is unavailable).
func (r *ScrubReconciler) ensureCronJob(ctx context.Context, tmpl *batchv1.CronJob, guid, schedule string, pool *storagev1alpha1.ZfsPool) error {
	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "scrub-" + guid, Namespace: r.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		cj.Spec = *tmpl.Spec.DeepCopy()
		if cj.Labels == nil {
			cj.Labels = map[string]string{}
		}
		for k, v := range tmpl.Labels {
			cj.Labels[k] = v
		}
		cj.Labels[scrubComponentLabel] = scrubComponentValue
		cj.Labels[scrubPoolGUIDLabel] = guid
		cj.Spec.Schedule = schedule

		pod := &cj.Spec.JobTemplate.Spec.Template.Spec
		if pool == nil || pool.Status.CurrentNode == "" || pool.Status.Health == storagev1alpha1.PoolHealthNodeOffline {
			// No reachable host: suspend rather than schedule a doomed scrub.
			cj.Spec.Suspend = boolPtr(true)
		} else {
			cj.Spec.Suspend = boolPtr(false)
			setHostnameAffinity(pod, pool.Status.CurrentNode)
		}
		if len(pod.Containers) > 0 {
			pod.Containers[0].Args = append(pod.Containers[0].Args, "--pool-guid="+guid)
		}
		return nil
	})
	return err
}

// deleteAll removes every managed scrub CronJob.
func (r *ScrubReconciler) deleteAll(ctx context.Context, items []batchv1.CronJob) error {
	for i := range items {
		if err := r.Delete(ctx, &items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// setHostnameAffinity pins the pod to a specific node by hostname, replacing any
// node affinity from the template.
func setHostnameAffinity(pod *corev1.PodSpec, node string) {
	if pod.Affinity == nil {
		pod.Affinity = &corev1.Affinity{}
	}
	pod.Affinity.NodeAffinity = &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      "kubernetes.io/hostname",
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{node},
				}},
			}},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

// enqueueConfig maps any watched event to the single scrub ConfigMap key so the
// reconciler always recomputes the whole world.
func (r *ScrubReconciler) enqueueConfig(_ context.Context, _ client.Object) []reconcile.Request {
	if r.ConfigMapName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: r.Namespace, Name: r.ConfigMapName}}}
}

// SetupWithManager wires the reconciler: it reconciles on changes to the scrub
// ConfigMap, any ZfsPool (takeover/health), and its own CronJobs (self-heal).
func (r *ScrubReconciler) SetupWithManager(mgr ctrl.Manager) error {
	configMapPred := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetNamespace() == r.Namespace && obj.GetName() == r.ConfigMapName
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(configMapPred)).
		Watches(&storagev1alpha1.ZfsPool{}, handler.EnqueueRequestsFromMapFunc(r.enqueueConfig)).
		Watches(&batchv1.CronJob{}, handler.EnqueueRequestsFromMapFunc(r.enqueueConfig)).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Named("scrub").
		Complete(r)
}

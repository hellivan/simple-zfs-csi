package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

const (
	// scrubComponentLabel marks the objects the scrub reconcilers own.
	scrubComponentLabel = "app.kubernetes.io/component"
	scrubComponentValue = "scrub"
	// scrubPoolGUIDLabel records which pool GUID a scrub object targets.
	scrubPoolGUIDLabel = "maintenance.simple-zfs-csi.io/pool-guid"
	// scrubConfigKey is the ConfigMap data key holding the scrub template YAML.
	scrubConfigKey = "config.yaml"
	// scrubContainerName is the container the reconcilers customise.
	scrubContainerName = "scrub"
)

// errScrubTemplateMissing is returned while the template ConfigMap is absent or
// empty. The reconcilers surface it in status and retry; the ConfigMap watch
// also re-triggers them as soon as it appears.
var errScrubTemplateMissing = errors.New("scrub template ConfigMap is missing or empty")

// ScrubConfig is the template ConfigMap's content, rendered by Helm. Template
// is a base CronJob (image, host-exec, volumes, service account, defaults) that
// both scrub reconcilers clone: the CronJob reconciler uses all of it, the Job
// reconciler uses its jobTemplate (design-decisions ADR-0044).
type ScrubConfig struct {
	Template *batchv1.CronJob `json:"template"`
}

// loadScrubTemplate reads and parses the template ConfigMap.
func loadScrubTemplate(ctx context.Context, c client.Reader, namespace, name string) (*batchv1.CronJob, error) {
	if name == "" {
		return nil, errScrubTemplateMissing
	}
	var cm corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errScrubTemplateMissing
		}
		return nil, err
	}
	raw := cm.Data[scrubConfigKey]
	if strings.TrimSpace(raw) == "" {
		return nil, errScrubTemplateMissing
	}
	var cfg ScrubConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("parse scrub template: %w", err)
	}
	if cfg.Template == nil {
		return nil, errScrubTemplateMissing
	}
	return cfg.Template, nil
}

// poolsByGUID indexes the live ZfsPools by GUID.
func poolsByGUID(ctx context.Context, c client.Reader) (map[string]*storagev1alpha1.ZfsPool, error) {
	var list storagev1alpha1.ZfsPoolList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make(map[string]*storagev1alpha1.ZfsPool, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		out[zfsPoolGUID(p)] = p
	}
	return out, nil
}

func zfsPoolGUID(p *storagev1alpha1.ZfsPool) string {
	if p.Status.GUID != "" {
		return p.Status.GUID
	}
	return strings.TrimPrefix(p.Name, "zpool-")
}

// scrubPoolHost returns the node a scrub for pool must run on, or a reason it
// cannot run right now.
func scrubPoolHost(pool *storagev1alpha1.ZfsPool) (node, unavailable string) {
	switch {
	case pool == nil:
		return "", "pool not found"
	case pool.Status.Health == storagev1alpha1.PoolHealthNodeOffline:
		return "", "pool's node is offline"
	case pool.Status.CurrentNode == "":
		return "", "pool has no current node"
	}
	return pool.Status.CurrentNode, ""
}

// scrubJobOverrides are the per-CR settings applied on top of the template.
type scrubJobOverrides struct {
	timeout                 string
	activeDeadlineSeconds   *int64
	ttlSecondsAfterFinished *int32
}

// buildScrubJobSpec clones the template's Job spec for one pool: it pins the pod
// to node (left unpinned when node is empty), passes --pool-guid, applies the overrides and makes a failure's log
// tail visible in the pod's termination message.
func buildScrubJobSpec(tmpl *batchv1.CronJob, guid, node string, o scrubJobOverrides) batchv1.JobSpec {
	spec := *tmpl.Spec.JobTemplate.Spec.DeepCopy()
	pod := &spec.Template.Spec
	if node != "" {
		setHostnameAffinity(pod, node)
	}
	if len(pod.Containers) > 0 {
		idx := 0
		for i := range pod.Containers {
			if pod.Containers[i].Name == scrubContainerName {
				idx = i
			}
		}
		c := &pod.Containers[idx]
		c.Args = withArg(c.Args, "--pool-guid=", guid)
		if o.timeout != "" {
			c.Args = withArg(c.Args, "--timeout=", o.timeout)
		}
		c.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	}
	if o.activeDeadlineSeconds != nil {
		spec.ActiveDeadlineSeconds = o.activeDeadlineSeconds
	}
	if o.ttlSecondsAfterFinished != nil {
		spec.TTLSecondsAfterFinished = o.ttlSecondsAfterFinished
	}
	return spec
}

// withArg sets "<prefix><value>" in args, replacing an existing flag with the
// same prefix.
func withArg(args []string, prefix, value string) []string {
	out := make([]string, 0, len(args)+1)
	for _, a := range args {
		if !strings.HasPrefix(a, prefix) {
			out = append(out, a)
		}
	}
	return append(out, prefix+value)
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

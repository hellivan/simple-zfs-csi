// Command zfs-migrate is the one-time migration that must run before the
// snapshot-delete redesign driver is deployed (docs/snapshot-delete-redesign-plan.md, S6).
//
// It audits the cluster and the pool and prints what it would change. Nothing is
// changed unless --apply is given. ZFS commands run in the toolbox pod through
// `kubectl exec`, so the machine running this needs kubectl and cluster access.
//
// Exit status: 0 clean, 1 error or blocking findings, 3 changes pending (dry run).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/migrate"
)

type kube struct{ c client.Client }

func (k kube) StampDatasetProvisioned(ctx context.Context, name string, at time.Time) error {
	var o storagev1alpha1.ZfsDataset
	if err := k.c.Get(ctx, client.ObjectKey{Name: name}, &o); err != nil {
		return err
	}
	if o.Status.ProvisionedAt != nil {
		return nil
	}
	t := metav1.NewTime(at)
	o.Status.ProvisionedAt = &t
	return k.c.Status().Update(ctx, &o)
}

func (k kube) StampSnapshotProvisioned(ctx context.Context, name string, at time.Time) error {
	var o storagev1alpha1.ZfsSnapshot
	if err := k.c.Get(ctx, client.ObjectKey{Name: name}, &o); err != nil {
		return err
	}
	if o.Status.ProvisionedAt != nil {
		return nil
	}
	t := metav1.NewTime(at)
	o.Status.ProvisionedAt = &t
	return k.c.Status().Update(ctx, &o)
}

func (k kube) SetSnapshotSourceType(ctx context.Context, name string, t storagev1alpha1.DatasetType) error {
	var o storagev1alpha1.ZfsSnapshot
	if err := k.c.Get(ctx, client.ObjectKey{Name: name}, &o); err != nil {
		return err
	}
	if o.Spec.SourceType != "" {
		return nil
	}
	o.Spec.SourceType = t
	return k.c.Update(ctx, &o)
}

func main() {
	var (
		kubeContext = flag.String("context", "", "kubeconfig context (default: current)")
		namespace   = flag.String("namespace", "simple-zfs-csi", "namespace of the toolbox pod")
		apply       = flag.Bool("apply", false, "make the changes (default: dry run)")
	)
	flag.Parse()
	if err := run(*kubeContext, *namespace, *apply); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(kubeContext, namespace string, apply bool) error {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = storagev1alpha1.AddToScheme(scheme)
	cfg, err := config.GetConfigWithContext(kubeContext)
	if err != nil {
		return err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"app.kubernetes.io/component": "toolbox"}); err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no toolbox pod in namespace %s", namespace)
	}
	pod := pods.Items[0].Name
	runner := func(ctx context.Context, name string, args ...string) (string, error) {
		a := []string{}
		if kubeContext != "" {
			a = append(a, "--context", kubeContext)
		}
		a = append(a, "-n", namespace, "exec", pod, "--", name)
		a = append(a, args...)
		out, err := exec.CommandContext(ctx, "kubectl", a...).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
			}
			return "", err
		}
		return string(out), nil
	}
	be := migrate.CLIBackend{Run: runner}

	in := migrate.Input{Pools: map[string]string{}}
	var pl storagev1alpha1.ZfsPoolList
	if err := c.List(ctx, &pl); err != nil {
		return err
	}
	for _, p := range pl.Items {
		in.Pools[p.Status.GUID] = p.Status.PoolName
	}
	var dl storagev1alpha1.ZfsDatasetList
	if err := c.List(ctx, &dl); err != nil {
		return err
	}
	in.Datasets = dl.Items
	var sl storagev1alpha1.ZfsSnapshotList
	if err := c.List(ctx, &sl); err != nil {
		return err
	}
	in.Snapshots = sl.Items

	plan, err := migrate.Compute(ctx, in, be, time.Now())
	if err != nil {
		return err
	}
	report(plan)
	if len(plan.Blockers()) > 0 {
		os.Exit(1)
	}
	if len(plan.Actions) == 0 {
		return nil
	}
	if !apply {
		fmt.Println("\ndry run: nothing changed; rerun with --apply")
		os.Exit(3)
	}
	if err := migrate.Apply(ctx, plan, be, kube{c}); err != nil {
		return err
	}
	// The audit after applying must be clean; re-read the CRs so the stamps count.
	if err := c.List(ctx, &dl); err != nil {
		return err
	}
	in.Datasets = dl.Items
	if err := c.List(ctx, &sl); err != nil {
		return err
	}
	in.Snapshots = sl.Items
	after, err := migrate.Compute(ctx, in, be, time.Now())
	if err != nil {
		return err
	}
	fmt.Println("\n--- audit after apply ---")
	report(after)
	if !after.Clean() {
		os.Exit(1)
	}
	return nil
}

func report(p *migrate.Plan) {
	fmt.Printf("already labelled: %d, restore-source already marked: %d\n", p.Labelled, p.Marked)
	fmt.Printf("\nactions (%d):\n", len(p.Actions))
	for _, a := range p.Actions {
		fmt.Println("  " + a.String())
	}
	fmt.Printf("\nfindings (%d):\n", len(p.Findings))
	for _, f := range p.Findings {
		fmt.Printf("  [%s] %s: %s\n", f.Severity, f.Subject, f.Message)
	}
}

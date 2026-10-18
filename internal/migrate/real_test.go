//go:build zfsreal

package migrate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// TestRealMigrate builds the old (unlabelled, @restore-source) layout on a real
// pool and runs the migration against it. It never talks to Kubernetes: the CRs
// are made up in memory. It runs only when ZFS_CONF_ROOT is a dataset whose path
// contains "verify_zfs_functions", and every zfs call is checked against that
// root before it runs, so it cannot touch other data.
//
//	CGO_ENABLED=0 go test -c -tags zfsreal -o migrate.test ./internal/migrate
//	ZFS_CONF_ROOT=pool/verify_zfs_functions ./migrate.test -test.v
func TestRealMigrate(t *testing.T) {
	root := strings.Trim(os.Getenv("ZFS_CONF_ROOT"), "/")
	if !strings.Contains(root, "verify_zfs_functions") || !strings.Contains(root, "/") {
		t.Skip("set ZFS_CONF_ROOT to a dataset path containing verify_zfs_functions")
	}
	pool := root[:strings.Index(root, "/")]
	sub := fmt.Sprintf("migrate-test-%d", time.Now().Unix())
	base := root + "/" + sub
	ctx := context.Background()

	guarded := func(ctx context.Context, name string, args ...string) (string, error) {
		if name != "zfs" {
			panic("refusing to run " + name)
		}
		named := 0
		for _, a := range args {
			if strings.HasPrefix(a, "-") || !strings.ContainsAny(a, "/@") {
				continue
			}
			target := a
			if i := strings.IndexByte(target, '@'); i >= 0 {
				target = target[:i]
			}
			if target != base && !strings.HasPrefix(target, base+"/") {
				panic(fmt.Sprintf("refusing zfs %v: %q is outside %s", args, a, base))
			}
			named++
		}
		if named == 0 {
			panic(fmt.Sprintf("refusing zfs %v: no dataset named", args))
		}
		out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("zfs %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out), nil
	}
	z := zpool.NewZFS(guarded)
	t.Cleanup(func() {
		if _, err := guarded(ctx, "zfs", "destroy", "-R", base); err != nil {
			t.Logf("cleanup: %v", err)
		}
	})

	must(t, z.CreateDataset(ctx, base, nil))
	n := func(s string) string { return base + "/" + s }
	// Old layout: pvc-1; snapshot csi-snap-A with backing clone and @restore-source;
	// restore pvc-2 from @restore-source and promoted; direct clone pvc-3 of pvc-1;
	// restore pvc-4 not promoted (pins @restore-source of csi-snap-B).
	must(t, z.CreateDataset(ctx, n("pvc-1"), nil))
	must(t, z.Snapshot(ctx, n("pvc-1@csi-snap-A")))
	must(t, z.Clone(ctx, n("pvc-1@csi-snap-A"), n("csi-snap-A"), nil))
	must(t, z.Snapshot(ctx, n("csi-snap-A@restore-source")))
	must(t, z.Clone(ctx, n("csi-snap-A@restore-source"), n("pvc-2"), nil))
	must(t, z.Promote(ctx, n("pvc-2")))
	must(t, z.Snapshot(ctx, n("pvc-1@clone-pvc-3")))
	must(t, z.Clone(ctx, n("pvc-1@clone-pvc-3"), n("pvc-3"), nil))
	must(t, z.Snapshot(ctx, n("pvc-1@csi-snap-B")))
	must(t, z.Clone(ctx, n("pvc-1@csi-snap-B"), n("csi-snap-B"), nil))
	must(t, z.Snapshot(ctx, n("csi-snap-B@restore-source")))
	must(t, z.Clone(ctx, n("csi-snap-B@restore-source"), n("pvc-4"), nil))

	d := func(name string) storagev1alpha1.ZfsDataset {
		o := ds(name, nil)
		o.Spec.PoolGUID, o.Spec.Dataset = "1", sub_path(root, pool)+"/"+sub+"/"+name
		return o
	}
	sn := func(name, snapName, src string) storagev1alpha1.ZfsSnapshot {
		o := snap(name, snapName, src)
		o.Spec.PoolGUID, o.Spec.Dataset = "1", sub_path(root, pool)+"/"+sub+"/"+src
		return o
	}
	at := metav1.NewTime(time.Unix(1700000000, 0))
	in := Input{
		Pools:     map[string]string{"1": pool},
		Datasets:  []storagev1alpha1.ZfsDataset{d("pvc-1"), d("pvc-2"), d("pvc-3"), d("pvc-4")},
		Snapshots: []storagev1alpha1.ZfsSnapshot{sn("snap-A", "csi-snap-A", "pvc-1"), sn("snap-B", "csi-snap-B", "pvc-1")},
	}
	for i := range in.Datasets {
		in.Datasets[i].Status.ProvisionedAt = &at
	}
	for i := range in.Snapshots {
		in.Snapshots[i].Status.ProvisionedAt = &at
	}
	be := CLIBackend{Run: guarded}

	p, err := Compute(ctx, in, be, time.Now())
	must(t, err)
	for _, a := range p.Actions {
		t.Log(a)
	}
	for _, f := range p.Findings {
		t.Logf("finding: %+v", f)
	}
	if len(p.Blockers()) != 0 {
		t.Fatalf("blockers: %v", p.Blockers())
	}
	// 9 objects (4 volumes, 2 raw, 2 backing, 1 clone origin) with 2 labels each, 2 restore-source.
	if len(p.Actions) != 20 {
		t.Fatalf("got %d actions, want 20", len(p.Actions))
	}
	must(t, Apply(ctx, p, be, &fakeKube{}))

	get := func(name, prop string) string {
		out, err := guarded(ctx, "zfs", "get", "-H", "-o", "value", "-s", "local", prop, name)
		must(t, err)
		return strings.TrimSpace(out)
	}
	for name, role := range map[string]string{
		n("pvc-1"): "volume", n("pvc-2"): "volume", n("pvc-3"): "volume", n("pvc-4"): "volume",
		n("pvc-1@csi-snap-B"): "raw", n("csi-snap-B"): "backing", n("pvc-1@clone-pvc-3"): "clone-origin",
	} {
		if got := get(name, zpool.PropRole); got != role {
			t.Errorf("%s role = %q, want %q", name, got, role)
		}
	}
	if got := get(n("pvc-1@csi-snap-A"), zpool.PropRole); got != "raw" {
		t.Errorf("raw snapshot A role = %q", got)
	}

	list, err := guarded(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name,defer_destroy", "-r", base)
	must(t, err)
	t.Logf("snapshots after apply:\n%s", list)
	for _, l := range strings.Split(strings.TrimSpace(list), "\n") {
		if strings.Contains(l, "@restore-source") && !strings.HasSuffix(l, "\ton") {
			t.Errorf("restore-source not marked: %s", l)
		}
	}

	p2, err := Compute(ctx, in, be, time.Now())
	must(t, err)
	if !p2.Clean() {
		t.Errorf("second audit not clean: %v %v", p2.Actions, p2.Findings)
	}

	// ZFS removes a marked snapshot when its last clone goes: destroying the
	// restore must leave no restore-source behind.
	must(t, z.Destroy(ctx, n("pvc-4"), false))
	list, _ = guarded(ctx, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-r", base)
	if strings.Contains(list, "csi-snap-B@restore-source") {
		t.Errorf("marked restore-source survived its last clone:\n%s", list)
	}
}

func sub_path(root, pool string) string { return strings.TrimPrefix(root, pool+"/") }

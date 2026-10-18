package migrate

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
	"github.com/hellivan/simple-zfs-csi/internal/zpool/memzfs"
)

type memBackend struct{ m *memzfs.Mem }

func (b memBackend) List(ctx context.Context, roots []string) ([]Obj, error) {
	all := append(b.m.Datasets(), b.m.Snapshots()...)
	var out []Obj
	for _, n := range all {
		ds := n
		if i := strings.IndexByte(n, '@'); i >= 0 {
			ds = n[:i]
		}
		for _, r := range roots {
			if ds == r || strings.HasPrefix(ds, r+"/") && strings.Count(ds[len(r)+1:], "/") == 0 {
				out = append(out, Obj{
					Name: n, Origin: b.m.Origin(n), Deferred: b.m.Deferred(n),
					Role: b.m.Local(n, zpool.PropRole), Owner: b.m.Local(n, zpool.PropOwner),
				})
				break
			}
		}
	}
	return out, nil
}
func (b memBackend) Set(ctx context.Context, n, p, v string) error {
	return b.m.SetProperty(ctx, n, p, v)
}
func (b memBackend) DestroyDeferred(ctx context.Context, n string) error {
	return b.m.DestroySnapshotDeferred(ctx, n)
}

type fakeKube struct{ calls []string }

func (k *fakeKube) StampDatasetProvisioned(_ context.Context, n string, _ time.Time) error {
	k.calls = append(k.calls, "ds:"+n)
	return nil
}
func (k *fakeKube) StampSnapshotProvisioned(_ context.Context, n string, _ time.Time) error {
	k.calls = append(k.calls, "snap:"+n)
	return nil
}
func (k *fakeKube) SetSnapshotSourceType(_ context.Context, n string, t storagev1alpha1.DatasetType) error {
	k.calls = append(k.calls, "type:"+n+"="+string(t))
	return nil
}

func ds(name string, src *storagev1alpha1.DatasetSource) storagev1alpha1.ZfsDataset {
	return storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec:       storagev1alpha1.ZfsDatasetSpec{PoolGUID: "1", Dataset: "k8s/" + name, Type: storagev1alpha1.DatasetTypeFilesystem, Source: src},
		Status:     storagev1alpha1.ZfsDatasetStatus{Phase: storagev1alpha1.DatasetPhaseReady},
	}
}

func snap(name, snapName, src string) storagev1alpha1.ZfsSnapshot {
	return storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: storagev1alpha1.ZfsSnapshotSpec{PoolGUID: "1", Dataset: "k8s/" + src, SnapshotName: snapName,
			SourceVolume: src, SourceType: storagev1alpha1.DatasetTypeFilesystem},
		Status: storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhaseReady},
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// oldLayout builds, without labels, the layout the previous driver produced:
// volume pvc-1, snapshot snap-1 with its backing clone and @restore-source, a
// restore pvc-2 (promoted, so @restore-source lives on it), and a direct clone pvc-3.
func oldLayout(t *testing.T) (*memzfs.Mem, Input) {
	t.Helper()
	ctx := context.Background()
	m := memzfs.New("tank")
	must(t, m.CreateDataset(ctx, "tank/k8s", nil))
	must(t, m.CreateDataset(ctx, "tank/k8s/pvc-1", nil))
	must(t, m.Snapshot(ctx, "tank/k8s/pvc-1@csi-snap-A"))
	must(t, m.Clone(ctx, "tank/k8s/pvc-1@csi-snap-A", "tank/k8s/csi-snap-A", nil))
	must(t, m.Snapshot(ctx, "tank/k8s/csi-snap-A@restore-source"))
	must(t, m.Clone(ctx, "tank/k8s/csi-snap-A@restore-source", "tank/k8s/pvc-2", nil))
	must(t, m.Promote(ctx, "tank/k8s/pvc-2"))
	must(t, m.Snapshot(ctx, "tank/k8s/pvc-1@clone-pvc-3"))
	must(t, m.Clone(ctx, "tank/k8s/pvc-1@clone-pvc-3", "tank/k8s/pvc-3", nil))
	in := Input{
		Pools:     map[string]string{"1": "tank"},
		Datasets:  []storagev1alpha1.ZfsDataset{ds("pvc-1", nil), ds("pvc-2", &storagev1alpha1.DatasetSource{Snapshot: "x"}), ds("pvc-3", &storagev1alpha1.DatasetSource{Volume: "k8s/pvc-1"})},
		Snapshots: []storagev1alpha1.ZfsSnapshot{snap("snap-1", "csi-snap-A", "pvc-1")},
	}
	return m, in
}

func run(t *testing.T, in Input, be Backend) *Plan {
	t.Helper()
	p, err := Compute(context.Background(), in, be, time.Unix(1700000000, 0))
	must(t, err)
	return p
}

func TestMigrateOldLayout(t *testing.T) {
	m, in := oldLayout(t)
	be := memBackend{m}
	p := run(t, in, be)
	if b := p.Blockers(); len(b) != 0 {
		t.Fatalf("blockers: %v", b)
	}
	k := &fakeKube{}
	must(t, Apply(context.Background(), p, be, k))

	want := map[string][2]string{
		"tank/k8s/pvc-1":             {zpool.RoleVolume, "uid-pvc-1"},
		"tank/k8s/pvc-2":             {zpool.RoleVolume, "uid-pvc-2"},
		"tank/k8s/pvc-3":             {zpool.RoleVolume, "uid-pvc-3"},
		"tank/k8s/pvc-1@csi-snap-A":  {zpool.RoleRaw, "uid-snap-1"},
		"tank/k8s/csi-snap-A":        {zpool.RoleBacking, "uid-snap-1"},
		"tank/k8s/pvc-1@clone-pvc-3": {zpool.RoleCloneOrigin, "uid-pvc-3"},
	}
	for n, w := range want {
		if got := [2]string{m.Local(n, zpool.PropRole), m.Local(n, zpool.PropOwner)}; got != w {
			t.Errorf("%s labels = %v, want %v", n, got, w)
		}
	}
	// pvc-2 was promoted, so @restore-source now lives on it: it must be marked.
	for _, n := range m.Snapshots() {
		if strings.HasSuffix(n, "@"+RestoreSourceSnapshot) && !m.Deferred(n) {
			t.Errorf("%s not marked", n)
		}
	}
	if len(k.calls) != 4 {
		t.Errorf("kube calls = %v, want 3 datasets + 1 snapshot stamped", k.calls)
	}
	must(t, m.Check())

	// Idempotent: once the stamps are on the CRs a second audit has nothing to do.
	at := metav1.NewTime(time.Unix(1700000000, 0))
	for i := range in.Datasets {
		in.Datasets[i].Status.ProvisionedAt = &at
	}
	in.Snapshots[0].Status.ProvisionedAt = &at
	if p2 := run(t, in, be); !p2.Clean() {
		t.Errorf("second audit not clean: %v %v", p2.Actions, p2.Findings)
	}
}

func TestMigrateInheritedLabelIsNotALabel(t *testing.T) {
	ctx := context.Background()
	m, in := oldLayout(t)
	// The volume is already labelled; its snapshot inherits the value but must
	// still be labelled raw.
	must(t, m.SetProperty(ctx, "tank/k8s/pvc-1", zpool.PropRole, zpool.RoleVolume))
	must(t, m.SetProperty(ctx, "tank/k8s/pvc-1", zpool.PropOwner, "uid-pvc-1"))
	p := run(t, in, memBackend{m})
	found := false
	for _, a := range p.Actions {
		if a.Kind == ActSet && a.Target == "tank/k8s/pvc-1@csi-snap-A" && a.Prop == zpool.PropRole && a.Value == zpool.RoleRaw {
			found = true
		}
		if a.Kind == ActSet && a.Target == "tank/k8s/pvc-1" {
			t.Errorf("already labelled volume gets action %v", a)
		}
	}
	if !found {
		t.Errorf("raw snapshot not labelled: %v", p.Actions)
	}
}

func TestMigrateConflictingLabelIsReportedNotChanged(t *testing.T) {
	ctx := context.Background()
	m, in := oldLayout(t)
	must(t, m.SetProperty(ctx, "tank/k8s/pvc-2", zpool.PropOwner, "someone-else"))
	p := run(t, in, memBackend{m})
	if len(p.Blockers()) != 1 || !strings.Contains(p.Blockers()[0].Message, "someone-else") {
		t.Fatalf("blockers = %v", p.Blockers())
	}
	for _, a := range p.Actions {
		if a.Target == "tank/k8s/pvc-2" && a.Prop == zpool.PropOwner {
			t.Errorf("conflicting label would be overwritten: %v", a)
		}
	}
}

func TestMigrateForeignRestoreSourceIsNotTouched(t *testing.T) {
	ctx := context.Background()
	m, in := oldLayout(t)
	must(t, m.CreateDataset(ctx, "tank/k8s/foreign", nil))
	must(t, m.Snapshot(ctx, "tank/k8s/foreign@restore-source"))
	p := run(t, in, memBackend{m})
	if len(p.Blockers()) != 1 || p.Blockers()[0].Subject != "tank/k8s/foreign@restore-source" {
		t.Fatalf("blockers = %v", p.Blockers())
	}
	for _, a := range p.Actions {
		if a.Target == "tank/k8s/foreign@restore-source" {
			t.Errorf("foreign snapshot would be destroyed: %v", a)
		}
	}
}

func TestMigrateOrphansAreReported(t *testing.T) {
	ctx := context.Background()
	m, in := oldLayout(t)
	must(t, m.Snapshot(ctx, "tank/k8s/pvc-1@csi-snap-GONE"))
	p := run(t, in, memBackend{m})
	var orphan bool
	for _, f := range p.Findings {
		if f.Subject == "tank/k8s/pvc-1@csi-snap-GONE" && f.Severity == Info {
			orphan = true
		}
	}
	if !orphan {
		t.Errorf("orphan not reported: %v", p.Findings)
	}
	for _, a := range p.Actions {
		if a.Target == "tank/k8s/pvc-1@csi-snap-GONE" {
			t.Errorf("orphan touched: %v", a)
		}
	}
}

func TestMigrateSourceType(t *testing.T) {
	m, in := oldLayout(t)
	in.Snapshots[0].Spec.SourceType = ""
	p := run(t, in, memBackend{m})
	var got bool
	for _, a := range p.Actions {
		if a.Kind == ActSnapshotSourceType && a.Value == "filesystem" {
			got = true
		}
	}
	if !got {
		t.Errorf("sourceType not derived: %v", p.Actions)
	}
	in.Datasets = in.Datasets[1:] // source gone
	p = run(t, in, memBackend{m})
	if len(p.Blockers()) == 0 {
		t.Errorf("empty sourceType with the source gone must block")
	}
}

func TestWithinOneLevel(t *testing.T) {
	for name, want := range map[string]bool{
		"tank/k8s": true, "tank/k8s@s": true, "tank/k8s/a": true, "tank/k8s/a@s": true,
		"tank/k8s/a/b": false, "tank/k8s/a/b@s": false, "tank/k8sx/a": false,
	} {
		if got := withinOneLevel(name, "tank/k8s"); got != want {
			t.Errorf("withinOneLevel(%q) = %v, want %v", name, got, want)
		}
	}
}

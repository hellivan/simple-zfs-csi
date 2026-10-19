// Package zfsconf is the ZFS conformance suite. Every case states one behaviour
// of ZFS that the driver relies on (the rows of docs/zfs-conformance-checklist.md)
// and runs, unchanged, against the in-memory model (memzfs) and against a real
// pool. A case that passes on the real pool but fails on the model is a bug in
// the model; the driver's scenario tests are only as trustworthy as that match.
package zfsconf

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// Options tunes the suite for the backend.
type Options struct {
	// Pause separates snapshots in time where list order is asserted
	// (ZFS orders by creation time, whole seconds). Zero for the model.
	Pause time.Duration
	// Check, when set, is called after every case (model invariants).
	Check func() error
	// Skip names cases to skip (for known model-only or real-only gaps).
	Skip map[string]string
}

type env struct {
	t    *testing.T
	z    zpool.ZFS
	ctx  context.Context
	root string
	kind zpool.DatasetKind
	opt  Options
	n    int
}

// Run executes the suite below root (a dataset path that must not exist yet and
// whose parent must exist). Everything it creates is destroyed again.
func Run(t *testing.T, z zpool.ZFS, root string, opt Options) {
	t.Helper()
	ctx := context.Background()
	if err := z.CreateDataset(ctx, root, map[string]string{"mountpoint": "none"}); err != nil {
		t.Fatalf("create suite root %s: %v", root, err)
	}
	t.Cleanup(func() { retryBusy(ctx, func() error { return z.Destroy(ctx, root, true) }) })
	for _, kind := range []zpool.DatasetKind{zpool.KindFilesystem, zpool.KindVolume} {
		kind := kind
		for _, c := range cases {
			c := c
			if c.fsOnly && kind != zpool.KindFilesystem {
				continue
			}
			t.Run(string(kind)+"/"+c.name, func(t *testing.T) {
				if why, ok := opt.Skip[c.name]; ok {
					t.Skip(why)
				}
				e := &env{t: t, z: z, ctx: ctx, kind: kind, opt: opt}
				e.root = fmt.Sprintf("%s/%s-%s", root, kind, c.name)
				e.must(z.CreateDataset(ctx, e.root, map[string]string{"mountpoint": "none"}))
				defer retryBusy(ctx, func() error { return z.Destroy(ctx, e.root, true) })
				c.run(e)
				if opt.Check != nil {
					if err := opt.Check(); err != nil {
						t.Errorf("invariant: %v", err)
					}
				}
			})
		}
	}
}

type testCase struct {
	name   string
	fsOnly bool
	run    func(e *env)
}

func retryBusy(ctx context.Context, f func() error) error {
	var err error
	for i := 0; i < 15; i++ {
		if err = f(); err == nil || !strings.Contains(err.Error(), "dataset is busy") {
			return err
		}
		time.Sleep(time.Second)
	}
	return err
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) name(p string) string { e.n++; return fmt.Sprintf("%s/%s%d", e.root, p, e.n) }

// mk creates a filesystem or zvol, depending on the kind under test.
func (e *env) mk() string {
	e.t.Helper()
	n := e.name("d")
	if e.kind == zpool.KindVolume {
		e.must(e.z.CreateZvol(e.ctx, n, 8<<20, nil))
	} else {
		e.must(e.z.CreateDataset(e.ctx, n, nil))
	}
	return n
}

func (e *env) snap(ds string, shorts ...string) {
	e.t.Helper()
	for _, s := range shorts {
		e.must(e.z.Snapshot(e.ctx, ds+"@"+s))
	}
}

func (e *env) clone(snap string) string {
	e.t.Helper()
	n := e.name("c")
	e.must(e.z.Clone(e.ctx, snap, n, nil))
	return n
}

func (e *env) destroy(name string) {
	e.t.Helper()
	e.must(retryBusy(e.ctx, func() error { return e.z.Destroy(e.ctx, name, false) }))
}

func (e *env) origin(ds string) string {
	e.t.Helper()
	v, err := e.z.Get(e.ctx, ds, "origin")
	e.must(err)
	if v == "-" {
		return ""
	}
	return v
}

// snaps lists a dataset's snapshot short names, sorted by name.
func (e *env) snaps(ds string) []string {
	e.t.Helper()
	l, err := e.z.ListSnapshots(e.ctx, ds)
	e.must(err)
	var out []string
	for _, s := range l {
		out = append(out, s[strings.Index(s, "@")+1:])
	}
	sort.Strings(out)
	return out
}

func (e *env) clonesOf(snap string) []string {
	e.t.Helper()
	l, err := e.z.Clones(e.ctx, snap)
	e.must(err)
	sort.Strings(l)
	return l
}

func (e *env) get(name, prop string) string {
	e.t.Helper()
	v, err := e.z.Get(e.ctx, name, prop)
	e.must(err)
	return v
}

func (e *env) exists(name string) bool {
	_, err := e.z.Get(e.ctx, name, "type")
	if err == nil {
		return true
	}
	if errors.Is(err, zpool.ErrNotExist) {
		return false
	}
	e.t.Fatal(err)
	return false
}

func (e *env) eq(what string, got, want any) {
	e.t.Helper()
	if g, w := reflect.ValueOf(got), reflect.ValueOf(want); g.Kind() == reflect.Slice && g.Len() == 0 && w.Kind() == reflect.Slice && w.Len() == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		e.t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func (e *env) errHas(what string, err error, texts ...string) {
	e.t.Helper()
	if err == nil {
		e.t.Errorf("%s: expected an error containing %q, got none", what, texts)
		return
	}
	for _, t := range texts {
		if !strings.Contains(err.Error(), t) {
			e.t.Errorf("%s: error %q does not contain %q", what, err, t)
		}
	}
}

func (e *env) deferred(snap string) bool { return e.get(snap, "defer_destroy") == "on" }

func (e *env) pause() {
	if e.opt.Pause > 0 {
		time.Sleep(e.opt.Pause)
	}
}

var cases = []testCase{
	// 1. Deferred destroy
	{"deferred-no-clone-destroys-at-once", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s")
		e.must(e.z.DestroySnapshotDeferred(e.ctx, a+"@s"))
		e.eq("snapshots", e.snaps(a), []string(nil))
	}},
	{"deferred-with-clone-marks-and-last-clone-removes", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s")
		c := e.clone(a + "@s")
		e.must(e.z.DestroySnapshotDeferred(e.ctx, a+"@s"))
		e.must(e.z.DestroySnapshotDeferred(e.ctx, a+"@s"))
		e.eq("snapshots stay visible", e.snaps(a), []string{"s"})
		e.eq("marked", e.deferred(a+"@s"), true)
		e.eq("clones", e.clonesOf(a+"@s"), []string{c})
		c2 := e.clone(a + "@s") // new clones of a marked snapshot are allowed
		e.eq("clones", e.clonesOf(a+"@s"), sorted(c, c2))
		e.destroy(c)
		e.eq("still pinned by the other clone", e.snaps(a), []string{"s"})
		e.destroy(c2)
		e.eq("removed by ZFS with the last clone", e.snaps(a), []string(nil))
	}},
	{"deferred-missing-is-not-an-error", false, func(e *env) {
		a := e.mk()
		e.must(e.z.DestroySnapshotDeferred(e.ctx, a+"@nope"))
		e.must(e.z.DestroySnapshotDeferred(e.ctx, e.root+"/nodataset@nope"))
	}},
	{"snapshot-of-a-clone-does-not-release-its-origin", false, func(e *env) {
		a := e.mk()
		e.snap(a, "o")
		c := e.clone(a + "@o")
		e.must(e.z.DestroySnapshotDeferred(e.ctx, a+"@o"))
		e.snap(c, "mine")
		e.eq("origin still pinned", e.snaps(a), []string{"o"})
		e.eq("clones", e.clonesOf(a+"@o"), []string{c})
	}},
	// 2. Promote
	{"promote-moves-origin-and-older-snapshots", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s1", "s2", "s3")
		c := e.clone(a + "@s2")
		e.must(e.z.Promote(e.ctx, c))
		e.eq("promoted dataset", e.snaps(c), []string{"s1", "s2"})
		e.eq("old parent keeps the newer one", e.snaps(a), []string{"s3"})
		e.eq("old parent is now a clone", e.origin(a), c+"@s2")
		e.eq("promoted is independent", e.origin(c), "")
		e.must(e.z.Promote(e.ctx, c)) // idempotent on an independent dataset
	}},
	{"promote-moves-mark-and-user-properties", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s")
		e.must(e.z.SetProperty(e.ctx, a+"@s", "t:role", "x"))
		c := e.clone(a + "@s")
		e.must(e.z.DestroySnapshotDeferred(e.ctx, a+"@s"))
		e.must(e.z.Promote(e.ctx, c))
		e.eq("moved", e.snaps(a), []string(nil))
		e.eq("snapshot on promoted", e.snaps(c), []string{"s"})
		e.eq("mark moved", e.deferred(c+"@s"), true)
		e.eq("property moved", e.get(c+"@s", "t:role"), "x")
		e.eq("old parent is its clone", e.clonesOf(c+"@s"), []string{a})
	}},
	{"promote-takes-over-the-parents-origin", false, func(e *env) {
		q := e.mk()
		e.snap(q, "x")
		p := e.clone(q + "@x")
		e.snap(p, "o")
		c := e.clone(p + "@o")
		e.must(e.z.Promote(e.ctx, c))
		e.eq("promoted takes the old parent's origin", e.origin(c), q+"@x")
		e.eq("old parent clones the moved snapshot", e.origin(p), c+"@o")
	}},
	{"promote-reparents-sibling-clones", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s")
		c1, c2 := e.clone(a+"@s"), e.clone(a+"@s")
		e.must(e.z.Promote(e.ctx, c1))
		e.eq("sibling origin", e.origin(c2), c1+"@s")
		e.eq("clones of the moved snapshot", e.clonesOf(c1+"@s"), sorted(a, c2))
	}},
	{"promote-conflict-names-the-snapshot", false, func(e *env) {
		a := e.mk()
		e.snap(a, "x")
		c := e.clone(a + "@x")
		e.snap(c, "x")
		e.errHas("promote", e.z.Promote(e.ctx, c), "conflicting snapshot 'x'")
		e.eq("nothing moved", e.snaps(a), []string{"x"})
	}},
	{"promote-of-a-chain-moves-one-level", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s")
		b := e.clone(a + "@s")
		e.snap(b, "t")
		c := e.clone(b + "@t")
		e.must(e.z.Promote(e.ctx, c))
		e.eq("c owns t", e.snaps(c), []string{"t"})
		e.eq("b is a clone of c@t", e.origin(b), c+"@t")
		e.eq("a is still the origin of b's lineage", e.origin(c), a+"@s")
	}},
	// 3. Destroy of datasets
	{"destroy-refused-while-snapshots-exist", false, func(e *env) {
		a := e.mk()
		e.snap(a, "plain")
		e.errHas("plain snapshot", e.z.Destroy(e.ctx, a, false), "has children")
		b := e.mk()
		e.snap(b, "m")
		c := e.clone(b + "@m")
		e.must(e.z.DestroySnapshotDeferred(e.ctx, b+"@m"))
		e.errHas("marked snapshot", e.z.Destroy(e.ctx, b, false), "has children")
		e.errHas("recursive with outside clone", e.z.Destroy(e.ctx, b, true), "has dependent clones")
		e.errHas("snapshot with clone", e.z.Destroy(e.ctx, b+"@m", false), "dependent clones")
		e.destroy(c)
		e.eq("marked snapshot went with its last clone", e.snaps(b), []string(nil))
		e.destroy(b)
		e.eq("gone", e.exists(b), false)
	}},
	{"destroy-missing-is-not-an-error", false, func(e *env) {
		e.must(e.z.Destroy(e.ctx, e.root+"/nope", false))
		e.must(e.z.Destroy(e.ctx, e.root+"/nope@s", false))
	}},
	{"promote-then-destroy-old-parent-frees-marked-snapshot", false, func(e *env) {
		a := e.mk()
		e.snap(a, "raw")
		bk := e.clone(a + "@raw")
		r := e.clone(a + "@raw")
		e.must(e.z.DestroySnapshotDeferred(e.ctx, a+"@raw"))
		e.must(e.z.Promote(e.ctx, bk))
		e.must(e.z.Promote(e.ctx, r))
		e.destroy(bk)
		e.eq("r owns the marked snapshot", e.snaps(r), []string{"raw"})
		e.destroy(a)
		e.eq("a gone", e.exists(a), false)
	}},
	// 4. Listing, snapshots, clone properties
	{"snapshot-list-order-after-promote", false, func(e *env) {
		a := e.mk()
		for _, s := range []string{"s1", "s2", "s3"} {
			e.snap(a, s)
			e.pause()
		}
		c := e.clone(a + "@s2")
		e.must(e.z.Promote(e.ctx, c))
		l, err := e.z.ListSnapshots(e.ctx, c)
		e.must(err)
		e.eq("oldest first", l, []string{c + "@s1", c + "@s2"})
	}},
	{"atomic-multi-snapshot", false, func(e *env) {
		a, b := e.mk(), e.mk()
		e.must(e.z.Snapshot(e.ctx, a+"@g", b+"@g"))
		e.eq("same transaction group", e.get(a+"@g", "createtxg"), e.get(b+"@g", "createtxg"))
		e.must(e.z.Snapshot(e.ctx, a+"@g", b+"@g")) // all exist: idempotent
		err := e.z.Snapshot(e.ctx, a+"@g", b+"@h")
		if !errors.Is(err, zpool.ErrSnapshotsPartiallyExist) {
			e.t.Errorf("partial: got %v, want ErrSnapshotsPartiallyExist", err)
		}
		e.eq("nothing created by the failed call", e.exists(b+"@h"), false)
		e.errHas("missing dataset", e.z.Snapshot(e.ctx, a+"@z", e.root+"/nope@z"), "does not exist")
		e.eq("atomic: no member created", e.exists(a+"@z"), false)
	}},
	{"clone-applies-properties", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s")
		c := e.name("c")
		e.must(e.z.Clone(e.ctx, a+"@s", c, map[string]string{"t:p": "v"}))
		e.eq("prop", e.get(c, "t:p"), "v")
		e.must(e.z.Clone(e.ctx, a+"@s", c, nil)) // idempotent
	}},
	// 5. Labels (user properties)
	{"user-properties-on-create-and-clone", false, func(e *env) {
		n := e.name("d")
		if e.kind == zpool.KindVolume {
			e.must(e.z.CreateZvol(e.ctx, n, 8<<20, map[string]string{"t:role": "volume", "t:owner": "u1"}))
		} else {
			e.must(e.z.CreateDataset(e.ctx, n, map[string]string{"t:role": "volume", "t:owner": "u1"}))
		}
		e.eq("role", e.get(n, "t:role"), "volume")
		e.snap(n, "s")
		e.eq("snapshot reads the dataset's value until set", e.get(n+"@s", "t:role"), "volume")
		e.must(e.z.SetProperty(e.ctx, n+"@s", "t:role", "raw"))
		e.eq("snapshot local value", e.get(n+"@s", "t:role"), "raw")
		e.eq("dataset unchanged", e.get(n, "t:role"), "volume")
		c := e.name("c")
		e.must(e.z.Clone(e.ctx, n+"@s", c, map[string]string{"t:role": "backing"}))
		e.eq("clone has its own value", e.get(c, "t:role"), "backing")
		e.eq("clone does not inherit the snapshot's owner", e.get(c, "t:owner"), "-")
		e.eq("unset property", e.get(n, "t:none"), "-")
	}},
	{"user-properties-are-inherited-from-the-parent-dataset", true, func(e *env) {
		p := e.name("p")
		e.must(e.z.CreateDataset(e.ctx, p, map[string]string{"t:role": "inherit-me"}))
		k := p + "/kid"
		e.must(e.z.CreateDataset(e.ctx, k, nil))
		e.eq("kid inherits", e.get(k, "t:role"), "inherit-me")
	}},
	{"snapshot-rejects-non-user-properties", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s")
		e.errHas("set", e.z.SetProperty(e.ctx, a+"@s", "compression", "off"), "snapshot")
		e.errHas("missing", e.z.SetProperty(e.ctx, a+"@nope", "t:x", "1"), "does not exist")
	}},
	{"properties-survive-deferred-destroy-and-promote", false, func(e *env) {
		a := e.mk()
		e.snap(a, "s")
		e.must(e.z.SetProperty(e.ctx, a+"@s", "t:owner", "uid"))
		c := e.clone(a + "@s")
		e.must(e.z.DestroySnapshotDeferred(e.ctx, a+"@s"))
		e.eq("after mark", e.get(a+"@s", "t:owner"), "uid")
		e.must(e.z.Promote(e.ctx, c))
		e.eq("after promote", e.get(c+"@s", "t:owner"), "uid")
	}},
	// 6. The raw-snapshot origin rule (P4)
	{"backing-clone-origin-follows-the-raw-snapshot", false, func(e *env) {
		src := e.mk()
		e.snap(src, "raw")
		bk := e.clone(src + "@raw")
		r1, r2 := e.clone(src+"@raw"), e.clone(src+"@raw")
		e.eq("initially on the source", e.origin(bk), src+"@raw")
		e.must(e.z.DestroySnapshotDeferred(e.ctx, src+"@raw"))
		e.must(e.z.Promote(e.ctx, r1))
		e.eq("after promoting a restore", e.origin(bk), r1+"@raw")
		e.must(e.z.Promote(e.ctx, r2))
		e.eq("after promoting another restore", e.origin(bk), r2+"@raw")
		e.must(e.z.Promote(e.ctx, bk))
		l, err := e.z.ListSnapshots(e.ctx, bk)
		e.must(err)
		e.eq("the raw snapshot moved onto the promoted backing clone", l, []string{bk + "@raw"})
		e.eq("backing origin is empty", e.origin(bk), "")
	}},
}

func sorted(s ...string) []string { sort.Strings(s); return s }

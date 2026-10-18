// Package migrate computes and applies the one-time migration that precedes the
// snapshot-delete redesign (docs/snapshot-delete-redesign-plan.md, S6). It labels
// every pre-label ZFS object whose owner is derivable from a CR, marks the old
// "@restore-source" snapshots with `destroy -d`, and stamps status.provisionedAt
// on settled CRs that predate the field. It never guesses: anything it cannot
// derive is reported, not changed.
package migrate

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// RestoreSourceSnapshot is the short name of the old self-snapshot on a backing clone.
const RestoreSourceSnapshot = "restore-source"

// Obj is a dataset, zvol or snapshot as the audit sees it. Role and Owner are
// the locally set label values only: a snapshot inherits user properties from
// its dataset, and an inherited value must never count as a label.
type Obj struct {
	Name     string
	Origin   string
	Role     string
	Owner    string
	Deferred bool
}

// Short is the part after "@", or "" for a dataset.
func (o Obj) Short() string {
	if i := strings.IndexByte(o.Name, '@'); i >= 0 {
		return o.Name[i+1:]
	}
	return ""
}

// Dataset is the name up to "@".
func (o Obj) Dataset() string {
	if i := strings.IndexByte(o.Name, '@'); i >= 0 {
		return o.Name[:i]
	}
	return o.Name
}

// IsSnapshot reports whether the object is a snapshot.
func (o Obj) IsSnapshot() bool { return strings.IndexByte(o.Name, '@') >= 0 }

// Backend is the ZFS side of the migration.
type Backend interface {
	// List returns every dataset, zvol and snapshot directly below each root
	// (one level), including the roots' snapshots.
	List(ctx context.Context, roots []string) ([]Obj, error)
	Set(ctx context.Context, name, prop, value string) error
	DestroyDeferred(ctx context.Context, snapshot string) error
}

// Kube is the Kubernetes side of the migration.
type Kube interface {
	StampDatasetProvisioned(ctx context.Context, name string, at time.Time) error
	StampSnapshotProvisioned(ctx context.Context, name string, at time.Time) error
	SetSnapshotSourceType(ctx context.Context, name string, t storagev1alpha1.DatasetType) error
}

// Input is the cluster state the plan is derived from.
type Input struct {
	// Pools maps a pool GUID to the current ZFS pool name.
	Pools     map[string]string
	Datasets  []storagev1alpha1.ZfsDataset
	Snapshots []storagev1alpha1.ZfsSnapshot
}

// Action kinds.
const (
	ActSet                = "set"
	ActDestroyDeferred    = "destroy-deferred"
	ActStampDataset       = "stamp-dataset-provisioned"
	ActStampSnapshot      = "stamp-snapshot-provisioned"
	ActSnapshotSourceType = "set-snapshot-source-type"
)

// Action is one change the migration would make.
type Action struct {
	Kind   string
	Target string // ZFS name, or CR name
	Prop   string
	Value  string
	At     time.Time
	Reason string
}

func (a Action) String() string {
	switch a.Kind {
	case ActSet:
		return fmt.Sprintf("zfs set %s=%s %s    # %s", a.Prop, a.Value, a.Target, a.Reason)
	case ActDestroyDeferred:
		return fmt.Sprintf("zfs destroy -d %s    # %s", a.Target, a.Reason)
	case ActStampDataset:
		return fmt.Sprintf("ZfsDataset %s: status.provisionedAt=%s    # %s", a.Target, a.At.UTC().Format(time.RFC3339), a.Reason)
	case ActStampSnapshot:
		return fmt.Sprintf("ZfsSnapshot %s: status.provisionedAt=%s    # %s", a.Target, a.At.UTC().Format(time.RFC3339), a.Reason)
	default:
		return fmt.Sprintf("ZfsSnapshot %s: spec.sourceType=%s    # %s", a.Target, a.Value, a.Reason)
	}
}

// Finding severities.
const (
	// Blocker findings must be resolved by a human before the new driver is deployed.
	Blocker = "blocker"
	// Info findings are reported only.
	Info = "info"
)

// Finding is something the migration found but will not change.
type Finding struct {
	Severity string
	Subject  string
	Message  string
}

// Plan is the result of an audit.
type Plan struct {
	Actions  []Action
	Findings []Finding
	// Labelled is the number of objects that already carry the right labels.
	Labelled int
	// Marked is the number of old restore-source snapshots already marked for deferred destroy.
	Marked int
}

// Blockers returns the blocker findings.
func (p *Plan) Blockers() []Finding {
	var out []Finding
	for _, f := range p.Findings {
		if f.Severity == Blocker {
			out = append(out, f)
		}
	}
	return out
}

// Clean reports whether nothing is left to do and nothing blocks.
func (p *Plan) Clean() bool { return len(p.Actions) == 0 && len(p.Blockers()) == 0 }

type claim struct{ role, owner, reason string }

// Compute audits the cluster and the pool and returns what has to change.
func Compute(ctx context.Context, in Input, be Backend, now time.Time) (*Plan, error) {
	p := &Plan{}
	finding := func(sev, subject, format string, a ...any) {
		p.Findings = append(p.Findings, Finding{sev, subject, fmt.Sprintf(format, a...)})
	}

	poolOf := func(guid string) (string, bool) { n, ok := in.Pools[guid]; return n, ok && n != "" }
	datasetCR := map[string]*storagev1alpha1.ZfsDataset{}
	for i := range in.Datasets {
		datasetCR[in.Datasets[i].Name] = &in.Datasets[i]
	}

	// Roots to list: the parent of every dataset and every snapshot's source.
	roots := map[string]bool{}
	for i := range in.Datasets {
		d := &in.Datasets[i]
		pool, ok := poolOf(d.Spec.PoolGUID)
		if !ok {
			finding(Blocker, "ZfsDataset "+d.Name, "pool %s has no known name", d.Spec.PoolGUID)
			continue
		}
		roots[parentOf(pool, d.Spec.Dataset)] = true
	}
	for i := range in.Snapshots {
		s := &in.Snapshots[i]
		pool, ok := poolOf(s.Spec.PoolGUID)
		if !ok {
			finding(Blocker, "ZfsSnapshot "+s.Name, "pool %s has no known name", s.Spec.PoolGUID)
			continue
		}
		roots[parentOf(pool, s.Spec.Dataset)] = true
	}
	rootList := sortedKeys(roots)
	objs, err := be.List(ctx, rootList)
	if err != nil {
		return nil, err
	}
	byName := map[string]Obj{}
	snapsByShort := map[string][]Obj{}
	for _, o := range objs {
		byName[o.Name] = o
		if o.IsSnapshot() {
			snapsByShort[o.Short()] = append(snapsByShort[o.Short()], o)
		}
	}

	claims := map[string]claim{}
	add := func(name, role, owner, reason string) {
		if prev, ok := claims[name]; ok && (prev.role != role || prev.owner != owner) {
			finding(Blocker, name, "claimed twice: %s by %s and %s by %s", prev.role, prev.owner, role, owner)
			return
		}
		claims[name] = claim{role, owner, reason}
	}

	for i := range in.Datasets {
		d := &in.Datasets[i]
		pool, ok := poolOf(d.Spec.PoolGUID)
		if !ok {
			continue
		}
		full := pool + "/" + strings.Trim(d.Spec.Dataset, "/")
		if _, ok := byName[full]; !ok {
			if d.Status.Phase == storagev1alpha1.DatasetPhaseReady || d.Status.Phase == storagev1alpha1.DatasetPhaseLost {
				finding(Info, "ZfsDataset "+d.Name, "dataset %s does not exist", full)
			}
			continue
		}
		add(full, zpool.RoleVolume, string(d.UID), "volume of ZfsDataset "+d.Name)
		for _, o := range snapsByShort["clone-"+d.Name] {
			add(o.Name, zpool.RoleCloneOrigin, string(d.UID), "clone origin of ZfsDataset "+d.Name)
		}
		if d.Status.ProvisionedAt == nil && settled(string(d.Status.Phase), string(storagev1alpha1.DatasetPhaseReady), string(storagev1alpha1.DatasetPhaseLost)) {
			p.Actions = append(p.Actions, Action{Kind: ActStampDataset, Target: d.Name, At: provisionedTime(d.Status.Conditions, now), Reason: "predates status.provisionedAt"})
		}
	}

	for i := range in.Snapshots {
		s := &in.Snapshots[i]
		pool, ok := poolOf(s.Spec.PoolGUID)
		if !ok {
			continue
		}
		subject := "ZfsSnapshot " + s.Name
		srcPath := s.Spec.Dataset
		if cr := datasetCR[s.Spec.SourceVolume]; cr != nil && s.Spec.SourceVolume != "" {
			srcPath = cr.Spec.Dataset
		}
		switch raws := snapsByShort[s.Spec.SnapshotName]; len(raws) {
		case 1:
			add(raws[0].Name, zpool.RoleRaw, string(s.UID), "raw snapshot of ZfsSnapshot "+s.Name)
		case 0:
			finding(Info, subject, "raw snapshot %q not found", s.Spec.SnapshotName)
		default:
			finding(Blocker, subject, "raw snapshot name %q is ambiguous: %s", s.Spec.SnapshotName, names(raws))
		}
		backing := pool + "/" + path.Join(path.Dir(strings.Trim(srcPath, "/")), s.Spec.SnapshotName)
		if _, ok := byName[backing]; ok {
			add(backing, zpool.RoleBacking, string(s.UID), "backing clone of ZfsSnapshot "+s.Name)
		} else {
			finding(Info, subject, "backing clone %s not found", backing)
		}
		if s.Status.ProvisionedAt == nil && settled(string(s.Status.Phase), string(storagev1alpha1.SnapshotPhaseReady), string(storagev1alpha1.SnapshotPhaseLost)) {
			p.Actions = append(p.Actions, Action{Kind: ActStampSnapshot, Target: s.Name, At: provisionedTime(s.Status.Conditions, now), Reason: "predates status.provisionedAt"})
		}
		if s.Spec.SourceType == "" {
			if cr := datasetCR[s.Spec.SourceVolume]; cr != nil && s.Spec.SourceVolume != "" {
				p.Actions = append(p.Actions, Action{Kind: ActSnapshotSourceType, Target: s.Name, Value: string(cr.Spec.Type), Reason: "copied from ZfsDataset " + cr.Name})
			} else {
				finding(Blocker, subject, "spec.sourceType is empty and the source ZfsDataset %q is gone; set it by hand", s.Spec.SourceVolume)
			}
		}
	}

	// Old self-snapshots. Only those on a dataset the migration itself owns are
	// touched, so a snapshot with the same name elsewhere is never destroyed.
	for _, o := range snapsByShort[RestoreSourceSnapshot] {
		c, owned := claims[o.Dataset()]
		switch {
		case !owned || (c.role != zpool.RoleBacking && c.role != zpool.RoleVolume):
			finding(Blocker, o.Name, "old restore-source snapshot on a dataset the migration does not own")
		case o.Deferred:
			p.Marked++
		default:
			p.Actions = append(p.Actions, Action{Kind: ActDestroyDeferred, Target: o.Name, Reason: "old restore-source snapshot"})
		}
	}

	names := make([]string, 0, len(claims))
	for n := range claims {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c, o := claims[n], byName[n]
		ok := true
		for _, l := range [][3]string{{zpool.PropRole, o.Role, c.role}, {zpool.PropOwner, o.Owner, c.owner}} {
			switch {
			case l[1] == l[2]:
			case l[1] != "":
				finding(Blocker, n, "label %s is %q, derived %q", l[0], l[1], l[2])
				ok = false
			default:
				p.Actions = append(p.Actions, Action{Kind: ActSet, Target: n, Prop: l[0], Value: l[2], Reason: c.reason})
				ok = false
			}
		}
		if ok {
			p.Labelled++
		}
	}

	rootSet := map[string]bool{}
	for _, r := range rootList {
		rootSet[r] = true
	}
	for _, o := range objs {
		if _, ok := claims[o.Name]; ok || rootSet[o.Name] || o.Short() == RestoreSourceSnapshot {
			continue
		}
		if o.Role != "" || isDriverSnapshot(o) {
			finding(Info, o.Name, "no CR owns this object (role %q, owner %q)", o.Role, o.Owner)
		}
	}
	return p, nil
}

// Apply runs the plan.
func Apply(ctx context.Context, p *Plan, be Backend, k Kube) error {
	for _, a := range p.Actions {
		var err error
		switch a.Kind {
		case ActSet:
			err = be.Set(ctx, a.Target, a.Prop, a.Value)
		case ActDestroyDeferred:
			err = be.DestroyDeferred(ctx, a.Target)
		case ActStampDataset:
			err = k.StampDatasetProvisioned(ctx, a.Target, a.At)
		case ActStampSnapshot:
			err = k.StampSnapshotProvisioned(ctx, a.Target, a.At)
		case ActSnapshotSourceType:
			err = k.SetSnapshotSourceType(ctx, a.Target, storagev1alpha1.DatasetType(a.Value))
		}
		if err != nil {
			return fmt.Errorf("%s: %w", a, err)
		}
	}
	return nil
}

func isDriverSnapshot(o Obj) bool {
	s := o.Short()
	return strings.HasPrefix(s, "csi-snap-") || strings.HasPrefix(s, "clone-")
}

func settled(phase string, states ...string) bool {
	for _, s := range states {
		if phase == s {
			return true
		}
	}
	return false
}

// provisionedTime is the time a settled object is stamped with: the Ready
// condition's transition time, which for a continuously Ready object is the
// real first-Ready time, otherwise now.
func provisionedTime(conds []metav1.Condition, now time.Time) time.Time {
	for _, c := range conds {
		if c.Type == "Ready" && c.Status == metav1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time
		}
	}
	return now
}

func parentOf(pool, dataset string) string {
	dir := path.Dir(strings.Trim(dataset, "/"))
	if dir == "." {
		return pool
	}
	return pool + "/" + dir
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func names(os []Obj) string {
	var n []string
	for _, o := range os {
		n = append(n, o.Name)
	}
	return strings.Join(n, ", ")
}

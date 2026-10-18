// Package memzfs is an in-memory implementation of zpool.ZFS. It models the
// parts of ZFS the driver depends on (datasets and zvols, snapshots, clones,
// promote, deferred destroy, user properties) as one state machine, with the
// error texts of real ZFS. Every behaviour here is pinned to a real pool by the
// conformance suite in package zfsconf, which runs against both this model and
// `zfs`; a difference there is a bug in the model.
package memzfs

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

type snapshot struct {
	name     string
	txg      int64
	props    map[string]string
	deferred bool
}

type dataset struct {
	name    string
	kind    zpool.DatasetKind
	props   map[string]string
	origin  string // full snapshot name, "" when independent
	snaps   map[string]*snapshot
	volsize int64
}

type injection struct {
	op    string
	name  string // "" matches any
	err   error
	times int
}

// Mem is the in-memory ZFS. The zero value is not usable; use New.
type Mem struct {
	mu   sync.Mutex
	ds   map[string]*dataset
	txg  int64
	inj  []*injection
	log  []string
	busy int // destroys of a fresh zvol that fail with "dataset is busy"
	// freshZvol holds zvols that were created and not yet destroyed once.
	freshZvol map[string]bool
	owner     []string
	scans     int
}

var _ zpool.ZFS = (*Mem)(nil)

// New returns a Mem holding one (empty) root dataset per pool name.
func New(pools ...string) *Mem {
	m := &Mem{ds: map[string]*dataset{}, freshZvol: map[string]bool{}}
	for _, p := range pools {
		m.ds[p] = &dataset{name: p, kind: zpool.KindFilesystem, props: map[string]string{}, snaps: map[string]*snapshot{}}
	}
	return m
}

// Log returns the mutating calls so far, e.g. "destroy -d pool/a@s".
func (m *Mem) Log() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.log...)
}

// Scans returns how many pool-wide snapshot scans (FindSnapshot) were made, so
// tests can assert that a cheaper lookup was enough.
func (m *Mem) Scans() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.scans
}

// Inject makes the next `times` calls of op (create, snapshot, clone, destroy,
// destroy-d, promote, set) on name (or any name when empty) fail with err.
func (m *Mem) Inject(op, name string, err error, times int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inj = append(m.inj, &injection{op: op, name: name, err: err, times: times})
}

// BusyAfterZvolCreate makes the first n destroys of each freshly created zvol
// fail with ZFS's "dataset is busy", which real ZFS does about 40% of the time
// right after creation.
func (m *Mem) BusyAfterZvolCreate(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.busy = n
}

func (m *Mem) injected(op, name string) error {
	for _, i := range m.inj {
		if i.times > 0 && i.op == op && (i.name == "" || i.name == name) {
			i.times--
			return i.err
		}
	}
	return nil
}

func (m *Mem) record(format string, a ...any) { m.log = append(m.log, fmt.Sprintf(format, a...)) }

func split(name string) (ds, snap string, isSnap bool) {
	if i := strings.Index(name, "@"); i >= 0 {
		return name[:i], name[i+1:], true
	}
	return name, "", false
}

func parentOf(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[:i]
	}
	return ""
}

func notExist(name string) error { return fmt.Errorf("cannot open '%s': dataset does not exist", name) }

func (m *Mem) clonesOf(snap string) []string {
	var out []string
	for n, d := range m.ds {
		if d.origin == snap {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (m *Mem) children(name string) []string {
	var out []string
	for n := range m.ds {
		if strings.HasPrefix(n, name+"/") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func (m *Mem) snapNames(d *dataset) []string {
	var out []string
	for _, s := range m.sortedSnaps(d) {
		out = append(out, d.name+"@"+s.name)
	}
	return out
}

func (m *Mem) sortedSnaps(d *dataset) []*snapshot {
	out := make([]*snapshot, 0, len(d.snaps))
	for _, s := range d.snaps {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].txg != out[j].txg {
			return out[i].txg < out[j].txg
		}
		return out[i].name < out[j].name
	})
	return out
}

// dropIfDone removes a snapshot that is marked for deferred destroy once
// nothing clones it any more.
func (m *Mem) dropIfDone(full string) {
	dsn, sn, ok := split(full)
	if !ok {
		return
	}
	d := m.ds[dsn]
	if d == nil {
		return
	}
	if s := d.snaps[sn]; s != nil && s.deferred && len(m.clonesOf(full)) == 0 {
		delete(d.snaps, sn)
	}
}

func (m *Mem) create(name string, kind zpool.DatasetKind, size int64, props map[string]string) error {
	if err := m.injected("create", name); err != nil {
		return err
	}
	if _, ok := m.ds[name]; ok {
		return fmt.Errorf("cannot create '%s': dataset already exists", name)
	}
	if p := parentOf(name); p == "" || m.ds[p] == nil {
		return fmt.Errorf("cannot create '%s': parent does not exist", name)
	}
	d := &dataset{name: name, kind: kind, props: copyProps(props), snaps: map[string]*snapshot{}, volsize: size}
	m.ds[name] = d
	if kind == zpool.KindVolume {
		m.freshZvol[name] = true
	}
	return nil
}

func copyProps(p map[string]string) map[string]string {
	out := make(map[string]string, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// CreateDataset implements zpool.ZFS.
func (m *Mem) CreateDataset(_ context.Context, name string, props map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("create %s", name)
	return m.create(name, zpool.KindFilesystem, 0, props)
}

// CreateZvol implements zpool.ZFS.
func (m *Mem) CreateZvol(_ context.Context, name string, size int64, props map[string]string) error {
	if size <= 0 {
		return fmt.Errorf("zvol size must be > 0, got %d", size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("create -V %d %s", size, name)
	return m.create(name, zpool.KindVolume, size, props)
}

// Snapshot implements zpool.ZFS: all names are created in one transaction group
// or none are.
func (m *Mem) Snapshot(_ context.Context, names ...string) error {
	if len(names) == 0 {
		return fmt.Errorf("snapshot name is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("snapshot %s", strings.Join(names, " "))
	for _, n := range names {
		if _, _, ok := split(n); !ok {
			return fmt.Errorf("snapshot name %q must be of the form pool/dataset@snap", n)
		}
	}
	if err := m.injected("snapshot", names[0]); err != nil {
		return err
	}
	existing := 0
	for _, n := range names {
		dn, sn, _ := split(n)
		d := m.ds[dn]
		if d == nil {
			return notExist(dn)
		}
		if d.snaps[sn] != nil {
			existing++
		}
	}
	if existing > 0 {
		if existing == len(names) || len(names) == 1 {
			return nil
		}
		return fmt.Errorf("%w", zpool.ErrSnapshotsPartiallyExist)
	}
	m.txg++
	for _, n := range names {
		dn, sn, _ := split(n)
		m.ds[dn].snaps[sn] = &snapshot{name: sn, txg: m.txg, props: map[string]string{}}
	}
	return nil
}

// Clone implements zpool.ZFS (idempotent on an existing dest).
func (m *Mem) Clone(_ context.Context, snap, dest string, props map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("clone %s %s", snap, dest)
	dn, sn, ok := split(snap)
	if !ok {
		return fmt.Errorf("clone source %q must be a snapshot (pool/dataset@snap)", snap)
	}
	if err := m.injected("clone", dest); err != nil {
		return err
	}
	src := m.ds[dn]
	if src == nil || src.snaps[sn] == nil {
		return notExist(snap)
	}
	if m.ds[dest] != nil {
		return nil
	}
	if err := m.create(dest, src.kind, src.volsize, props); err != nil {
		return err
	}
	m.ds[dest].origin = snap
	return nil
}

// Promote implements zpool.ZFS: one promote moves the origin snapshot and all
// older snapshots of the parent to the clone.
func (m *Mem) Promote(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.ds[name]
	if d == nil {
		return fmt.Errorf("%w: %s", zpool.ErrNotExist, name)
	}
	if d.origin == "" {
		return nil
	}
	m.record("promote %s", name)
	if err := m.injected("promote", name); err != nil {
		return fmt.Errorf("zfs promote %s (attempt 1/20): %w", name, err)
	}
	pn, osn, _ := split(d.origin)
	p := m.ds[pn]
	o := p.snaps[osn]
	var moved []*snapshot
	for _, s := range m.sortedSnaps(p) {
		if s.txg <= o.txg {
			moved = append(moved, s)
		}
	}
	for _, s := range moved {
		if d.snaps[s.name] != nil {
			return fmt.Errorf("zfs promote %s (attempt 1/20): cannot promote '%s': conflicting snapshot '%s' from parent '%s@%s'",
				name, name, s.name, pn, osn)
		}
	}
	for _, s := range moved {
		delete(p.snaps, s.name)
		d.snaps[s.name] = s
		for _, other := range m.ds {
			if other.origin == pn+"@"+s.name {
				other.origin = name + "@" + s.name
			}
		}
	}
	d.origin, p.origin = p.origin, name+"@"+osn
	return nil
}

// Destroy implements zpool.ZFS for datasets, zvols and snapshots.
func (m *Mem) Destroy(_ context.Context, name string, recursive bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("destroy %s", name)
	if err := m.injected("destroy", name); err != nil {
		return err
	}
	if dn, sn, ok := split(name); ok {
		d := m.ds[dn]
		if d == nil || d.snaps[sn] == nil {
			return nil // the CLI maps both "does not exist" texts to success
		}
		if c := m.clonesOf(name); len(c) > 0 {
			return fmt.Errorf("cannot destroy '%s': snapshot has dependent clones\nuse '-R' to destroy the following datasets:\n%s", name, strings.Join(c, "\n"))
		}
		delete(d.snaps, sn)
		return nil
	}
	d := m.ds[name]
	if d == nil {
		return nil
	}
	if d.kind == zpool.KindVolume && m.freshZvol[name] {
		delete(m.freshZvol, name)
		if m.busy > 0 {
			m.busy--
			return fmt.Errorf("cannot destroy '%s': dataset is busy", name)
		}
	}
	kind := "filesystem"
	if d.kind == zpool.KindVolume {
		kind = "volume"
	}
	children := m.children(name)
	if !recursive && (len(children) > 0 || len(d.snaps) > 0) {
		list := append(append([]string{}, children...), m.snapNames(d)...)
		return fmt.Errorf("cannot destroy '%s': %s has children\nuse '-r' to destroy the following datasets:\n%s", name, kind, strings.Join(list, "\n"))
	}
	tree := append([]string{name}, children...)
	in := map[string]bool{}
	for _, t := range tree {
		in[t] = true
	}
	var outside []string
	for _, t := range tree {
		for _, sn := range m.snapNames(m.ds[t]) {
			for _, c := range m.clonesOf(sn) {
				if !in[c] {
					outside = append(outside, c)
				}
			}
		}
	}
	if len(outside) > 0 {
		return fmt.Errorf("cannot destroy '%s': %s has dependent clones\nuse '-R' to destroy the following datasets:\n%s", name, kind, strings.Join(outside, "\n"))
	}
	var origins []string
	for _, t := range tree {
		if o := m.ds[t].origin; o != "" {
			origins = append(origins, o)
		}
		delete(m.ds, t)
		delete(m.freshZvol, t)
	}
	for _, o := range origins {
		m.dropIfDone(o)
	}
	return nil
}

// DestroySnapshotDeferred implements zpool.ZFS (`zfs destroy -d`).
func (m *Mem) DestroySnapshotDeferred(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("destroy -d %s", name)
	dn, sn, ok := split(name)
	if !ok {
		return fmt.Errorf("snapshot name %q must be of the form pool/dataset@snap", name)
	}
	if err := m.injected("destroy-d", name); err != nil {
		return err
	}
	d := m.ds[dn]
	if d == nil || d.snaps[sn] == nil {
		return nil
	}
	if len(m.clonesOf(name)) == 0 {
		delete(d.snaps, sn)
		return nil
	}
	d.snaps[sn].deferred = true
	return nil
}

// ListSnapshots implements zpool.ZFS.
func (m *Mem) ListSnapshots(_ context.Context, dataset string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.ds[dataset]
	if d == nil {
		return nil, fmt.Errorf("%w: %s", zpool.ErrNotExist, dataset)
	}
	return m.snapNames(d), nil
}

// Clones implements zpool.ZFS.
func (m *Mem) Clones(_ context.Context, snap string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dn, sn, ok := split(snap)
	if !ok {
		return nil, fmt.Errorf("clones requires a snapshot name (pool/dataset@snap), got %q", snap)
	}
	if d := m.ds[dn]; d == nil || d.snaps[sn] == nil {
		return nil, fmt.Errorf("%w: %s", zpool.ErrNotExist, snap)
	}
	return m.clonesOf(snap), nil
}

// FindSnapshot implements zpool.ZFS.
func (m *Mem) FindSnapshot(_ context.Context, pool, suffix string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scans++
	names := make([]string, 0, len(m.ds))
	for n := range m.ds {
		if n == pool || strings.HasPrefix(n, pool+"/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if m.ds[n].snaps[suffix] != nil {
			return n + "@" + suffix, nil
		}
	}
	return "", nil
}

func isUser(prop string) bool { return strings.Contains(prop, ":") }

// inherited resolves a user property: local value, else the nearest ancestor's.
func (m *Mem) inherited(dsName, prop string) string {
	for n := dsName; n != ""; n = parentOf(n) {
		if d := m.ds[n]; d != nil {
			if v, ok := d.props[prop]; ok {
				return v
			}
		}
	}
	return "-"
}

// Get implements zpool.ZFS.
func (m *Mem) Get(_ context.Context, name, prop string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dn, sn, isSnap := split(name)
	d := m.ds[dn]
	if d == nil || (isSnap && d.snaps[sn] == nil) {
		return "", fmt.Errorf("%w: %s", zpool.ErrNotExist, name)
	}
	if isSnap {
		s := d.snaps[sn]
		switch prop {
		case "type":
			return "snapshot", nil
		case "clones":
			if c := m.clonesOf(name); len(c) > 0 {
				return strings.Join(c, ","), nil
			}
			return "-", nil
		case "defer_destroy":
			if s.deferred {
				return "on", nil
			}
			return "off", nil
		case "createtxg":
			return strconv.FormatInt(s.txg, 10), nil
		case "origin":
			return "-", nil
		}
		if isUser(prop) {
			if v, ok := s.props[prop]; ok {
				return v, nil
			}
			return m.inherited(dn, prop), nil
		}
		return "-", nil
	}
	switch prop {
	case "type":
		return string(d.kind), nil
	case "origin":
		if d.origin == "" {
			return "-", nil
		}
		return d.origin, nil
	case "clones", "defer_destroy":
		return "-", nil
	case "volsize":
		if d.kind == zpool.KindVolume {
			return strconv.FormatInt(d.volsize, 10), nil
		}
		return "-", nil
	}
	if isUser(prop) {
		return m.inherited(dn, prop), nil
	}
	if v, ok := d.props[prop]; ok {
		return v, nil
	}
	return "-", nil
}

// SetProperty implements zpool.ZFS. Snapshots accept user properties only.
func (m *Mem) SetProperty(_ context.Context, name, prop, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.record("set %s=%s %s", prop, value, name)
	if err := m.injected("set", name); err != nil {
		return err
	}
	dn, sn, isSnap := split(name)
	d := m.ds[dn]
	if d == nil || (isSnap && d.snaps[sn] == nil) {
		return notExist(name)
	}
	switch prop {
	case "origin", "clones", "type", "defer_destroy", "createtxg":
		return fmt.Errorf("cannot set property for '%s': '%s' is readonly", name, prop)
	}
	if isSnap {
		if !isUser(prop) {
			return fmt.Errorf("cannot set property for '%s': this property can not be modified for snapshots", name)
		}
		d.snaps[sn].props[prop] = value
		return nil
	}
	if prop == "volsize" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("cannot set property for '%s': invalid volsize", name)
		}
		d.volsize = n
	}
	d.props[prop] = value
	return nil
}

// List implements zpool.ZFS (datasets and zvols, never snapshots).
func (m *Mem) List(_ context.Context, kind zpool.DatasetKind) ([]zpool.Dataset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.ds))
	for n := range m.ds {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []zpool.Dataset
	for _, n := range names {
		d := m.ds[n]
		if kind != zpool.KindAll && d.kind != kind {
			continue
		}
		mp := ""
		if d.kind == zpool.KindFilesystem {
			mp = d.props["mountpoint"]
			if mp == "" {
				mp = "/" + n
			}
			if mp == "none" || mp == "legacy" {
				mp = ""
			}
		}
		out = append(out, zpool.Dataset{Name: n, Type: d.kind, Mountpoint: mp})
	}
	return out, nil
}

// ApplyOwnership implements zpool.ZFS by recording the call.
func (m *Mem) ApplyOwnership(_ context.Context, mountpoint string, uid, gid *int64, mode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.owner = append(m.owner, mountpoint)
	return nil
}

// Check verifies the invariants that must hold after every operation.
func (m *Mem) Check() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, d := range m.ds {
		if p := parentOf(n); p != "" && m.ds[p] == nil {
			return fmt.Errorf("%s has no parent %s", n, p)
		}
		if d.origin != "" {
			dn, sn, _ := split(d.origin)
			if od := m.ds[dn]; od == nil || od.snaps[sn] == nil {
				return fmt.Errorf("%s has dangling origin %s", n, d.origin)
			}
		}
		for _, s := range d.snaps {
			if s.deferred && len(m.clonesOf(n+"@"+s.name)) == 0 {
				return fmt.Errorf("%s@%s is marked for deferred destroy but has no clones", n, s.name)
			}
		}
		seen := map[string]bool{n: true}
		for o := d.origin; o != ""; {
			on, _, _ := split(o)
			if seen[on] {
				return fmt.Errorf("origin cycle at %s", on)
			}
			seen[on] = true
			o = m.ds[on].origin
		}
	}
	return nil
}

// Datasets returns the full names of every dataset, sorted.
func (m *Mem) Datasets() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.ds))
	for n := range m.ds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Snapshots returns the full names of every snapshot on the pool, sorted.
func (m *Mem) Snapshots() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for n, d := range m.ds {
		for s := range d.snaps {
			out = append(out, n+"@"+s)
		}
	}
	sort.Strings(out)
	return out
}

// Deferred reports whether the snapshot is marked for deferred destruction.
func (m *Mem) Deferred(full string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	dn, sn, ok := split(full)
	if !ok || m.ds[dn] == nil || m.ds[dn].snaps[sn] == nil {
		return false
	}
	return m.ds[dn].snaps[sn].deferred
}

package zpool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ErrNotExist is returned (wrapped) by Get when the dataset or zvol does not
// exist, letting callers implement idempotent create/delete via errors.Is.
var ErrNotExist = errors.New("zfs: dataset does not exist")

// DatasetKind selects filesystem datasets, block zvols, or both.
type DatasetKind string

const (
	// KindFilesystem is a POSIX filesystem dataset.
	KindFilesystem DatasetKind = "filesystem"
	// KindVolume is a block zvol.
	KindVolume DatasetKind = "volume"
	// KindAll matches both datasets and zvols.
	KindAll DatasetKind = "all"
)

// Dataset is a single ZFS object as reported by `zfs list`.
type Dataset struct {
	// Name is the full ZFS name, e.g. "tank/k8s/pvc-123".
	Name string
	// Type is the object kind (filesystem or volume).
	Type DatasetKind
	// Mountpoint is the filesystem mountpoint; empty for zvols or
	// none/legacy mounts.
	Mountpoint string
}

// ZFS is the subset of ZFS operations the storage agent needs to fulfil
// ZfsDataset allocations. It is an interface so the agent reconciler can be
// unit-tested against a fake, while the real implementation shells out to the
// host `zfs` binary through the same HostExec redirection as pool discovery.
type ZFS interface {
	// CreateDataset creates a filesystem dataset with optional ZFS properties.
	CreateDataset(ctx context.Context, name string, props map[string]string) error
	// CreateZvol creates a block zvol of the given logical size in bytes.
	CreateZvol(ctx context.Context, name string, sizeBytes int64, props map[string]string) error
	// Snapshot creates the snapshot named by the full ZFS snapshot name
	// "pool/dataset@snap". It is idempotent: an already-existing snapshot is not
	// an error.
	Snapshot(ctx context.Context, name string) error
	// Clone creates dest as a clone of the snapshot (both full ZFS names),
	// applying optional settable properties. Idempotent: an already-existing dest
	// is not an error.
	Clone(ctx context.Context, snapshot, dest string, props map[string]string) error
	// Destroy removes a dataset/zvol. It is idempotent: destroying a
	// non-existent object is not an error. recursive also destroys children.
	Destroy(ctx context.Context, name string, recursive bool) error
	// Get returns a single ZFS property value. It wraps ErrNotExist when the
	// object does not exist.
	Get(ctx context.Context, name, property string) (string, error)
	// SetProperty sets a single ZFS property on an existing object, e.g.
	// refquota (filesystem) or volsize (zvol). It backs volume expansion.
	SetProperty(ctx context.Context, name, property, value string) error
	// List enumerates datasets/zvols of the given kind.
	List(ctx context.Context, kind DatasetKind) ([]Dataset, error)
}

// CLI is the host-backed ZFS implementation. Run defaults to a real exec runner;
// wrap it with HostExec.BuildRunner to redirect to the host's version-matched
// zfs binary (chroot /host or nsenter).
type CLI struct {
	// Bin is the zfs binary, default "zfs" (resolved on PATH / by HostExec).
	Bin string
	// Run executes commands; defaults to a real exec runner.
	Run Runner
}

// NewZFS returns a CLI using the given runner (nil uses a real exec runner).
func NewZFS(run Runner) *CLI {
	return &CLI{Bin: "zfs", Run: run}
}

func (z *CLI) run(ctx context.Context, args ...string) (string, error) {
	bin := z.Bin
	if bin == "" {
		bin = "zfs"
	}
	run := z.Run
	if run == nil {
		run = execRunner
	}
	return run(ctx, bin, args...)
}

// CreateDataset creates a filesystem dataset with optional properties.
func (z *CLI) CreateDataset(ctx context.Context, name string, props map[string]string) error {
	if name == "" {
		return fmt.Errorf("dataset name is empty")
	}
	args := append([]string{"create"}, propArgs(props)...)
	args = append(args, name)
	_, err := z.run(ctx, args...)
	return err
}

// CreateZvol creates a block zvol of sizeBytes with optional properties.
func (z *CLI) CreateZvol(ctx context.Context, name string, sizeBytes int64, props map[string]string) error {
	if name == "" {
		return fmt.Errorf("zvol name is empty")
	}
	if sizeBytes <= 0 {
		return fmt.Errorf("zvol size must be > 0, got %d", sizeBytes)
	}
	args := []string{"create", "-V", strconv.FormatInt(sizeBytes, 10)}
	args = append(args, propArgs(props)...)
	args = append(args, name)
	_, err := z.run(ctx, args...)
	return err
}

// Destroy removes a dataset/zvol, treating a missing object as success.
func (z *CLI) Destroy(ctx context.Context, name string, recursive bool) error {
	if name == "" {
		return fmt.Errorf("dataset name is empty")
	}
	args := []string{"destroy"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, name)
	_, err := z.run(ctx, args...)
	if err != nil && isNotExist(err) {
		return nil
	}
	return err
}

// Snapshot creates the snapshot "pool/dataset@snap", treating an already-existing
// snapshot as success (idempotent).
func (z *CLI) Snapshot(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("snapshot name is empty")
	}
	if !strings.Contains(name, "@") {
		return fmt.Errorf("snapshot name %q must be of the form pool/dataset@snap", name)
	}
	_, err := z.run(ctx, "snapshot", name)
	if err != nil && isExists(err) {
		return nil
	}
	return err
}

// Clone creates dest as a clone of snapshot, treating an already-existing dest as
// success (idempotent). Only settable properties should be passed (a clone
// inherits read-only properties such as volblocksize from its origin).
func (z *CLI) Clone(ctx context.Context, snapshot, dest string, props map[string]string) error {
	if snapshot == "" || dest == "" {
		return fmt.Errorf("clone requires both snapshot and dest names")
	}
	if !strings.Contains(snapshot, "@") {
		return fmt.Errorf("clone source %q must be a snapshot (pool/dataset@snap)", snapshot)
	}
	args := append([]string{"clone"}, propArgs(props)...)
	args = append(args, snapshot, dest)
	_, err := z.run(ctx, args...)
	if err != nil && isExists(err) {
		return nil
	}
	return err
}

// Get returns a single ZFS property value, wrapping ErrNotExist when missing.
func (z *CLI) Get(ctx context.Context, name, property string) (string, error) {
	out, err := z.run(ctx, "get", "-H", "-p", "-o", "value", property, name)
	if err != nil {
		if isNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotExist, name)
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// SetProperty sets a single ZFS property on an existing dataset/zvol.
func (z *CLI) SetProperty(ctx context.Context, name, property, value string) error {
	if name == "" {
		return fmt.Errorf("dataset name is empty")
	}
	if property == "" {
		return fmt.Errorf("property is empty")
	}
	_, err := z.run(ctx, "set", property+"="+value, name)
	return err
}

// List enumerates datasets/zvols of the given kind.
func (z *CLI) List(ctx context.Context, kind DatasetKind) ([]Dataset, error) {
	t := string(kind)
	if t == "" {
		t = string(KindAll)
	}
	out, err := z.run(ctx, "list", "-H", "-p", "-o", "name,type,mountpoint", "-t", t)
	if err != nil {
		return nil, err
	}

	var datasets []Dataset
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		d := Dataset{Name: fields[0], Type: DatasetKind(fields[1])}
		if len(fields) >= 3 {
			d.Mountpoint = normalizeMountpoint(fields[2])
		}
		datasets = append(datasets, d)
	}
	return datasets, nil
}

// normalizeMountpoint maps ZFS's non-path mountpoint values ("none", "legacy",
// "-", empty) to an empty string, leaving real paths untouched.
func normalizeMountpoint(mp string) string {
	switch strings.TrimSpace(mp) {
	case "", "-", "none", "legacy":
		return ""
	default:
		return strings.TrimSpace(mp)
	}
}

// propArgs renders a stable, sorted list of "-o key=value" arguments.
func propArgs(props map[string]string) []string {
	if len(props) == 0 {
		return nil
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		out = append(out, "-o", k+"="+props[k])
	}
	return out
}

// isNotExist reports whether a CLI error is a ZFS "does not exist" failure.
func isNotExist(err error) bool {
	return err != nil && strings.Contains(err.Error(), "does not exist")
}

// isExists reports whether a CLI error is a ZFS "already exists" failure, letting
// snapshot creation be idempotent.
func isExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

// compile-time assertion that CLI satisfies ZFS.
var _ ZFS = (*CLI)(nil)

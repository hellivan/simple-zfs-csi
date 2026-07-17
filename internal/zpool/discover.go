// Package zpool discovers the ZFS pools imported on the local storage node by
// shelling out to the host `zpool`/`zfs` command-line tools. It is used by the
// per-node discovery DaemonSet (Tier 1) to publish live pool identity, routing
// and health into ZfsPool CRDs.
package zpool

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

// Pool is a single ZFS pool observed on the local node.
type Pool struct {
	// Name is the human-readable pool name, e.g. "tank".
	Name string
	// GUID is the immutable ZFS pool GUID, e.g. "12140134988506841113".
	GUID string
	// Health is the pool availability mapped to the ZfsPool health vocabulary.
	Health storagev1alpha1.ZfsPoolHealth
	// Mountpoint is the pool root's ZFS mountpoint, e.g. "/mnt/tank". Empty when
	// the pool has mountpoint=none or legacy.
	Mountpoint string
}

// Runner executes a command and returns its combined stdout. It is an
// indirection so discovery can be unit-tested without a real ZFS host.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// execRunner runs the command for real, trimming trailing whitespace.
func execRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(out), nil
}

// Discoverer enumerates local pools using the ZFS CLI.
type Discoverer struct {
	// ZpoolBin is the zpool binary, default "zpool" (resolved on PATH).
	ZpoolBin string
	// ZfsBin is the zfs binary, default "zfs" (resolved on PATH).
	ZfsBin string
	// Run executes commands; defaults to a real exec runner.
	Run Runner
}

// NewDiscoverer returns a Discoverer with defaults applied.
func NewDiscoverer() *Discoverer {
	return &Discoverer{ZpoolBin: "zpool", ZfsBin: "zfs", Run: execRunner}
}

// Discover returns all pools currently imported on the node. A pool that cannot
// have its mountpoint resolved is still returned (with an empty Mountpoint) so
// its health is never silently dropped.
func (d *Discoverer) Discover(ctx context.Context) ([]Pool, error) {
	zpoolBin := d.ZpoolBin
	if zpoolBin == "" {
		zpoolBin = "zpool"
	}
	run := d.Run
	if run == nil {
		run = execRunner
	}

	// -H: tab-separated, no headers; -p is not needed. One row per pool.
	out, err := run(ctx, zpoolBin, "list", "-H", "-o", "name,guid,health")
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}

	var pools []Pool
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		p := Pool{
			Name:   fields[0],
			GUID:   fields[1],
			Health: MapHealth(fields[2]),
		}
		if mp, err := d.mountpoint(ctx, run, p.Name); err == nil {
			p.Mountpoint = mp
		}
		pools = append(pools, p)
	}
	return pools, nil
}

// mountpoint resolves the pool root's ZFS mountpoint via the shared ZFS CLI.
// "none"/"legacy"/"-" are normalized to an empty string.
func (d *Discoverer) mountpoint(ctx context.Context, run Runner, pool string) (string, error) {
	zfs := &CLI{Bin: d.ZfsBin, Run: run}
	mp, err := zfs.Get(ctx, pool, "mountpoint")
	if err != nil {
		return "", err
	}
	return normalizeMountpoint(mp), nil
}

// MapHealth translates a ZFS pool state string (from `zpool list -o health` or
// the `zpool status` state line) into the ZfsPool health vocabulary. Unusable
// states (OFFLINE/UNAVAIL/REMOVED) collapse to FAULTED; anything unrecognized
// becomes UNKNOWN so a healthy status is never fabricated on a parse miss.
func MapHealth(state string) storagev1alpha1.ZfsPoolHealth {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "ONLINE":
		return storagev1alpha1.PoolHealthOnline
	case "DEGRADED":
		return storagev1alpha1.PoolHealthDegraded
	case "FAULTED", "OFFLINE", "UNAVAIL", "REMOVED":
		return storagev1alpha1.PoolHealthFaulted
	case "SUSPENDED":
		return storagev1alpha1.PoolHealthSuspended
	default:
		return storagev1alpha1.PoolHealthUnknown
	}
}

// ResourceName returns the ZfsPool metadata.name for a pool GUID: the immutable
// GUID prefixed with "zpool-" to form a valid, collision-free object name.
func ResourceName(guid string) string {
	return "zpool-" + guid
}

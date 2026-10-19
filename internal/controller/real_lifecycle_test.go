//go:build zfsreal

package controller

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// With the zfsreal tag and ZFS_CONF_ROOT set, every TestLifecycle_* runs the real
// reconcilers against a real pool instead of the in-memory one. It never talks
// to Kubernetes (the CRs live in a fake client) and every zfs call is checked
// before it runs: it must name a dataset inside ZFS_CONF_ROOT (a path containing
// "verify_zfs_functions"), and the root itself is never destroyed.
//
//	CGO_ENABLED=0 go test -c -tags zfsreal -o controller.test ./internal/controller
//	ZFS_CONF_ROOT=pool/verify_zfs_functions ./controller.test -test.v -test.run TestLifecycle
func init() {
	root := strings.Trim(os.Getenv("ZFS_CONF_ROOT"), "/")
	if !strings.Contains(root, "verify_zfs_functions") || !strings.Contains(root, "/") {
		return
	}
	realPool = true
	var mu sync.Mutex
	n := 0
	worldFactory = func(t *testing.T) *lifeWorld {
		t.Helper()
		mu.Lock()
		n++
		id := n
		mu.Unlock()
		pool := root[:strings.Index(root, "/")]
		sub := fmt.Sprintf("lc-%d-%d", os.Getpid(), id)
		base := root + "/" + sub
		rp := &realLifePool{root: root, base: base}
		rp.CLI = zpool.NewZFS(rp.guarded)
		ctx := context.Background()
		if err := rp.CreateDataset(ctx, base, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := rp.guarded(ctx, "zfs", "destroy", "-R", base); err != nil {
				t.Logf("cleanup of %s: %v", base, err)
			}
		})
		return buildWorld(t, rp, pool, strings.TrimPrefix(base, pool+"/"))
	}
}

type realLifePool struct {
	*zpool.CLI
	root string
	base string
	mu   sync.Mutex
}

// guarded runs zfs, and only zfs, on names inside the root.
func (p *realLifePool) guarded(ctx context.Context, name string, args ...string) (string, error) {
	if name != "zfs" {
		panic("refusing to run " + name)
	}
	named := 0
	destroy := len(args) > 0 && args[0] == "destroy"
	for _, a := range args {
		if strings.HasPrefix(a, "-") || strings.Contains(a, "=") || !strings.ContainsAny(a, "/@") {
			continue
		}
		target := a
		if i := strings.IndexByte(target, '@'); i >= 0 {
			target = target[:i]
		}
		if target != p.root && !strings.HasPrefix(target, p.root+"/") {
			panic(fmt.Sprintf("refusing zfs %v: %q is outside %s", args, a, p.root))
		}
		if destroy && target == p.root {
			panic(fmt.Sprintf("refusing to destroy the root: zfs %v", args))
		}
		named++
	}
	if named == 0 {
		panic(fmt.Sprintf("refusing zfs %v: no dataset named", args))
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("zfs %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (p *realLifePool) Check() error { return nil }

func (p *realLifePool) list(kind string) []string {
	out, err := p.guarded(context.Background(), "zfs", "list", "-H", "-o", "name", "-t", kind, "-r", p.base)
	if err != nil {
		panic(err)
	}
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			names = append(names, l)
		}
	}
	return names
}

func (p *realLifePool) Datasets() []string  { return p.list("filesystem,volume") }
func (p *realLifePool) Snapshots() []string { return p.list("snapshot") }

func (p *realLifePool) Deferred(snapshot string) bool {
	v, err := p.Get(context.Background(), snapshot, "defer_destroy")
	return err == nil && v == "on"
}

package migrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/hellivan/simple-zfs-csi/internal/zpool"
)

// CLIBackend runs the real zfs binary through a Runner.
type CLIBackend struct {
	Run zpool.Runner
}

const objectTypes = "filesystem,volume,snapshot"

// List implements Backend. Labels are read with `-s local` so that a value a
// snapshot merely inherits from its dataset is never mistaken for a label.
func (b CLIBackend) List(ctx context.Context, roots []string) ([]Obj, error) {
	byName := map[string]*Obj{}
	var order []string
	for _, root := range roots {
		out, err := b.Run(ctx, "zfs", "list", "-H", "-p", "-t", objectTypes, "-d", "2", "-o", "name,origin,defer_destroy", root)
		if err != nil {
			if strings.Contains(err.Error(), "does not exist") {
				continue
			}
			return nil, err
		}
		for _, line := range splitLines(out) {
			f := strings.Split(line, "\t")
			if len(f) != 3 {
				return nil, fmt.Errorf("unexpected zfs list line %q", line)
			}
			if _, dup := byName[f[0]]; dup || !withinOneLevel(f[0], root) {
				continue
			}
			o := &Obj{Name: f[0], Deferred: f[2] == "on"}
			if f[1] != "-" {
				o.Origin = f[1]
			}
			byName[f[0]] = o
			order = append(order, f[0])
		}
		out, err = b.Run(ctx, "zfs", "get", "-H", "-p", "-s", "local", "-t", objectTypes, "-d", "2",
			"-o", "name,property,value", zpool.PropRole+","+zpool.PropOwner, root)
		if err != nil {
			return nil, err
		}
		for _, line := range splitLines(out) {
			f := strings.Split(line, "\t")
			if len(f) != 3 {
				return nil, fmt.Errorf("unexpected zfs get line %q", line)
			}
			o := byName[f[0]]
			if o == nil {
				continue
			}
			switch f[1] {
			case zpool.PropRole:
				o.Role = f[2]
			case zpool.PropOwner:
				o.Owner = f[2]
			}
		}
	}
	objs := make([]Obj, 0, len(order))
	for _, n := range order {
		objs = append(objs, *byName[n])
	}
	return objs, nil
}

// Set implements Backend.
func (b CLIBackend) Set(ctx context.Context, name, prop, value string) error {
	_, err := b.Run(ctx, "zfs", "set", prop+"="+value, name)
	return err
}

// DestroyDeferred implements Backend.
func (b CLIBackend) DestroyDeferred(ctx context.Context, snapshot string) error {
	_, err := b.Run(ctx, "zfs", "destroy", "-d", snapshot)
	return err
}

// withinOneLevel keeps the root, its direct children and the snapshots of
// either. `zfs -d 2` is needed to reach the snapshots of the children (a
// snapshot is one level below its dataset), and it also returns grandchildren.
func withinOneLevel(name, root string) bool {
	ds := name
	if i := strings.IndexByte(name, '@'); i >= 0 {
		ds = name[:i]
	}
	if ds == root {
		return true
	}
	rest, ok := strings.CutPrefix(ds, root+"/")
	return ok && !strings.Contains(rest, "/")
}

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

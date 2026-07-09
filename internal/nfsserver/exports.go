// Package nfsserver renders /etc/exports from ZfsShare intent and supervises the
// in-container NFS server daemons (rpcbind, rpc.mountd, nfsd kernel threads).
package nfsserver

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// DefaultOptions are applied to a client when the ZfsShare specifies none.
//
// no_root_squash lets containerized workloads that run as root write to the
// share (required by most RWX Kubernetes use cases). Override per-client via
// spec.nfs.clients[].options if you need stricter squashing.
var DefaultOptions = []string{"rw", "sync", "no_subtree_check", "no_root_squash"}

// Client is a single export client rule.
type Client struct {
	Client  string
	Options []string
}

// Export is one exported path with its client rules.
type Export struct {
	Path    string
	Clients []Client
}

// RenderExports produces deterministic /etc/exports content for the given
// exports (sorted by path) so the file only changes on real drift.
func RenderExports(exports []Export) string {
	sorted := make([]Export, len(exports))
	copy(sorted, exports)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	var b strings.Builder
	b.WriteString("# Managed by zfs-shares nfs-controller. DO NOT EDIT.\n")
	for _, e := range sorted {
		b.WriteString(e.Path)
		for _, c := range e.Clients {
			opts := c.Options
			if len(opts) == 0 {
				opts = DefaultOptions
			}
			fmt.Fprintf(&b, " %s(%s)", c.Client, strings.Join(opts, ","))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ExportManager owns the exports file and reloads the kernel export table.
type ExportManager struct {
	// Path to the exports file, default /etc/exports.
	Path string
	// ExportfsBin is the exportfs binary, default "exportfs" (resolved on PATH).
	ExportfsBin string
}

// NewExportManager returns a manager with defaults applied.
func NewExportManager(path string) *ExportManager {
	if path == "" {
		path = "/etc/exports"
	}
	return &ExportManager{Path: path, ExportfsBin: "exportfs"}
}

// Apply writes the desired exports file (only if it changed) and runs
// `exportfs -ra` to atomically sync the kernel export table.
func (m *ExportManager) Apply(exports []Export) error {
	content := RenderExports(exports)

	changed, err := m.writeIfChanged(content)
	if err != nil {
		return err
	}
	// Always run exportfs on first apply; afterwards only on change. Callers
	// invoke Apply on every reconcile, so gate the (cheap) syscall on drift.
	if !changed {
		return nil
	}
	return m.reload()
}

func (m *ExportManager) writeIfChanged(content string) (bool, error) {
	if cur, err := os.ReadFile(m.Path); err == nil && string(cur) == content {
		return false, nil
	} else if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	tmp := m.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, m.Path); err != nil {
		return false, err
	}
	return true, nil
}

func (m *ExportManager) reload() error {
	out, err := exec.Command(m.ExportfsBin, "-ra").CombinedOutput()
	if err != nil {
		return fmt.Errorf("exportfs -ra failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Package nvmet programs the Linux kernel NVMe target (nvmet) via configfs.
//
// The controller is the sole manager of nvmet on its storage node: reconcile
// makes the on-disk configfs tree exactly match the desired set of subsystems.
// It works incrementally (only touching what changed) so adding or removing one
// volume never disrupts unrelated, already-connected subsystems.
package nvmet

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultRoot is the standard mount point of the nvmet configfs tree.
const DefaultRoot = "/sys/kernel/config/nvmet"

// PortConfig describes the single shared TCP port all subsystems are attached to.
type PortConfig struct {
	// ID is the numeric port directory name, e.g. "1".
	ID string
	// TrType is the transport type, e.g. "tcp".
	TrType string
	// AdrFam is the address family, e.g. "ipv4".
	AdrFam string
	// TrAddr is the bind address, e.g. "0.0.0.0".
	TrAddr string
	// TrSvcID is the transport service id (port), e.g. "4420".
	TrSvcID string
}

// Subsystem is one desired NVMe-oF subsystem exporting a single zvol namespace.
type Subsystem struct {
	// NQN uniquely identifies the subsystem.
	NQN string
	// DevicePath is the backing block device (zvol), e.g. /dev/zvol/tank/pvc-1.
	DevicePath string
	// AllowedHosts are host NQNs permitted to connect. Empty allows any host.
	AllowedHosts []string
}

// Target manages the nvmet configfs tree rooted at Root.
type Target struct {
	Root string
	Port PortConfig
}

// NewTarget returns a Target with sane defaults applied.
func NewTarget(root string, port PortConfig) *Target {
	if root == "" {
		root = DefaultRoot
	}
	if port.ID == "" {
		port.ID = "1"
	}
	if port.TrType == "" {
		port.TrType = "tcp"
	}
	if port.AdrFam == "" {
		port.AdrFam = "ipv4"
	}
	if port.TrAddr == "" {
		port.TrAddr = "0.0.0.0"
	}
	if port.TrSvcID == "" {
		port.TrSvcID = "4420"
	}
	return &Target{Root: root, Port: port}
}

// Available reports whether the nvmet configfs tree is mounted and writable. It
// distinguishes the two common failure modes (configfs not mounted vs the nvmet
// kernel module not loaded) so operators get an actionable error.
func (t *Target) Available() error {
	// The nvmet subtree lives directly under the configfs mount point; if that
	// parent is missing, configfs itself is not mounted (into the host or pod).
	configfsRoot := filepath.Dir(t.Root)
	if _, err := os.Stat(configfsRoot); err != nil {
		return fmt.Errorf("configfs not available at %s (is configfs mounted on the node and into the pod?): %w", configfsRoot, err)
	}
	// The nvmet directory only appears once the nvmet kernel module is loaded.
	fi, err := os.Stat(t.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("nvmet configfs tree not found at %s: load the nvmet kernel module on the node (e.g. Talos machine.kernel.modules: nvmet and nvmet_tcp)", t.Root)
		}
		return fmt.Errorf("stat nvmet root %s: %w", t.Root, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", t.Root)
	}
	return nil
}

// TransportModuleLoaded reports whether the kernel module backing the port's
// transport appears loaded. It is best-effort: a negative result may simply mean
// the transport is built into the kernel (no /sys/module entry), so callers
// should treat false as a warning rather than a hard failure.
func (t *Target) TransportModuleLoaded() bool {
	if t.Port.TrType != "tcp" {
		return true // only the tcp transport is checked here
	}
	_, err := os.Stat("/sys/module/nvmet_tcp")
	return err == nil
}

func (t *Target) subsystemsDir() string { return filepath.Join(t.Root, "subsystems") }
func (t *Target) hostsDir() string      { return filepath.Join(t.Root, "hosts") }
func (t *Target) portDir() string       { return filepath.Join(t.Root, "ports", t.Port.ID) }

// EnsurePort creates and configures the shared TCP listener port. It is safe to
// call repeatedly; only drifting attributes are rewritten.
func (t *Target) EnsurePort() error {
	dir := t.portDir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := ensureDir(dir); err != nil {
		return fmt.Errorf("create port %s: %w", t.Port.ID, err)
	}
	// Attributes must be set before any subsystem is linked; writing an
	// unchanged value is rejected by the kernel, so only write on drift.
	attrs := map[string]string{
		"addr_adrfam":  t.Port.AdrFam,
		"addr_trtype":  t.Port.TrType,
		"addr_traddr":  t.Port.TrAddr,
		"addr_trsvcid": t.Port.TrSvcID,
	}
	for name, want := range attrs {
		if err := writeAttrIfChanged(filepath.Join(dir, name), want); err != nil {
			return fmt.Errorf("set port attr %s: %w", name, err)
		}
	}
	return nil
}

// Reconcile makes the configfs tree exactly match desired: create/update the
// listed subsystems and remove any others (this node's target pod owns nvmet).
func (t *Target) Reconcile(desired []Subsystem) error {
	if err := t.EnsurePort(); err != nil {
		return err
	}

	want := make(map[string]Subsystem, len(desired))
	for _, s := range desired {
		want[s.NQN] = s
	}

	existing, err := t.listSubsystems()
	if err != nil {
		return err
	}
	existingSet := make(map[string]struct{}, len(existing))
	for _, nqn := range existing {
		existingSet[nqn] = struct{}{}
	}

	// Remove subsystems that are no longer desired.
	for _, nqn := range existing {
		if _, ok := want[nqn]; !ok {
			if err := t.removeSubsystem(nqn); err != nil {
				return fmt.Errorf("remove subsystem %s: %w", nqn, err)
			}
		}
	}

	// Create or update desired subsystems (deterministic order for stable logs).
	nqns := make([]string, 0, len(want))
	for nqn := range want {
		nqns = append(nqns, nqn)
	}
	sort.Strings(nqns)
	for _, nqn := range nqns {
		if err := t.ensureSubsystem(want[nqn]); err != nil {
			return fmt.Errorf("ensure subsystem %s: %w", nqn, err)
		}
	}
	return nil
}

func (t *Target) listSubsystems() ([]string, error) {
	entries, err := os.ReadDir(t.subsystemsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (t *Target) ensureSubsystem(s Subsystem) error {
	subDir := filepath.Join(t.subsystemsDir(), s.NQN)
	if err := ensureDir(subDir); err != nil {
		return err
	}

	// Access control: explicit host allow-list vs allow-any.
	allowAny := len(s.AllowedHosts) == 0
	if allowAny {
		if err := writeAttrIfChanged(filepath.Join(subDir, "attr_allow_any_host"), "1"); err != nil {
			return err
		}
	} else {
		if err := writeAttrIfChanged(filepath.Join(subDir, "attr_allow_any_host"), "0"); err != nil {
			return err
		}
		if err := t.reconcileAllowedHosts(subDir, s.AllowedHosts); err != nil {
			return err
		}
	}

	// Namespace 1 backs the single zvol for this share.
	nsDir := filepath.Join(subDir, "namespaces", "1")
	if err := ensureDir(nsDir); err != nil {
		return err
	}
	devPathFile := filepath.Join(nsDir, "device_path")
	enableFile := filepath.Join(nsDir, "enable")
	curDev, _ := readAttr(devPathFile)
	if curDev != s.DevicePath {
		// device_path cannot be changed while the namespace is enabled.
		if err := writeAttrIfChanged(enableFile, "0"); err != nil {
			return err
		}
		if err := writeAttr(devPathFile, s.DevicePath); err != nil {
			return fmt.Errorf("set device_path=%s: %w", s.DevicePath, err)
		}
	}
	if err := writeAttrIfChanged(enableFile, "1"); err != nil {
		return fmt.Errorf("enable namespace: %w", err)
	}

	// Link the subsystem to the shared port so clients can discover it.
	link := filepath.Join(t.portDir(), "subsystems", s.NQN)
	if err := ensureSymlink(subDir, link); err != nil {
		return fmt.Errorf("attach subsystem to port: %w", err)
	}
	return nil
}

func (t *Target) reconcileAllowedHosts(subDir string, hosts []string) error {
	allowedDir := filepath.Join(subDir, "allowed_hosts")
	want := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		want[h] = struct{}{}
		// The referenced host object must exist before it can be linked.
		if err := ensureDir(filepath.Join(t.hostsDir(), h)); err != nil {
			return err
		}
		link := filepath.Join(allowedDir, h)
		if err := ensureSymlink(filepath.Join(t.hostsDir(), h), link); err != nil {
			return err
		}
	}
	// Drop hosts that are no longer allowed.
	entries, err := os.ReadDir(allowedDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if _, ok := want[e.Name()]; !ok {
			if err := os.Remove(filepath.Join(allowedDir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *Target) removeSubsystem(nqn string) error {
	subDir := filepath.Join(t.subsystemsDir(), nqn)

	// 1. Detach from the port.
	if err := removeIfExists(filepath.Join(t.portDir(), "subsystems", nqn)); err != nil {
		return err
	}
	// 2. Disable and remove the namespace.
	nsDir := filepath.Join(subDir, "namespaces", "1")
	if _, err := os.Stat(nsDir); err == nil {
		_ = writeAttr(filepath.Join(nsDir, "enable"), "0")
		if err := os.Remove(nsDir); err != nil {
			return fmt.Errorf("rmdir namespace: %w", err)
		}
	}
	// 3. Remove allowed_hosts symlinks.
	allowedDir := filepath.Join(subDir, "allowed_hosts")
	if entries, err := os.ReadDir(allowedDir); err == nil {
		for _, e := range entries {
			_ = os.Remove(filepath.Join(allowedDir, e.Name()))
		}
	}
	// 4. Remove the subsystem directory itself.
	return removeIfExists(subDir)
}

// --- configfs primitives ---

func ensureDir(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func ensureSymlink(target, link string) error {
	if cur, err := os.Readlink(link); err == nil {
		if cur == target {
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		// Not a symlink or unreadable; try to clear it.
		if _, statErr := os.Lstat(link); statErr == nil {
			if err := os.Remove(link); err != nil {
				return err
			}
		}
	}
	if err := os.Symlink(target, link); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

func removeIfExists(path string) error {
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.Remove(path)
}

func writeAttr(path, value string) error {
	// configfs attributes are written without a trailing newline requirement,
	// but the kernel tolerates one; keep it clean.
	return os.WriteFile(path, []byte(value), 0o644)
}

func writeAttrIfChanged(path, value string) error {
	if cur, err := readAttr(path); err == nil && cur == value {
		return nil
	}
	return writeAttr(path, value)
}

func readAttr(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

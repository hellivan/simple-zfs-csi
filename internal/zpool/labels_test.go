package zpool

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// labelZFS is a minimal ZFS whose Get and SetProperty read and write one map,
// which is all EnsureLabels needs. Embedding the interface makes any other
// call fail loudly.
type labelZFS struct {
	ZFS
	props   map[string]string // "name prop" -> value
	sets    []string
	getErr  error
	setErr  error
	missing bool
}

func (l *labelZFS) Get(_ context.Context, name, prop string) (string, error) {
	if l.getErr != nil {
		return "", l.getErr
	}
	if l.missing {
		return "", ErrNotExist
	}
	if v, ok := l.props[name+" "+prop]; ok {
		return v, nil
	}
	return "-", nil // ZFS prints "-" for an unset user property
}

func (l *labelZFS) SetProperty(_ context.Context, name, prop, value string) error {
	if l.setErr != nil {
		return l.setErr
	}
	l.props[name+" "+prop] = value
	l.sets = append(l.sets, name+" "+prop+"="+value)
	return nil
}

func TestLabelProps(t *testing.T) {
	got := LabelProps(RoleRaw, "uid-1")
	want := map[string]string{PropRole: "raw", PropOwner: "uid-1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LabelProps = %v, want %v", got, want)
	}
	if _, ok := LabelProps(RoleRaw, "")[PropOwner]; ok {
		t.Fatal("an empty owner must not be stored as an empty value")
	}
}

func TestMergeLabels_KeepsInputAndLabelsWin(t *testing.T) {
	in := map[string]string{"compression": "lz4", PropRole: "user-chose-this"}
	out := MergeLabels(in, RoleVolume, "uid-1")
	if out["compression"] != "lz4" || out[PropRole] != RoleVolume || out[PropOwner] != "uid-1" {
		t.Fatalf("MergeLabels = %v", out)
	}
	if in[PropRole] != "user-chose-this" || len(in) != 2 {
		t.Fatalf("input map was modified: %v", in)
	}
	if got := MergeLabels(nil, RoleBacking, ""); !reflect.DeepEqual(got, map[string]string{PropRole: "backing"}) {
		t.Fatalf("MergeLabels(nil) = %v", got)
	}
}

func TestEnsureLabels(t *testing.T) {
	const snap = "tank/k8s/v@csi-snap-1"
	tests := []struct {
		name     string
		existing map[string]string
		wantSets []string
	}{
		{"unset labels are written", map[string]string{}, []string{
			snap + " " + PropOwner + "=uid-1",
			snap + " " + PropRole + "=raw",
		}},
		{"correct labels are left alone", map[string]string{
			snap + " " + PropRole:  "raw",
			snap + " " + PropOwner: "uid-1",
		}, nil},
		{"a wrong owner is repaired", map[string]string{
			snap + " " + PropRole:  "raw",
			snap + " " + PropOwner: "other",
		}, []string{snap + " " + PropOwner + "=uid-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			z := &labelZFS{props: tt.existing}
			if err := EnsureLabels(context.Background(), z, snap, RoleRaw, "uid-1"); err != nil {
				t.Fatal(err)
			}
			if len(z.sets) != len(tt.wantSets) {
				t.Fatalf("sets = %v, want %v", z.sets, tt.wantSets)
			}
			for _, w := range tt.wantSets {
				found := false
				for _, s := range z.sets {
					found = found || s == w
				}
				if !found {
					t.Fatalf("sets = %v, missing %q", z.sets, w)
				}
			}
			// A second call changes nothing.
			z.sets = nil
			if err := EnsureLabels(context.Background(), z, snap, RoleRaw, "uid-1"); err != nil || len(z.sets) != 0 {
				t.Fatalf("not idempotent: err=%v sets=%v", err, z.sets)
			}
		})
	}
}

func TestEnsureLabels_Errors(t *testing.T) {
	ctx := context.Background()
	if err := EnsureLabels(ctx, &labelZFS{missing: true}, "tank/x", RoleVolume, "u"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("a missing object must surface ErrNotExist, got %v", err)
	}
	if err := EnsureLabels(ctx, &labelZFS{getErr: errors.New("boom")}, "tank/x", RoleVolume, "u"); err == nil {
		t.Fatal("a read error must be returned")
	}
	z := &labelZFS{props: map[string]string{}, setErr: errors.New("set failed")}
	if err := EnsureLabels(ctx, z, "tank/x", RoleVolume, "u"); err == nil || !strings.Contains(err.Error(), "set failed") {
		t.Fatalf("a write error must be returned, got %v", err)
	}
}

// The CLI turns the labels into `-o` arguments, so they reach ZFS at create
// time in one call with the other properties.
func TestCreateDataset_LabelsReachZFS(t *testing.T) {
	f := &fakeRunner{}
	z := &CLI{Run: f.run}
	if err := z.CreateDataset(context.Background(), "tank/k8s/pvc-1", MergeLabels(map[string]string{"compression": "lz4"}, RoleVolume, "uid-1")); err != nil {
		t.Fatal(err)
	}
	want := []string{"create", "-o", "compression=lz4", "-o", PropOwner + "=uid-1", "-o", PropRole + "=volume", "tank/k8s/pvc-1"}
	if !reflect.DeepEqual(f.gotArgs, want) {
		t.Fatalf("args = %v, want %v", f.gotArgs, want)
	}
}

func TestClone_LabelsReachZFS(t *testing.T) {
	f := &fakeRunner{}
	z := &CLI{Run: f.run}
	if err := z.Clone(context.Background(), "tank/v@s", "tank/b", MergeLabels(map[string]string{"canmount": "off"}, RoleBacking, "uid-2")); err != nil {
		t.Fatal(err)
	}
	want := []string{"clone", "-o", "canmount=off", "-o", PropOwner + "=uid-2", "-o", PropRole + "=backing", "tank/v@s", "tank/b"}
	if !reflect.DeepEqual(f.gotArgs, want) {
		t.Fatalf("args = %v, want %v", f.gotArgs, want)
	}
}

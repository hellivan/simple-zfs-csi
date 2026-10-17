package memzfs

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBusyAfterZvolCreate(t *testing.T) {
	ctx := context.Background()
	m := New("tank")
	m.BusyAfterZvolCreate(2)
	if err := m.CreateZvol(ctx, "tank/v", 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1; i++ {
		if err := m.Destroy(ctx, "tank/v", false); err == nil || !strings.Contains(err.Error(), "dataset is busy") {
			t.Fatalf("want busy, got %v", err)
		}
	}
	if err := m.Destroy(ctx, "tank/v", false); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
}

func TestInjectAndLog(t *testing.T) {
	ctx := context.Background()
	m := New("tank")
	boom := errors.New("boom")
	m.Inject("promote", "tank/c", boom, 1)
	_ = m.CreateDataset(ctx, "tank/a", nil)
	_ = m.Snapshot(ctx, "tank/a@s")
	_ = m.Clone(ctx, "tank/a@s", "tank/c", nil)
	if err := m.Promote(ctx, "tank/c"); !errors.Is(err, boom) {
		t.Fatalf("want injected error, got %v", err)
	}
	if err := m.Promote(ctx, "tank/c"); err != nil {
		t.Fatalf("injection must be consumed, got %v", err)
	}
	if got := m.Log(); got[len(got)-1] != "promote tank/c" {
		t.Errorf("log = %v", got)
	}
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCatchesBrokenState(t *testing.T) {
	m := New("tank")
	m.ds["tank/x"] = &dataset{name: "tank/x", origin: "tank/gone@s", snaps: map[string]*snapshot{}}
	if err := m.Check(); err == nil {
		t.Fatal("dangling origin not detected")
	}
}

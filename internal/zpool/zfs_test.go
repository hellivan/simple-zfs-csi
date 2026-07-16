package zpool

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeRunner records the last invocation and returns canned output/error.
type fakeRunner struct {
	gotName string
	gotArgs []string
	out     string
	err     error
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) (string, error) {
	f.gotName = name
	f.gotArgs = args
	return f.out, f.err
}

func TestCreateDataset_Args(t *testing.T) {
	f := &fakeRunner{}
	z := &CLI{Bin: "zfs", Run: f.run}

	if err := z.CreateDataset(context.Background(), "tank/k8s/pvc-1", map[string]string{
		"compression": "lz4",
		"recordsize":  "1M",
	}); err != nil {
		t.Fatalf("CreateDataset: %v", err)
	}

	if f.gotName != "zfs" {
		t.Errorf("bin = %q, want zfs", f.gotName)
	}
	want := []string{"create", "-o", "compression=lz4", "-o", "recordsize=1M", "tank/k8s/pvc-1"}
	if !reflect.DeepEqual(f.gotArgs, want) {
		t.Errorf("args = %v, want %v", f.gotArgs, want)
	}
}

func TestCreateZvol_Args(t *testing.T) {
	f := &fakeRunner{}
	z := &CLI{Run: f.run}

	if err := z.CreateZvol(context.Background(), "tank/pvc-2", 10*1024*1024, map[string]string{"volblocksize": "16k"}); err != nil {
		t.Fatalf("CreateZvol: %v", err)
	}
	want := []string{"create", "-V", "10485760", "-o", "volblocksize=16k", "tank/pvc-2"}
	if !reflect.DeepEqual(f.gotArgs, want) {
		t.Errorf("args = %v, want %v", f.gotArgs, want)
	}
}

func TestCreateZvol_RejectsNonPositiveSize(t *testing.T) {
	z := &CLI{Run: (&fakeRunner{}).run}
	if err := z.CreateZvol(context.Background(), "tank/pvc", 0, nil); err == nil {
		t.Fatal("expected error for zero size")
	}
}

func TestDestroy_IdempotentOnMissing(t *testing.T) {
	f := &fakeRunner{err: errors.New("cannot destroy 'tank/x': dataset does not exist")}
	z := &CLI{Run: f.run}
	if err := z.Destroy(context.Background(), "tank/x", true); err != nil {
		t.Fatalf("Destroy should ignore missing dataset, got %v", err)
	}
	want := []string{"destroy", "-r", "tank/x"}
	if !reflect.DeepEqual(f.gotArgs, want) {
		t.Errorf("args = %v, want %v", f.gotArgs, want)
	}
}

func TestDestroy_PropagatesOtherErrors(t *testing.T) {
	f := &fakeRunner{err: errors.New("cannot destroy 'tank/x': dataset is busy")}
	z := &CLI{Run: f.run}
	if err := z.Destroy(context.Background(), "tank/x", false); err == nil {
		t.Fatal("expected busy error to propagate")
	}
}

func TestGet_NotExistWrapsSentinel(t *testing.T) {
	f := &fakeRunner{err: errors.New("cannot open 'tank/x': dataset does not exist")}
	z := &CLI{Run: f.run}
	_, err := z.Get(context.Background(), "tank/x", "type")
	if !errors.Is(err, ErrNotExist) {
		t.Fatalf("expected ErrNotExist, got %v", err)
	}
}

func TestGet_TrimsValue(t *testing.T) {
	f := &fakeRunner{out: "filesystem\n"}
	z := &CLI{Run: f.run}
	got, err := z.Get(context.Background(), "tank/x", "type")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "filesystem" {
		t.Errorf("value = %q, want filesystem", got)
	}
	want := []string{"get", "-H", "-p", "-o", "value", "type", "tank/x"}
	if !reflect.DeepEqual(f.gotArgs, want) {
		t.Errorf("args = %v, want %v", f.gotArgs, want)
	}
}

func TestList_ParsesTabSeparated(t *testing.T) {
	f := &fakeRunner{out: strings.Join([]string{
		"tank\tfilesystem\t/tank",
		"tank/k8s\tfilesystem\t/tank/k8s",
		"tank/pvc-2\tvolume\t-",
		"",
	}, "\n")}
	z := &CLI{Run: f.run}

	got, err := z.List(context.Background(), KindAll)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Dataset{
		{Name: "tank", Type: KindFilesystem, Mountpoint: "/tank"},
		{Name: "tank/k8s", Type: KindFilesystem, Mountpoint: "/tank/k8s"},
		{Name: "tank/pvc-2", Type: KindVolume, Mountpoint: ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("List = %+v, want %+v", got, want)
	}
	wantArgs := []string{"list", "-H", "-p", "-o", "name,type,mountpoint", "-t", "all"}
	if !reflect.DeepEqual(f.gotArgs, wantArgs) {
		t.Errorf("args = %v, want %v", f.gotArgs, wantArgs)
	}
}

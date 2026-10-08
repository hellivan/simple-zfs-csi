package csi

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/hellivan/simple-zfs-csi/api/v1alpha1"
)

func gsrc(name, pool string) *storagev1alpha1.ZfsDataset {
	return &storagev1alpha1.ZfsDataset{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: storagev1alpha1.ZfsDatasetSpec{
			PoolGUID: pool, Dataset: "k8s/" + name, Type: storagev1alpha1.DatasetTypeFilesystem,
		},
	}
}

func newGroupServer(cl client.Client) *GroupControllerServer {
	return &GroupControllerServer{Client: cl, CreateTimeout: 150 * time.Millisecond, PollInterval: 10 * time.Millisecond, Log: logr.Discard()}
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("code = %v (%v), want %v", status.Code(err), err, want)
	}
}

func TestCreateGroup_ValidationAndCrossPool(t *testing.T) {
	gs := newGroupServer(newTestClient(t, gsrc("a", "1"), gsrc("b", "2")))
	ctx := context.Background()

	_, err := gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{SourceVolumeIds: []string{"a"}})
	wantCode(t, err, codes.InvalidArgument)
	_, err = gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g"})
	wantCode(t, err, codes.InvalidArgument)
	_, err = gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"a", "a"}})
	wantCode(t, err, codes.InvalidArgument)
	_, err = gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"a", "missing"}})
	wantCode(t, err, codes.NotFound)
	_, err = gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"a", "b"}})
	wantCode(t, err, codes.FailedPrecondition)
	if err := gs.Client.Get(ctx, client.ObjectKey{Name: "g"}, &storagev1alpha1.ZfsGroupSnapshot{}); err == nil {
		t.Fatal("cross-pool request must not create a group")
	}
}

func TestCreateGroup_ReadyAndIdempotent(t *testing.T) {
	cl := newTestClient(t, gsrc("data", "1"), gsrc("wal", "1"))
	gs := newGroupServer(cl)
	ctx := context.Background()

	// Simulate the agent making the group Ready once it appears.
	go func() {
		for i := 0; i < 100; i++ {
			g := &storagev1alpha1.ZfsGroupSnapshot{}
			if cl.Get(ctx, client.ObjectKey{Name: "g"}, g) == nil {
				for _, m := range g.Spec.Members {
					_ = cl.Create(ctx, &storagev1alpha1.ZfsSnapshot{
						ObjectMeta: metav1.ObjectMeta{Name: m.ZfsSnapshotRef},
						Spec:       storagev1alpha1.ZfsSnapshotSpec{SourceVolume: m.SourceVolume, GroupSnapshotID: "g"},
						Status:     storagev1alpha1.ZfsSnapshotStatus{Phase: storagev1alpha1.SnapshotPhaseReady, ReadyToUse: true},
					})
				}
				now := metav1.Now()
				g.Status.Phase = storagev1alpha1.GroupSnapshotPhaseReady
				g.Status.ReadyToUse = true
				g.Status.CreationTime = &now
				_ = cl.Status().Update(ctx, g)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	req := &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"data", "wal"}}
	resp, err := gs.CreateVolumeGroupSnapshot(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GroupSnapshot.ReadyToUse || len(resp.GroupSnapshot.Snapshots) != 2 || resp.GroupSnapshot.GroupSnapshotId != "g" {
		t.Fatalf("resp = %+v", resp.GroupSnapshot)
	}
	for _, s := range resp.GroupSnapshot.Snapshots {
		if s.GroupSnapshotId != "g" || s.SourceVolumeId == "" {
			t.Errorf("member = %+v", s)
		}
	}

	// Same set in another order: same group. Different set: conflict.
	if _, err := gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"wal", "data"}}); err != nil {
		t.Fatal(err)
	}
	_, err = gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"data"}})
	wantCode(t, err, codes.AlreadyExists)

	ids := []string{resp.GroupSnapshot.Snapshots[1].SnapshotId, resp.GroupSnapshot.Snapshots[0].SnapshotId}
	if _, err := gs.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "g", SnapshotIds: ids}); err != nil {
		t.Fatal(err)
	}
	_, err = gs.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "g", SnapshotIds: ids[:1]})
	wantCode(t, err, codes.InvalidArgument)
	_, err = gs.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{GroupSnapshotId: "g", SnapshotIds: ids[:1]})
	wantCode(t, err, codes.InvalidArgument)
	if _, err := gs.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{GroupSnapshotId: "g", SnapshotIds: ids}); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.DeleteVolumeGroupSnapshot(ctx, &csi.DeleteVolumeGroupSnapshotRequest{GroupSnapshotId: "nope"}); err != nil {
		t.Fatalf("missing group must delete cleanly: %v", err)
	}
}

func TestCreateGroup_TimeoutAndTerminating(t *testing.T) {
	cl := newTestClient(t, gsrc("a", "1"))
	gs := newGroupServer(cl)
	ctx := context.Background()

	_, err := gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"a"}})
	wantCode(t, err, codes.DeadlineExceeded)

	g := &storagev1alpha1.ZfsGroupSnapshot{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "g"}, g); err != nil {
		t.Fatal(err)
	}
	g.Finalizers = []string{"x"}
	if err := cl.Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(ctx, g); err != nil {
		t.Fatal(err)
	}
	_, err = gs.CreateVolumeGroupSnapshot(ctx, &csi.CreateVolumeGroupSnapshotRequest{Name: "g", SourceVolumeIds: []string{"a"}})
	wantCode(t, err, codes.Aborted)
}

func TestGetGroup_DecisionTable(t *testing.T) {
	now := metav1.Now()
	grp := &storagev1alpha1.ZfsGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "g"},
		Spec:       storagev1alpha1.ZfsGroupSnapshotSpec{PoolGUID: "1", Members: []storagev1alpha1.ZfsGroupSnapshotMember{{SourceVolume: "a", ZfsSnapshotRef: "c-a"}}},
	}
	cl := newTestClient(t, grp)
	gs := newGroupServer(cl)
	ctx := context.Background()
	ids := memberSnapshotIDs(grp)

	_, err := gs.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "missing"})
	wantCode(t, err, codes.NotFound)
	_, err = gs.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "g", SnapshotIds: ids})
	wantCode(t, err, codes.Aborted) // not taken yet

	g := &storagev1alpha1.ZfsGroupSnapshot{}
	_ = cl.Get(ctx, client.ObjectKey{Name: "g"}, g)
	g.Status.CreationTime = &now
	if err := cl.Status().Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	_, err = gs.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "g", SnapshotIds: ids})
	wantCode(t, err, codes.Aborted) // taken, members not created yet: still provisioning

	_ = cl.Get(ctx, client.ObjectKey{Name: "g"}, g)
	g.Status.ProvisionedAt = &now
	if err := cl.Status().Update(ctx, g); err != nil {
		t.Fatal(err)
	}
	_, err = gs.GetVolumeGroupSnapshot(ctx, &csi.GetVolumeGroupSnapshotRequest{GroupSnapshotId: "g", SnapshotIds: ids})
	wantCode(t, err, codes.FailedPrecondition) // frozen group, member gone: really lost
}

func TestDeleteSnapshot_RefusesGroupMember(t *testing.T) {
	cl := newTestClient(t, &storagev1alpha1.ZfsSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "m"},
		Spec:       storagev1alpha1.ZfsSnapshotSpec{GroupSnapshotID: "g"},
	})
	cs := newController(cl)
	_, err := cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "m"})
	wantCode(t, err, codes.InvalidArgument)
	if _, err := cs.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "gone"}); err != nil {
		t.Fatal(err)
	}
}

func TestMemberRefNameLength(t *testing.T) {
	if got := memberRefName("groupsnapshot-abc"); !strings.HasPrefix(got, "groupsnapshot-abc-") || len(got) != len("groupsnapshot-abc-")+36 {
		t.Fatalf("unexpected name %q", got)
	}
	if got := memberRefName(strings.Repeat("a", 253)); len(got) > 253 {
		t.Fatalf("name too long: %d", len(got))
	}
}

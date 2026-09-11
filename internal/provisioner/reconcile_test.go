package provisioner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// fakeVC implements orphanVCenter for tests.
type fakeVC struct {
	folder       string
	vms          []vcenter.FolderVM
	listErr      error
	powerOffErr  map[string]error
	destroyErr   map[string]error
	powerOffSeen []string
	destroySeen  []string
}

func (f *fakeVC) ListVMsInFolder(_ context.Context, folder string) ([]vcenter.FolderVM, error) {
	f.folder = folder
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.vms, nil
}
func (f *fakeVC) PowerOffVM(_ context.Context, moref string) error {
	f.powerOffSeen = append(f.powerOffSeen, moref)
	if err, ok := f.powerOffErr[moref]; ok {
		return err
	}
	return nil
}
func (f *fakeVC) DestroyVM(_ context.Context, moref string) error {
	f.destroySeen = append(f.destroySeen, moref)
	if err, ok := f.destroyErr[moref]; ok {
		return err
	}
	return nil
}

// fakeDB implements orphanDB for tests.
type fakeDB struct {
	links map[string]database.PodVMLink
	err   error
}

func (d *fakeDB) ListPodVMLinksByVCenterID(_ context.Context) (map[string]database.PodVMLink, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.links, nil
}

func TestReconcileVCenterOrphans_ClassifiesEveryCategory(t *testing.T) {
	fixedNow := time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return fixedNow }

	// Six VMs in the folder, exercising every action path the reconciler
	// can take. cutoff = fixedNow - 1h = 23:00 UTC.
	vc := &fakeVC{
		vms: []vcenter.FolderVM{
			{MoRef: "vm-1", Name: "synthetic-noop-aaa"},  // destroyed synthetic, old → destroy
			{MoRef: "vm-2", Name: "synthetic-noop-bbb"},  // destroy_failed synthetic, old → destroy
			{MoRef: "vm-3", Name: "synthetic-noop-recent"}, // destroyed synthetic, recent → skip
			{MoRef: "vm-4", Name: "student-pod-keep-me"}, // active student → leave alone
			{MoRef: "vm-5", Name: "student-pod-dead"},    // destroyed non-synthetic → dry-run
			{MoRef: "vm-6", Name: "manual-vm"},           // no DB row → dry-run
		},
	}
	db := &fakeDB{
		links: map[string]database.PodVMLink{
			"vm-1": {PodID: uuid.New(), PodName: "synthetic-noop-aaa", PodStatus: "destroyed", PodUpdatedAt: fixedNow.Add(-2 * time.Hour)},
			"vm-2": {PodID: uuid.New(), PodName: "synthetic-noop-bbb", PodStatus: "destroy_failed", PodUpdatedAt: fixedNow.Add(-2 * time.Hour)},
			"vm-3": {PodID: uuid.New(), PodName: "synthetic-noop-recent", PodStatus: "destroyed", PodUpdatedAt: fixedNow.Add(-10 * time.Minute)},
			"vm-4": {PodID: uuid.New(), PodName: "student-pod-keep-me", PodStatus: "active", PodUpdatedAt: fixedNow.Add(-30 * time.Minute)},
			"vm-5": {PodID: uuid.New(), PodName: "student-pod-dead", PodStatus: "destroyed", PodUpdatedAt: fixedNow.Add(-2 * time.Hour)},
			// vm-6 intentionally missing.
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	counts, err := reconcileVCenterOrphans(context.Background(), vc, db, logger,
		OrphanReconcilerConfig{Folder: "Student-VMs"}, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if vc.folder != "Student-VMs" {
		t.Errorf("folder passed to ListVMsInFolder = %q, want Student-VMs", vc.folder)
	}

	want := ReconcileCounts{
		InventoryVMs:       6,
		Active:             1,
		DestroyedSynthetic: 2,
		DestroyedOther:     1,
		Unknown:            1,
		SkippedRecent:      1,
		Destroyed:          2,
		DestroyFailures:    0,
	}
	if counts != want {
		t.Errorf("counts mismatch\n got  %+v\n want %+v", counts, want)
	}

	// Synthetic terminal pods must be destroyed; nothing else.
	if !equalUnordered(vc.destroySeen, []string{"vm-1", "vm-2"}) {
		t.Errorf("destroySeen = %v, want [vm-1 vm-2] in any order", vc.destroySeen)
	}
	// PowerOff is best-effort and runs before destroy on the same set.
	if !equalUnordered(vc.powerOffSeen, []string{"vm-1", "vm-2"}) {
		t.Errorf("powerOffSeen = %v, want [vm-1 vm-2] in any order", vc.powerOffSeen)
	}
}

func TestReconcileVCenterOrphans_DefaultsApplied(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC) }
	vc := &fakeVC{}
	db := &fakeDB{links: map[string]database.PodVMLink{}}

	_, err := reconcileVCenterOrphans(context.Background(), vc, db, nil,
		OrphanReconcilerConfig{}, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if vc.folder != "Student-VMs" {
		t.Errorf("empty Folder should default to Student-VMs, got %q", vc.folder)
	}
}

func TestReconcileVCenterOrphans_PowerOffFailureDoesNotBlockDestroy(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC) }
	vc := &fakeVC{
		vms: []vcenter.FolderVM{
			{MoRef: "vm-1", Name: "synthetic-noop-x"},
		},
		powerOffErr: map[string]error{"vm-1": errors.New("already off")},
	}
	db := &fakeDB{links: map[string]database.PodVMLink{
		"vm-1": {PodName: "synthetic-noop-x", PodStatus: "destroyed", PodUpdatedAt: now().Add(-2 * time.Hour)},
	}}

	counts, err := reconcileVCenterOrphans(context.Background(), vc, db, nil,
		OrphanReconcilerConfig{}, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if counts.Destroyed != 1 || counts.DestroyFailures != 0 {
		t.Errorf("power-off failure shouldn't surface; got counts=%+v", counts)
	}
	if len(vc.destroySeen) != 1 || vc.destroySeen[0] != "vm-1" {
		t.Errorf("destroy should still run; destroySeen=%v", vc.destroySeen)
	}
}

func TestReconcileVCenterOrphans_DestroyFailureCounted(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC) }
	vc := &fakeVC{
		vms: []vcenter.FolderVM{
			{MoRef: "vm-1", Name: "synthetic-noop-x"},
		},
		destroyErr: map[string]error{"vm-1": errors.New("task failed")},
	}
	db := &fakeDB{links: map[string]database.PodVMLink{
		"vm-1": {PodName: "synthetic-noop-x", PodStatus: "destroyed", PodUpdatedAt: now().Add(-2 * time.Hour)},
	}}

	counts, err := reconcileVCenterOrphans(context.Background(), vc, db, nil,
		OrphanReconcilerConfig{}, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if counts.Destroyed != 0 || counts.DestroyFailures != 1 {
		t.Errorf("expected destroyed=0 destroy_failures=1, got %+v", counts)
	}
}

func TestReconcileVCenterOrphans_PropagatesListError(t *testing.T) {
	vc := &fakeVC{listErr: errors.New("vcenter down")}
	db := &fakeDB{}
	_, err := reconcileVCenterOrphans(context.Background(), vc, db, nil, OrphanReconcilerConfig{}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "list vCenter folder") {
		t.Errorf("expected list-folder error wrapping, got %v", err)
	}
}

func TestReconcileVCenterOrphans_PropagatesDBError(t *testing.T) {
	vc := &fakeVC{}
	db := &fakeDB{err: errors.New("db down")}
	_, err := reconcileVCenterOrphans(context.Background(), vc, db, nil, OrphanReconcilerConfig{}, time.Now)
	if err == nil || !strings.Contains(err.Error(), "list pod_vms") {
		t.Errorf("expected db error wrapping, got %v", err)
	}
}

func TestReconcileVCenterOrphans_PushesMetricsWhenPusherSet(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	pusher := &OrphanCountPusher{
		BaseURL: srv.URL,
		Job:     "crucible_provision_worker",
		HTTP:    srv.Client(),
	}

	now := func() time.Time { return time.Date(2026, 6, 11, 0, 0, 0, 0, time.UTC) }
	vc := &fakeVC{vms: []vcenter.FolderVM{{MoRef: "vm-X", Name: "manual-vm"}}}
	db := &fakeDB{links: map[string]database.PodVMLink{}}

	_, err := reconcileVCenterOrphans(context.Background(), vc, db, nil,
		OrphanReconcilerConfig{Folder: "Student-VMs", Pusher: pusher}, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if !strings.Contains(gotPath, "/metrics/job/crucible_provision_worker/folder/Student-VMs") {
		t.Errorf("unexpected push path: %s", gotPath)
	}
	for _, want := range []string{
		`crucible_vcenter_orphans_total{folder="Student-VMs",category="inventory"} 1`,
		`crucible_vcenter_orphans_total{folder="Student-VMs",category="unknown_dryrun"} 1`,
		`crucible_vcenter_orphans_destroyed_total{folder="Student-VMs"} 0`,
		`crucible_vcenter_orphans_destroy_failures_total{folder="Student-VMs"} 0`,
		`crucible_vcenter_orphans_run_timestamp_seconds{folder="Student-VMs"}`,
	} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("body missing %q\n--- body ---\n%s", want, gotBody)
		}
	}
}

func TestOrphanCountPusher_NoOpWhenBaseURLEmpty(t *testing.T) {
	p := &OrphanCountPusher{}
	if err := p.Push(context.Background(), "Student-VMs", ReconcileCounts{}); err != nil {
		t.Fatalf("expected nil err when BaseURL empty, got %v", err)
	}
}

func TestOrphanCountPusher_PropagatesNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	p := &OrphanCountPusher{BaseURL: srv.URL, Job: "x", HTTP: srv.Client()}
	err := p.Push(context.Background(), "Student-VMs", ReconcileCounts{})
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("expected 502 error, got %v", err)
	}
}

func equalUnordered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}
	return true
}

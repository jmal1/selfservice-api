package provisioner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type fakeTemplateOrphanVC struct {
	folder      string
	vms         []vcenter.FolderVM
	listErr     error
	destroyErr  map[string]error
	destroySeen []string

	// poweredOn / guestInfoErr drive the DB pass's pre-destroy power-state
	// check, keyed by moref. Absent entries model a powered-off VM, which is
	// the ordinary abandoned-shell case the reaper exists to clean up.
	poweredOn    map[string]bool
	guestInfoErr map[string]error
}

func (f *fakeTemplateOrphanVC) ListVMsInFolder(_ context.Context, folder string) ([]vcenter.FolderVM, error) {
	f.folder = folder
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.vms, nil
}

func (f *fakeTemplateOrphanVC) PowerOffVM(_ context.Context, _ string) error { return nil }

func (f *fakeTemplateOrphanVC) GetGuestInfo(_ context.Context, moref string) (*vcenter.GuestInfo, error) {
	if err, ok := f.guestInfoErr[moref]; ok {
		return nil, err
	}
	return &vcenter.GuestInfo{Name: moref, PoweredOn: f.poweredOn[moref]}, nil
}

func (f *fakeTemplateOrphanVC) DestroyVM(_ context.Context, moref string) error {
	f.destroySeen = append(f.destroySeen, moref)
	if err, ok := f.destroyErr[moref]; ok {
		return err
	}
	return nil
}

type fakeTemplateOrphanDB struct {
	stale    []database.StaleWizardTemplateVM
	owned    map[string]struct{}
	staleErr error
	ownedErr error
	cleared  []string
	clearErr error
}

func (d *fakeTemplateOrphanDB) ListStaleWizardTemplateVMs(_ context.Context, _ time.Duration) ([]database.StaleWizardTemplateVM, error) {
	if d.staleErr != nil {
		return nil, d.staleErr
	}
	return d.stale, nil
}

func (d *fakeTemplateOrphanDB) ListOwnedTemplateFolderMorefs(_ context.Context) (map[string]struct{}, error) {
	if d.ownedErr != nil {
		return nil, d.ownedErr
	}
	if d.owned == nil {
		return map[string]struct{}{}, nil
	}
	return d.owned, nil
}

func (d *fakeTemplateOrphanDB) ClearTemplateVCenterVMIfMatch(_ context.Context, id uuid.UUID, expectedMoref string) error {
	if d.clearErr != nil {
		return d.clearErr
	}
	d.cleared = append(d.cleared, id.String()+":"+expectedMoref)
	return nil
}

func TestReconcileTemplateOrphans_ClassifiesEveryCategory(t *testing.T) {
	fixedNow := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	now := func() time.Time { return fixedNow }
	old := fixedNow.Add(-25 * time.Hour)
	recent := fixedNow.Add(-30 * time.Minute)

	errorID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	draftID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	vc := &fakeTemplateOrphanVC{
		vms: []vcenter.FolderVM{
			{MoRef: "vm-active", Name: "tpl-ubuntu-active", CreatedAt: &old},       // owned → skip
			{MoRef: "vm-orphan-tpl", Name: "tpl-gone-abcdef", CreatedAt: &old},     // destroy
			{MoRef: "vm-recent-tpl", Name: "tpl-fresh-123456", CreatedAt: &recent}, // skip recent
			{MoRef: "vm-manual", Name: "manual-parked", CreatedAt: &old},           // dry-run
			{MoRef: "vm-replica", Name: "tplrep-source-1", CreatedAt: &old},        // owned replica
			{MoRef: "vm-health", Name: "crucible-healthcheck-x", CreatedAt: &old},  // destroy
		},
	}
	db := &fakeTemplateOrphanDB{
		stale: []database.StaleWizardTemplateVM{
			{ID: errorID, Name: "failed", State: models.TemplateStateError, VCenterVMID: "vm-err", UpdatedAt: old},
			{ID: draftID, Name: "cancelled", State: models.TemplateStateDraft, VCenterVMID: "vm-draft", UpdatedAt: old},
		},
		owned: map[string]struct{}{
			"vm-active":  {},
			"vm-replica": {},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	counts, err := reconcileTemplateOrphans(context.Background(), vc, db, logger,
		TemplateOrphanReconcilerConfig{Folder: "/DC/vm/templates"}, now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if counts.StaleFailed != 1 || counts.StaleDraft != 1 {
		t.Errorf("stale_failed=%d stale_draft=%d; want 1/1", counts.StaleFailed, counts.StaleDraft)
	}
	if counts.InventoryDestroyed != 2 { // tpl-gone + healthcheck
		t.Errorf("inventory_destroyed=%d; want 2", counts.InventoryDestroyed)
	}
	if counts.SkippedOwned != 2 {
		t.Errorf("skipped_owned=%d; want 2", counts.SkippedOwned)
	}
	if counts.SkippedRecent != 1 {
		t.Errorf("skipped_recent=%d; want 1", counts.SkippedRecent)
	}
	if counts.UnknownDryRun != 1 {
		t.Errorf("unknown_dryrun=%d; want 1", counts.UnknownDryRun)
	}
	if counts.Destroyed != 4 { // 2 stale + 2 inventory
		t.Errorf("destroyed=%d; want 4", counts.Destroyed)
	}
	if counts.DestroyFailures != 0 {
		t.Errorf("destroy_failures=%d; want 0", counts.DestroyFailures)
	}

	wantDestroyed := map[string]bool{
		"vm-err": true, "vm-draft": true, "vm-orphan-tpl": true, "vm-health": true,
	}
	for _, moref := range vc.destroySeen {
		if !wantDestroyed[moref] {
			t.Errorf("unexpected destroy of %s", moref)
		}
		delete(wantDestroyed, moref)
	}
	for moref := range wantDestroyed {
		t.Errorf("missing destroy of %s", moref)
	}
	if len(db.cleared) != 2 {
		t.Errorf("cleared morefs = %v; want 2", db.cleared)
	}
}

func TestReconcileTemplateOrphans_SkipsYoungStaleRowsViaDBFilter(t *testing.T) {
	// The inactivity filter lives in the DB query; the reconciler must not
	// destroy whatever the query returns as "not stale". Prove by returning
	// an empty stale list while inventory still has an owned active template.
	vc := &fakeTemplateOrphanVC{
		vms: []vcenter.FolderVM{
			{MoRef: "vm-live", Name: "tpl-live-aaaaaa"},
		},
	}
	db := &fakeTemplateOrphanDB{
		owned: map[string]struct{}{"vm-live": {}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	counts, err := reconcileTemplateOrphans(context.Background(), vc, db, logger,
		TemplateOrphanReconcilerConfig{Folder: "/DC/vm/templates"}, time.Now)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if counts.Destroyed != 0 || len(vc.destroySeen) != 0 {
		t.Fatalf("destroyed live template: counts=%+v destroys=%v", counts, vc.destroySeen)
	}
	if counts.SkippedOwned != 1 {
		t.Errorf("skipped_owned=%d; want 1", counts.SkippedOwned)
	}
}

func TestReconcileTemplateOrphans_DestroyFailureCounted(t *testing.T) {
	fixedNow := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	old := fixedNow.Add(-25 * time.Hour)
	id := uuid.New()
	vc := &fakeTemplateOrphanVC{
		destroyErr: map[string]error{"vm-err": errors.New("boom")},
	}
	db := &fakeTemplateOrphanDB{
		stale: []database.StaleWizardTemplateVM{
			{ID: id, Name: "failed", State: models.TemplateStateError, VCenterVMID: "vm-err", UpdatedAt: old},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	counts, err := reconcileTemplateOrphans(context.Background(), vc, db, logger,
		TemplateOrphanReconcilerConfig{Folder: ""}, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if counts.DestroyFailures != 1 || counts.Destroyed != 0 {
		t.Fatalf("counts=%+v; want 1 failure and 0 destroyed", counts)
	}
	if len(db.cleared) != 0 {
		t.Fatalf("must not clear moref after destroy failure; got %v", db.cleared)
	}
}

// TestReconcileTemplateOrphans_SkipsPoweredOnStagingVM is the data-loss guard.
//
// A manual ISO provision that failed on a slow datastore leaves the row in
// `error` with its moref intact while the staging VM stays perfectly usable —
// and an operator may well have finished installing an OS on it through the
// vCenter console. destroyTemplateOrphanVM powers the VM off and ignores the
// result before deleting it, so without this check the pass destroys that work
// 24h later with nothing to review. A powered-on VM must be retained and
// counted so an operator disposes of it deliberately via /cancel.
func TestReconcileTemplateOrphans_SkipsPoweredOnStagingVM(t *testing.T) {
	fixedNow := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	old := fixedNow.Add(-25 * time.Hour)
	installedID := uuid.New()
	abandonedID := uuid.New()

	vc := &fakeTemplateOrphanVC{
		poweredOn: map[string]bool{"vm-27390": true},
	}
	db := &fakeTemplateOrphanDB{
		stale: []database.StaleWizardTemplateVM{
			// The Mint case: errored template, VM running a finished install.
			{ID: installedID, Name: "Linux Mint 22.3 MATE", State: models.TemplateStateError, VCenterVMID: "vm-27390", UpdatedAt: old},
			// A genuinely abandoned shell, powered off — still cleaned up.
			{ID: abandonedID, Name: "abandoned", State: models.TemplateStateDraft, VCenterVMID: "vm-dead", UpdatedAt: old},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	counts, err := reconcileTemplateOrphans(context.Background(), vc, db, logger,
		TemplateOrphanReconcilerConfig{Folder: ""}, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	for _, moref := range vc.destroySeen {
		if moref == "vm-27390" {
			t.Fatal("destroyed a powered-on staging VM that may hold a completed console install")
		}
	}
	if counts.SkippedPoweredOn != 1 {
		t.Errorf("skipped_powered_on=%d; want 1 so the retained VM is visible in metrics", counts.SkippedPoweredOn)
	}
	if counts.StaleFailed != 0 {
		t.Errorf("stale_failed=%d; want 0 — the errored row was retained, not destroyed", counts.StaleFailed)
	}
	// The moref must survive so the row keeps pointing at the VM an operator
	// still has to deal with.
	for _, cleared := range db.cleared {
		if cleared == installedID.String()+":vm-27390" {
			t.Error("cleared the moref of a retained VM; the row would lose track of it")
		}
	}
	// The abandoned shell is the reaper's actual job and must still go.
	if counts.Destroyed != 1 || len(vc.destroySeen) != 1 || vc.destroySeen[0] != "vm-dead" {
		t.Errorf("destroyed=%d seen=%v; want only the powered-off abandoned shell", counts.Destroyed, vc.destroySeen)
	}
}

// TestReconcileTemplateOrphans_SkipsWhenPowerStateUnknown keeps the guard
// fail-closed: an unreadable power state is not permission to assume the VM is
// idle.
func TestReconcileTemplateOrphans_SkipsWhenPowerStateUnknown(t *testing.T) {
	fixedNow := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	old := fixedNow.Add(-25 * time.Hour)
	id := uuid.New()

	vc := &fakeTemplateOrphanVC{
		guestInfoErr: map[string]error{"vm-unknown": errors.New("ServerFaultCode: connection reset by peer")},
	}
	db := &fakeTemplateOrphanDB{
		stale: []database.StaleWizardTemplateVM{
			{ID: id, Name: "unreadable", State: models.TemplateStateError, VCenterVMID: "vm-unknown", UpdatedAt: old},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	counts, err := reconcileTemplateOrphans(context.Background(), vc, db, logger,
		TemplateOrphanReconcilerConfig{Folder: ""}, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(vc.destroySeen) != 0 {
		t.Fatalf("destroyed a VM whose power state could not be read; got %v", vc.destroySeen)
	}
	if counts.SkippedUndetermined != 1 {
		t.Errorf("skipped_undetermined=%d; want 1", counts.SkippedUndetermined)
	}
	if counts.DestroyFailures != 0 {
		t.Errorf("destroy_failures=%d; a skip is not a destroy failure", counts.DestroyFailures)
	}
}

// TestReconcileTemplateOrphans_DestroysWhenVMAlreadyGone proves the guard does
// not strand rows whose VM no longer exists. A missing VM must not read as
// "cannot determine power state" forever, or the dangling moref is never
// cleared.
func TestReconcileTemplateOrphans_DestroysWhenVMAlreadyGone(t *testing.T) {
	fixedNow := time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC)
	old := fixedNow.Add(-25 * time.Hour)
	id := uuid.New()

	vc := &fakeTemplateOrphanVC{
		guestInfoErr: map[string]error{
			"vm-deleted": errors.New("ServerFaultCode: ManagedObjectNotFound: could not be found"),
		},
	}
	db := &fakeTemplateOrphanDB{
		stale: []database.StaleWizardTemplateVM{
			{ID: id, Name: "already-gone", State: models.TemplateStateError, VCenterVMID: "vm-deleted", UpdatedAt: old},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	counts, err := reconcileTemplateOrphans(context.Background(), vc, db, logger,
		TemplateOrphanReconcilerConfig{Folder: ""}, func() time.Time { return fixedNow })
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if counts.SkippedUndetermined != 0 {
		t.Errorf("skipped_undetermined=%d; a deleted VM is not an unreadable one", counts.SkippedUndetermined)
	}
	if len(db.cleared) != 1 {
		t.Errorf("cleared=%v; want the dangling moref cleared", db.cleared)
	}
}

func TestIsDisposableTemplateOrphanName(t *testing.T) {
	cases := map[string]bool{
		"tpl-ubuntu-abc123":        true,
		"crucible-healthcheck-xyz": true,
		"smoke-aaaa-bbbb":          true,
		"manual-vm":                false,
		"student-windows-11":       false,
		"tplrep-source-1":          false,
		"":                         false,
	}
	for name, want := range cases {
		if got := isDisposableTemplateOrphanName(name); got != want {
			t.Errorf("isDisposableTemplateOrphanName(%q)=%v want %v", name, got, want)
		}
	}
}

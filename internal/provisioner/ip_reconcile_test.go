package provisioner

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// fakeIPVC implements ipReconcileVCenter for tests.
type fakeIPVC struct {
	info map[string]*vcenter.GuestInfo
	err  map[string]error
	seen []string
}

func (f *fakeIPVC) GetGuestInfo(_ context.Context, moref string) (*vcenter.GuestInfo, error) {
	f.seen = append(f.seen, moref)
	if err, ok := f.err[moref]; ok {
		return nil, err
	}
	if gi, ok := f.info[moref]; ok {
		return gi, nil
	}
	return &vcenter.GuestInfo{}, nil
}

// fakeIPDB implements ipReconcileDB for tests.
type fakeIPDB struct {
	cands     []database.PodVMIPCandidate
	listErr   error
	updated   map[uuid.UUID]string
	updateErr map[uuid.UUID]error
}

func (d *fakeIPDB) ListActivePodVMsForIPRefresh(_ context.Context) ([]database.PodVMIPCandidate, error) {
	if d.listErr != nil {
		return nil, d.listErr
	}
	return d.cands, nil
}

func (d *fakeIPDB) UpdatePodVMIP(_ context.Context, id uuid.UUID, ip string) error {
	if err, ok := d.updateErr[id]; ok {
		return err
	}
	if d.updated == nil {
		d.updated = make(map[uuid.UUID]string)
	}
	d.updated[id] = ip
	return nil
}

func TestReconcilePodVMIPs(t *testing.T) {
	stale := uuid.New()   // stored IP is out of date -> should update
	missing := uuid.New() // stored IP empty (missed at provisioning) -> should update
	same := uuid.New()    // stored IP matches live -> unchanged
	noip := uuid.New()    // guest not reporting an IPv4 -> skipped, not blanked
	readErr := uuid.New() // vCenter read fails -> counted as error
	writeErr := uuid.New() // DB write fails -> counted as error

	db := &fakeIPDB{
		cands: []database.PodVMIPCandidate{
			{PodVMID: stale, VCenterVMID: "vm-1", CurrentIP: "10.0.0.5"},
			{PodVMID: missing, VCenterVMID: "vm-2", CurrentIP: ""},
			{PodVMID: same, VCenterVMID: "vm-3", CurrentIP: "10.0.0.7"},
			{PodVMID: noip, VCenterVMID: "vm-4", CurrentIP: "10.0.0.8"},
			{PodVMID: readErr, VCenterVMID: "vm-5", CurrentIP: ""},
			{PodVMID: writeErr, VCenterVMID: "vm-6", CurrentIP: "10.0.0.1"},
		},
		updateErr: map[uuid.UUID]error{writeErr: errors.New("db down")},
	}
	vc := &fakeIPVC{
		info: map[string]*vcenter.GuestInfo{
			"vm-1": {IPAddress: "10.0.0.50"},
			"vm-2": {IPAddress: "10.0.0.51"},
			"vm-3": {IPAddress: "10.0.0.7"},
			"vm-4": {IPAddress: ""},
			"vm-6": {IPAddress: "10.0.0.99"},
		},
		err: map[string]error{"vm-5": errors.New("timeout")},
	}

	counts, err := reconcilePodVMIPs(context.Background(), vc, db, slog.Default(), IPReconcilerConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if counts.Candidates != 6 {
		t.Errorf("Candidates = %d, want 6", counts.Candidates)
	}
	if counts.Updated != 2 {
		t.Errorf("Updated = %d, want 2", counts.Updated)
	}
	if counts.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", counts.Unchanged)
	}
	if counts.NoIP != 1 {
		t.Errorf("NoIP = %d, want 1", counts.NoIP)
	}
	if counts.Errors != 2 {
		t.Errorf("Errors = %d, want 2", counts.Errors)
	}

	if got := db.updated[stale]; got != "10.0.0.50" {
		t.Errorf("stale VM updated to %q, want 10.0.0.50", got)
	}
	if got := db.updated[missing]; got != "10.0.0.51" {
		t.Errorf("missing VM updated to %q, want 10.0.0.51", got)
	}
	if _, ok := db.updated[same]; ok {
		t.Errorf("unchanged VM should not have been written")
	}
	if _, ok := db.updated[noip]; ok {
		t.Errorf("no-IP VM must never be written (would blank a good address)")
	}
}

func TestReconcilePodVMIPs_ListError(t *testing.T) {
	db := &fakeIPDB{listErr: errors.New("boom")}
	vc := &fakeIPVC{}
	if _, err := reconcilePodVMIPs(context.Background(), vc, db, slog.Default(), IPReconcilerConfig{}); err == nil {
		t.Fatal("expected error when listing candidates fails")
	}
}

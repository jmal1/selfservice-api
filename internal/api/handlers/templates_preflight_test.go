package handlers

// templates_preflight_test.go -- Handler-level tests for the preflight gate.
//
// Tests:
//  1. Blocking failure -> 409 with results list in body.
//  2. Warnings alone -> 202 (provisioning proceeds).
//  3. Admin override bypasses a blocking failure -> 202.
//  4. Non-admin instructor cannot use the override -> 403.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/jmal1/selfservice-api/internal/middleware"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/vcenter/preflight"
)

// ---------------------------------------------------------------------------
// Fake PreflightVCenter seam
// ---------------------------------------------------------------------------

// stubPreflightVC is a minimal PreflightVCenter. Behaviour is controlled via
// optional closure fields; unset closures return safe defaults.
type stubPreflightVC struct {
	fetchVMPropsFunc        func(ctx context.Context, moref string) (*mo.VirtualMachine, error)
	datastoreHostMorefsFunc func(ctx context.Context, ds string) ([]string, error)
}

func (s *stubPreflightVC) FetchVMProps(ctx context.Context, moref string) (*mo.VirtualMachine, error) {
	if s.fetchVMPropsFunc != nil {
		return s.fetchVMPropsFunc(ctx, moref)
	}
	return &mo.VirtualMachine{}, nil
}
func (s *stubPreflightVC) DatastoreInfo(_ context.Context, _ string) (*mo.Datastore, error) {
	return &mo.Datastore{
		Summary: types.DatastoreSummary{FreeSpace: 100 * (1 << 30)},
	}, nil
}
func (s *stubPreflightVC) InFlightTasksForVM(_ context.Context, _ string) ([]types.TaskInfo, error) {
	return nil, nil
}
func (s *stubPreflightVC) VMExistsInFolder(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (s *stubPreflightVC) DatastoreFileExists(_ context.Context, _, _ string) (bool, error) {
	return true, nil
}
func (s *stubPreflightVC) HostPortGroupNames(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}
func (s *stubPreflightVC) ClusterNameForHost(_ context.Context, _ string) (string, error) {
	return "TestCluster", nil
}
func (s *stubPreflightVC) DatastoreHostMorefs(ctx context.Context, ds string) ([]string, error) {
	if s.datastoreHostMorefsFunc != nil {
		return s.datastoreHostMorefsFunc(ctx, ds)
	}
	return nil, nil
}
func (s *stubPreflightVC) ClusterHostMorefs(_ context.Context, moref string) ([]string, error) {
	return []string{moref}, nil
}

// blockingStubVC returns an error for FetchVMProps so PF-01 blocks.
func blockingStubVC() *stubPreflightVC {
	return &stubPreflightVC{
		fetchVMPropsFunc: func(_ context.Context, _ string) (*mo.VirtualMachine, error) {
			return nil, errors.New("managed object not found: vm-missing")
		},
	}
}

// warningOnlyStubVC returns a healthy VM with empty guest credentials so
// PF-08 (severity=warn) fires, but all block checks pass.
// DatastoreHostMorefs returns the same host moref as the cluster so PF-03
// does not block.
func warningOnlyStubVC() *stubPreflightVC {
	return &stubPreflightVC{
		fetchVMPropsFunc: func(_ context.Context, _ string) (*mo.VirtualMachine, error) {
			return &mo.VirtualMachine{
				Runtime: types.VirtualMachineRuntimeInfo{
					Host: &types.ManagedObjectReference{Type: "HostSystem", Value: "host-1"},
				},
				Guest: &types.GuestInfo{
					ToolsRunningStatus: string(types.VirtualMachineToolsRunningStatusGuestToolsRunning),
				},
				Config: &types.VirtualMachineConfigInfo{},
				Summary: types.VirtualMachineSummary{
					Storage: &types.VirtualMachineStorageSummary{
						Committed:   int64(10 * 1 << 30),
						Uncommitted: 0,
					},
				},
			}, nil
		},
		// PF-03: datastore is mounted on host-1, so cluster check passes.
		datastoreHostMorefsFunc: func(_ context.Context, _ string) ([]string, error) {
			return []string{"host-1"}, nil
		},
	}
}

// buildTestPreflightHandler creates a minimal Handler with the given
// PreflightVCenter seam and a discard logger. db is nil -- tests that do not
// reach audit.Log are safe; runPreflightGate guards the audit call.
func buildTestPreflightHandler(vc PreflightVCenter) *Handler {
	return &Handler{
		logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		vcPreflight: vc,
		preflightCfg: PreflightConfig{
			DatastoreName:               "test-ds",
			TemplateFolder:              "DC/vm/Templates",
			ConfiguredResourcePoolPaths: []string{"TestCluster/Resources/Pool"},
		},
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestPreflightGate_BlockFailures: blocking failure -> 409, results in body.
func TestPreflightGate_BlockFailures(t *testing.T) {
	h := buildTestPreflightHandler(blockingStubVC())
	tmpl := &models.Template{
		SourceType: models.TemplateSourceCloneVCenter,
		SourceRef:  "vm-missing",
	}

	rec := httptest.NewRecorder()
	req := middleware.WithRoleForTest(
		httptest.NewRequest(http.MethodPost, "/provision", nil),
		models.RoleInstructor,
	)

	_, blocked := h.runPreflightGate(rec, req, tmpl, "tpl-test", false)
	if !blocked {
		t.Fatal("runPreflightGate should return blocked=true when a block check fails")
	}
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d; want 409", rec.Code)
	}

	var body struct {
		Error            string             `json:"error"`
		PreflightBlocked bool               `json:"preflight_blocked"`
		Results          []preflight.Result `json:"results"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "preflight_blocked" {
		t.Errorf("error field = %q; want preflight_blocked", body.Error)
	}
	if !body.PreflightBlocked {
		t.Error("preflight_blocked should be true in body")
	}
	if len(body.Results) == 0 {
		t.Error("results list should be non-empty")
	}
	var foundPF01 bool
	for _, r := range body.Results {
		if r.ID == "PF-01" {
			foundPF01 = true
			if r.OK {
				t.Error("PF-01 should be failing in block scenario")
			}
		}
	}
	if !foundPF01 {
		t.Error("PF-01 should appear in the results list")
	}
}

// TestPreflightGate_WarningsAllow: warnings alone must not block provisioning.
func TestPreflightGate_WarningsAllow(t *testing.T) {
	h := buildTestPreflightHandler(warningOnlyStubVC())
	tmpl := &models.Template{
		SourceType:     models.TemplateSourceCloneVCenter,
		SourceRef:      "vm-1",
		StagingNetwork: "PG-VM-Lab",
		// DefaultUsername / DefaultPassword intentionally empty -> PF-08 warn.
	}

	rec := httptest.NewRecorder()
	req := middleware.WithRoleForTest(
		httptest.NewRequest(http.MethodPost, "/provision", nil),
		models.RoleInstructor,
	)

	_, blocked := h.runPreflightGate(rec, req, tmpl, "tpl-test", false)
	if blocked {
		t.Fatalf("runPreflightGate must NOT block on warn-only failures; got status=%d body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestPreflightGate_AdminOverride: admin with override=true bypasses blocks.
func TestPreflightGate_AdminOverride(t *testing.T) {
	h := buildTestPreflightHandler(blockingStubVC())
	tmpl := &models.Template{
		SourceType: models.TemplateSourceCloneVCenter,
		SourceRef:  "vm-missing",
	}

	rec := httptest.NewRecorder()
	req := middleware.WithRoleForTest(
		httptest.NewRequest(http.MethodPost, "/provision",
			bytes.NewBufferString(`{"override_preflight_blocks":true}`)),
		models.RoleAdmin,
	)

	_, blocked := h.runPreflightGate(rec, req, tmpl, "tpl-test", true)
	if blocked {
		t.Fatalf("runPreflightGate should NOT block when admin uses override; status=%d body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestPreflightGate_InstructorCannotOverride: instructor with override=true -> 403.
func TestPreflightGate_InstructorCannotOverride(t *testing.T) {
	h := buildTestPreflightHandler(blockingStubVC())
	tmpl := &models.Template{
		SourceType: models.TemplateSourceCloneVCenter,
		SourceRef:  "vm-missing",
	}

	rec := httptest.NewRecorder()
	req := middleware.WithRoleForTest(
		httptest.NewRequest(http.MethodPost, "/provision",
			bytes.NewBufferString(`{"override_preflight_blocks":true}`)),
		models.RoleInstructor,
	)

	_, blocked := h.runPreflightGate(rec, req, tmpl, "tpl-test", true)
	if !blocked {
		t.Fatal("runPreflightGate should block when a non-admin uses the override flag")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rec.Code)
	}
}

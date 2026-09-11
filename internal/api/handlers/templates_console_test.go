package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// Phase G unit tests for the template build-VM console handler.
//
// These cover the pure-function bits that don't need a database
// fixture: state gating, auth matrix, and the response JSON shape.
// DB-dependent paths (GetTemplateByID nil/error) are covered by the
// integration suite and the Phase G9 cluster verification.

func TestTemplateConsoleStates_OnlyInteractiveLifecycleStates(t *testing.T) {
	// templateConsoleStates must be exactly the three in-flight states
	// the wizard exposes to instructors. Anything else means either we
	// expose a console too early (no VM yet) or too late (VM already
	// converted/snapshotted away from the staging slot).
	want := map[string]bool{
		models.TemplateStateProvisioning: true,
		models.TemplateStateConfiguring:  true,
		models.TemplateStateGeneralizing: true,
	}
	if len(templateConsoleStates) != len(want) {
		gotKeys := make([]string, 0, len(templateConsoleStates))
		for k := range templateConsoleStates {
			gotKeys = append(gotKeys, k)
		}
		sort.Strings(gotKeys)
		t.Errorf("templateConsoleStates has %d entries, want %d (got %v)",
			len(templateConsoleStates), len(want), gotKeys)
	}
	for state := range want {
		if !templateConsoleStates[state] {
			t.Errorf("templateConsoleStates missing %q", state)
		}
	}
	// Explicitly verify the never-allowed states are absent — these
	// would be bugs if added (no VM yet, or VM gone).
	for _, never := range []string{
		models.TemplateStateDraft,
		models.TemplateStateReady,
		models.TemplateStateActive,
		models.TemplateStateError,
	} {
		if templateConsoleStates[never] {
			t.Errorf("templateConsoleStates must NOT include %q (no staging VM)", never)
		}
	}
}

func TestValidateTemplateConsoleAccess(t *testing.T) {
	ownerID := uuid.New()
	otherID := uuid.New()

	mkTmpl := func(state string, createdBy *uuid.UUID, vmID string) *models.Template {
		return &models.Template{
			ID:            uuid.New(),
			Name:          "test",
			TemplateState: state,
			CreatedBy:     createdBy,
			VCenterVMID:   vmID,
		}
	}

	tests := []struct {
		name       string
		tmpl       *models.Template
		userID     uuid.UUID
		role       string
		wantStatus int // 0 = allowed
	}{
		{
			name:       "admin can access any in-flight template",
			tmpl:       mkTmpl(models.TemplateStateConfiguring, &ownerID, "vm-123"),
			userID:     otherID,
			role:       models.RoleAdmin,
			wantStatus: 0,
		},
		{
			name:       "admin can access NULL created_by template",
			tmpl:       mkTmpl(models.TemplateStateConfiguring, nil, "vm-123"),
			userID:     otherID,
			role:       models.RoleAdmin,
			wantStatus: 0,
		},
		{
			name:       "owner can access their own template",
			tmpl:       mkTmpl(models.TemplateStateConfiguring, &ownerID, "vm-123"),
			userID:     ownerID,
			role:       models.RoleInstructor,
			wantStatus: 0,
		},
		{
			name:       "non-owner instructor is forbidden",
			tmpl:       mkTmpl(models.TemplateStateConfiguring, &ownerID, "vm-123"),
			userID:     otherID,
			role:       models.RoleInstructor,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "instructor cannot access NULL created_by template",
			tmpl:       mkTmpl(models.TemplateStateConfiguring, nil, "vm-123"),
			userID:     ownerID,
			role:       models.RoleInstructor,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "provisioning state allowed",
			tmpl:       mkTmpl(models.TemplateStateProvisioning, &ownerID, "vm-123"),
			userID:     ownerID,
			role:       models.RoleInstructor,
			wantStatus: 0,
		},
		{
			name:       "generalizing state allowed",
			tmpl:       mkTmpl(models.TemplateStateGeneralizing, &ownerID, "vm-123"),
			userID:     ownerID,
			role:       models.RoleInstructor,
			wantStatus: 0,
		},
		{
			name:       "draft state rejected with conflict",
			tmpl:       mkTmpl(models.TemplateStateDraft, &ownerID, "vm-123"),
			userID:     ownerID,
			role:       models.RoleInstructor,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "ready state rejected with conflict",
			tmpl:       mkTmpl(models.TemplateStateReady, &ownerID, "vm-123"),
			userID:     ownerID,
			role:       models.RoleInstructor,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "active state rejected with conflict",
			tmpl:       mkTmpl(models.TemplateStateActive, &ownerID, "vm-123"),
			userID:     ownerID,
			role:       models.RoleInstructor,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "error state rejected with conflict",
			tmpl:       mkTmpl(models.TemplateStateError, &ownerID, "vm-123"),
			userID:     ownerID,
			role:       models.RoleInstructor,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "missing vCenter VM rejected with conflict (even when admin + valid state)",
			tmpl:       mkTmpl(models.TemplateStateConfiguring, &ownerID, ""),
			userID:     ownerID,
			role:       models.RoleAdmin,
			wantStatus: http.StatusConflict,
		},
		{
			name:       "auth check runs BEFORE state check (forbidden, not conflict, when both fail)",
			tmpl:       mkTmpl(models.TemplateStateDraft, &ownerID, ""),
			userID:     otherID,
			role:       models.RoleInstructor,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, status := validateTemplateConsoleAccess(tt.tmpl, tt.userID, tt.role)
			if status != tt.wantStatus {
				t.Errorf("validateTemplateConsoleAccess() status = %d (%q); want %d",
					status, msg, tt.wantStatus)
			}
			if tt.wantStatus == 0 && msg != "" {
				t.Errorf("allowed access returned non-empty message: %q", msg)
			}
			if tt.wantStatus != 0 && msg == "" {
				t.Errorf("rejected access returned empty message (status=%d)", status)
			}
		})
	}
}

func TestTemplateConsoleTicketResponse_JSONShape(t *testing.T) {
	// The TS frontend depends on these exact field names. If you
	// change them, also update TemplateConsoleTicket in
	// selfservice-ui/src/lib/api/client.ts.
	resp := TemplateConsoleTicketResponse{
		WSURL:         "/api/v1/admin/templates/abc/console/ws",
		TemplateState: "configuring",
		VMName:        "ubuntu-24-04",
	}
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	wantKeys := []string{"ws_url", "template_state", "vm_name"}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in JSON; got %v", k, got)
		}
	}
	if len(got) != len(wantKeys) {
		t.Errorf("unexpected keys in JSON: got %v, want exactly %v", got, wantKeys)
	}
}

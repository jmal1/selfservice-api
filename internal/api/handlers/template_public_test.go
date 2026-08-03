package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// contextKey is a type for context keys (matches middleware/auth.go pattern).
type contextKey string

func TestTemplatePublicMarshalDoesNotLeakPassword(t *testing.T) {
	// Test that marshalling a TemplatePublic built from a Template with
	// a distinctive password does NOT include the password in the JSON output.
	// This is the critical assertion — it verifies the fix works on the wire.

	secretPassword := "SUPERSECRET-DO-NOT-LEAK"

	tmpl := models.Template{
		ID:              uuid.New(),
		Name:            "Test Template",
		VCenterTemplate: "test-vcenter",
		OSType:          "ubuntu-22-04",
		DefaultVCPUs:    2,
		DefaultRAMMB:    2048,
		DefaultDiskGB:   20,
		MinVCPUs:        1,
		MinRAMMB:        1024,
		Description:     "A test template",
		IconURL:         "https://example.com/icon.png",
		DefaultUsername: "testuser",
		DefaultPassword: secretPassword,
		Kind:            "clone_no_customize",
		AssignIP:        true,
		IsActive:        true,
		IsInternal:      false,
		TemplateState:   "active",
		CreatedBy:       nil,
		VCenterVMID:     "vm-123",
		SourceType:      "clone_template",
		SourceRef:       "src-123",
		StagingNetwork:  "lab-vlan",
		UnattendMode:    "",
		UnattendConfig:  nil,
		GuestID:         "ubuntu64Guest",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	// Convert to DTO
	public := newTemplatePublic(tmpl)

	// Marshal to JSON bytes
	data, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("Failed to marshal TemplatePublic: %v", err)
	}

	// Assert that the raw bytes do NOT contain the password
	if bytes.Contains(data, []byte(secretPassword)) {
		t.Errorf("LEAK: marshalled TemplatePublic contains the plaintext password %q", secretPassword)
	}

	// Assert that the raw bytes do NOT contain the "default_password" key at all
	if bytes.Contains(data, []byte("default_password")) {
		t.Error("LEAK: marshalled TemplatePublic contains the key 'default_password'")
	}
}

func TestTemplatePublicMarshalIncludesAllRequiredFields(t *testing.T) {
	// Test that the DTO's marshalled JSON still contains all the fields
	// the UI depends on. This ensures we didn't accidentally drop a field.

	tmpl := models.Template{
		ID:              uuid.New(),
		Name:            "Test Template",
		VCenterTemplate: "test-vcenter",
		OSType:          "ubuntu-22-04",
		DefaultVCPUs:    2,
		DefaultRAMMB:    2048,
		DefaultDiskGB:   20,
		MinVCPUs:        1,
		MinRAMMB:        1024,
		Description:     "A test template",
		IconURL:         "https://example.com/icon.png",
		DefaultUsername: "testuser",
		DefaultPassword: "secret",
		Kind:            "clone_no_customize",
		AssignIP:        true,
		IsActive:        true,
		IsInternal:      false,
		TemplateState:   "active",
		CreatedBy:       nil,
		VCenterVMID:     "vm-123",
		SourceType:      "clone_template",
		SourceRef:       "src-123",
		StagingNetwork:  "lab-vlan",
		UnattendMode:    "",
		UnattendConfig:  nil,
		GuestID:         "ubuntu64Guest",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	public := newTemplatePublic(tmpl)
	data, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("Failed to marshal TemplatePublic: %v", err)
	}

	// Assert all required fields are present in the JSON
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal JSON: %v", err)
	}

	requiredFields := []string{
		"id",
		"name",
		"os_type",
		"default_username",
		"default_vcpus",
		"default_ram_mb",
		"default_disk_gb",
		"min_vcpus",
		"min_ram_mb",
		"description",
		"icon_url",
		"kind",
		"assign_ip",
		"is_active",
		"is_internal",
		"template_state",
		"vcenter_vm_id",
		"source_type",
		"source_ref",
		"staging_network",
		"vcenter_template",
		"created_at",
		"updated_at",
	}

	for _, field := range requiredFields {
		if _, ok := parsed[field]; !ok {
			t.Errorf("Missing required field in TemplatePublic JSON: %s", field)
		}
	}
}

func TestTemplatePublicFieldPreservation(t *testing.T) {
	// Test that converting a Template to TemplatePublic preserves all fields
	// except DefaultPassword.

	createdBy := uuid.New()

	tmpl := models.Template{
		ID:              uuid.New(),
		Name:            "Preservation Test",
		VCenterTemplate: "vcenter-ref",
		OSType:          "windows-2022",
		DefaultVCPUs:    4,
		DefaultRAMMB:    8192,
		DefaultDiskGB:   100,
		MinVCPUs:        2,
		MinRAMMB:        4096,
		Description:     "Test description",
		IconURL:         "https://example.com/icon.png",
		DefaultUsername: "admin",
		DefaultPassword: "should-not-appear",
		Kind:            "registered_existing_vm",
		AssignIP:        false,
		IsActive:        false,
		IsInternal:      true,
		TemplateState:   "draft",
		CreatedBy:       &createdBy,
		VCenterVMID:     "vm-999",
		SourceType:      "clone_vcenter",
		SourceRef:       "ref-999",
		StagingNetwork:  "staging-vlan",
		UnattendMode:    "sysprep",
		UnattendConfig:  json.RawMessage(`{"locale":"en-US"}`),
		GuestID:         "windows9_64Guest",
		CreatedAt:       time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
		UpdatedAt:       time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC),
	}

	public := newTemplatePublic(tmpl)

	// Check all fields (except DefaultPassword) are preserved
	if public.ID != tmpl.ID {
		t.Errorf("ID mismatch: %v != %v", public.ID, tmpl.ID)
	}
	if public.Name != tmpl.Name {
		t.Errorf("Name mismatch: %s != %s", public.Name, tmpl.Name)
	}
	if public.VCenterTemplate != tmpl.VCenterTemplate {
		t.Errorf("VCenterTemplate mismatch")
	}
	if public.OSType != tmpl.OSType {
		t.Errorf("OSType mismatch")
	}
	if public.DefaultVCPUs != tmpl.DefaultVCPUs {
		t.Errorf("DefaultVCPUs mismatch")
	}
	if public.DefaultRAMMB != tmpl.DefaultRAMMB {
		t.Errorf("DefaultRAMMB mismatch")
	}
	if public.DefaultDiskGB != tmpl.DefaultDiskGB {
		t.Errorf("DefaultDiskGB mismatch")
	}
	if public.MinVCPUs != tmpl.MinVCPUs {
		t.Errorf("MinVCPUs mismatch")
	}
	if public.MinRAMMB != tmpl.MinRAMMB {
		t.Errorf("MinRAMMB mismatch")
	}
	if public.Description != tmpl.Description {
		t.Errorf("Description mismatch")
	}
	if public.IconURL != tmpl.IconURL {
		t.Errorf("IconURL mismatch")
	}
	if public.DefaultUsername != tmpl.DefaultUsername {
		t.Errorf("DefaultUsername mismatch: %s != %s", public.DefaultUsername, tmpl.DefaultUsername)
	}
	if public.Kind != tmpl.Kind {
		t.Errorf("Kind mismatch")
	}
	if public.AssignIP != tmpl.AssignIP {
		t.Errorf("AssignIP mismatch")
	}
	if public.IsActive != tmpl.IsActive {
		t.Errorf("IsActive mismatch")
	}
	if public.IsInternal != tmpl.IsInternal {
		t.Errorf("IsInternal mismatch")
	}
	if public.TemplateState != tmpl.TemplateState {
		t.Errorf("TemplateState mismatch")
	}
	if public.CreatedBy == nil || *public.CreatedBy != *tmpl.CreatedBy {
		t.Errorf("CreatedBy mismatch")
	}
	if public.VCenterVMID != tmpl.VCenterVMID {
		t.Errorf("VCenterVMID mismatch")
	}
	if public.SourceType != tmpl.SourceType {
		t.Errorf("SourceType mismatch")
	}
	if public.SourceRef != tmpl.SourceRef {
		t.Errorf("SourceRef mismatch")
	}
	if public.StagingNetwork != tmpl.StagingNetwork {
		t.Errorf("StagingNetwork mismatch")
	}
	if public.UnattendMode != tmpl.UnattendMode {
		t.Errorf("UnattendMode mismatch")
	}
	if !bytes.Equal(public.UnattendConfig, tmpl.UnattendConfig) {
		t.Errorf("UnattendConfig mismatch")
	}
	if public.GuestID != tmpl.GuestID {
		t.Errorf("GuestID mismatch")
	}
	if public.CreatedAt != tmpl.CreatedAt {
		t.Errorf("CreatedAt mismatch")
	}
	if public.UpdatedAt != tmpl.UpdatedAt {
		t.Errorf("UpdatedAt mismatch")
	}
}

func TestNewTemplatePublicList(t *testing.T) {
	// Test that newTemplatePublicList correctly converts a slice.

	tmpl1 := models.Template{ID: uuid.New(), DefaultPassword: "secret1"}
	tmpl2 := models.Template{ID: uuid.New(), DefaultPassword: "secret2"}
	tmpl3 := models.Template{ID: uuid.New(), DefaultPassword: "secret3"}

	result := newTemplatePublicList([]models.Template{tmpl1, tmpl2, tmpl3})

	if len(result) != 3 {
		t.Errorf("Expected 3 items, got %d", len(result))
	}

	if result[0].ID != tmpl1.ID || result[1].ID != tmpl2.ID || result[2].ID != tmpl3.ID {
		t.Error("IDs not preserved in list conversion")
	}
}

// TestPositiveControl_RawTemplateContainsPassword is the positive control.
// It verifies that a raw models.Template DOES leak the password when marshalled.
// This proves the test infrastructure is capable of detecting the vulnerability;
// without this, TestTemplatePublicMarshalDoesNotLeakPassword could pass for the
// wrong reason (test infrastructure broken, not fix working).
func TestPositiveControl_RawTemplateContainsPassword(t *testing.T) {
	secretPassword := "SUPERSECRET-DO-NOT-LEAK"

	tmpl := models.Template{
		ID:              uuid.New(),
		Name:            "Test Template",
		VCenterTemplate: "test-vcenter",
		OSType:          "ubuntu-22-04",
		DefaultVCPUs:    2,
		DefaultRAMMB:    2048,
		DefaultDiskGB:   20,
		MinVCPUs:        1,
		MinRAMMB:        1024,
		Description:     "A test template",
		IconURL:         "https://example.com/icon.png",
		DefaultUsername: "testuser",
		DefaultPassword: secretPassword,
		Kind:            "clone_no_customize",
		AssignIP:        true,
		IsActive:        true,
		IsInternal:      false,
		TemplateState:   "active",
		CreatedBy:       nil,
		VCenterVMID:     "vm-123",
		SourceType:      "clone_template",
		SourceRef:       "src-123",
		StagingNetwork:  "lab-vlan",
		UnattendMode:    "",
		UnattendConfig:  nil,
		GuestID:         "ubuntu64Guest",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	// Marshal the raw template (this should leak the password)
	data, err := json.Marshal(tmpl)
	if err != nil {
		t.Fatalf("Failed to marshal Template: %v", err)
	}

	// Assert that the raw bytes DO contain the password
	if !bytes.Contains(data, []byte(secretPassword)) {
		t.Errorf("TESTBUG: marshalled models.Template does NOT contain the plaintext password %q; test is broken", secretPassword)
	}

	// Assert that the raw bytes DO contain the "default_password" key
	if !bytes.Contains(data, []byte("default_password")) {
		t.Error("TESTBUG: marshalled models.Template does NOT contain the key 'default_password'; test is broken")
	}
}

// fakeTemplateLister is a test double for templateLister.
type fakeTemplateLister struct {
	templates []models.Template
	err       error
}

func (f *fakeTemplateLister) ListTemplatesForUser(ctx context.Context, userID uuid.UUID, role string) ([]models.Template, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.templates, nil
}

func (f *fakeTemplateLister) ListExplicitTemplateAccessForUser(ctx context.Context, userID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	// Return an empty map; templates are not filtered as internal in this test.
	// If the template were internal and had no explicit access, it would be
	// filtered out before serialization — but our test template is not internal
	// (IsInternal: false), so it passes through regardless.
	return map[uuid.UUID]struct{}{}, nil
}

// TestListTemplatesHandlerDoesNotLeakPassword is an integration test that drives
// the ListTemplates handler end-to-end with a fake database. It verifies that
// the wire response (HTTP body bytes) does NOT contain the template password.
// This guards against the handler accidentally reverting to serializing the raw
// models.Template instead of the DTO.
func TestListTemplatesHandlerDoesNotLeakPassword(t *testing.T) {
	secretPassword := "SENTINEL-PW-DO-NOT-LEAK"

	// Build a template with the distinctive secret password
	tmpl := models.Template{
		ID:              uuid.New(),
		Name:            "Sensitive Template",
		VCenterTemplate: "prod-template",
		OSType:          "ubuntu-22-04",
		DefaultVCPUs:    4,
		DefaultRAMMB:    4096,
		DefaultDiskGB:   50,
		MinVCPUs:        2,
		MinRAMMB:        2048,
		Description:     "A template with a real password",
		IconURL:         "https://example.com/prod-icon.png",
		DefaultUsername: "ubuntu",
		DefaultPassword: secretPassword,
		Kind:            "clone_no_customize",
		AssignIP:        true,
		IsActive:        true,
		IsInternal:      false,
		TemplateState:   "active",
		CreatedBy:       nil,
		VCenterVMID:     "vm-prod-123",
		SourceType:      "clone_template",
		SourceRef:       "prod-src-123",
		StagingNetwork:  "prod-vlan",
		UnattendMode:    "",
		UnattendConfig:  nil,
		GuestID:         "ubuntu64Guest",
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}

	// Create a fake database that returns our sensitive template
	fake := &fakeTemplateLister{
		templates: []models.Template{tmpl},
		err:       nil,
	}

	// Create a handler with the fake wired in
	h := &Handler{
		db:       nil, // Not used; fake overrides it
		templates: fake,
		logger:   slog.New(slog.NewTextHandler(nil, nil)),
	}

	// Create a student-role request context using the same key values as middleware
	// See internal/middleware/auth.go for the context key definitions
	userID := uuid.New()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/templates", nil)
	
	ctx := context.WithValue(req.Context(), contextKey("user_id"), userID)
	ctx = context.WithValue(ctx, contextKey("role"), models.RoleStudent)
	req = req.WithContext(ctx)

	// Capture the response
	rec := httptest.NewRecorder()

	// Call the handler
	h.ListTemplates(rec, req)

	// Verify HTTP status is OK
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	// Get the raw response bytes
	body := rec.Body.Bytes()

	// Assert the password does NOT appear in the wire bytes
	if bytes.Contains(body, []byte(secretPassword)) {
		t.Errorf("FAIL: HTTP response body contains plaintext password %q; DTO leak detected", secretPassword)
	}

	// Assert the default_password key does NOT appear in the wire bytes
	if bytes.Contains(body, []byte("default_password")) {
		t.Errorf("FAIL: HTTP response body contains key 'default_password'; DTO leak detected")
	}

	// Sanity check: verify other fields are still present
	if !bytes.Contains(body, []byte("\"name\"")) {
		t.Error("SANITY FAIL: response missing 'name' field")
	}
	if !bytes.Contains(body, []byte("\"default_username\"")) {
		t.Error("SANITY FAIL: response missing 'default_username' field")
	}

	// CRITICAL: Assert that the template was actually serialized (not filtered out).
	// If the fake returned an empty list or the template was filtered away,
	// the response would be an empty array, and we'd pass this test for the
	// wrong reason (no secrets to leak = test passes even if the fix is reverted).
	// This guard prevents that trap.
	if !bytes.Contains(body, []byte("Sensitive Template")) {
		t.Error("TRAP: response does not contain template name; template was filtered out or list is empty")
	}
}

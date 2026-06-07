package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type fakeEnumerator struct {
	calls atomic.Int32
	vms   []vcenter.FolderVM
	err   error
}

func (f *fakeEnumerator) ListVMsInFolder(_ context.Context, _ string) ([]vcenter.FolderVM, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.vms, nil
}

func newHandlerWithStubs(t *testing.T, fe *fakeEnumerator, rows []vcenterTemplateRow) *TemplatesFolderHandler {
	t.Helper()
	return NewTemplatesFolderHandler(
		fe,
		"/JMAL-Datacenter/vm/Templates",
		func(ctx context.Context) ([]vcenterTemplateRow, error) { return rows, nil },
	)
}

func decode(t *testing.T, body []byte) templatesFolderResponse {
	t.Helper()
	var resp templatesFolderResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v\nbody: %s", err, string(body))
	}
	return resp
}

func TestTemplatesFolderHandler_HappyPath(t *testing.T) {
	fe := &fakeEnumerator{vms: []vcenter.FolderVM{
		{Name: "z-vm", MoRef: "vm-2", OSType: "linux"},
		{Name: "a-vm", MoRef: "vm-1", OSType: "windows"},
	}}
	h := newHandlerWithStubs(t, fe, []vcenterTemplateRow{
		{ID: "00000000-0000-0000-0000-000000000001", Name: "Registered A", VCenterTemplate: "a-vm"},
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/vcenter/templates-folder", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rr.Code, rr.Body.String())
	}
	resp := decode(t, rr.Body.Bytes())
	if got, want := len(resp.VMs), 2; got != want {
		t.Fatalf("len(VMs) = %d, want %d", got, want)
	}
	if resp.VMs[0].Name != "a-vm" || resp.VMs[1].Name != "z-vm" {
		t.Errorf("expected sorted by name, got %v", []string{resp.VMs[0].Name, resp.VMs[1].Name})
	}
	if resp.VMs[0].RegisteredTemplateID == nil || *resp.VMs[0].RegisteredTemplateID == "" {
		t.Errorf("a-vm should be registered: %+v", resp.VMs[0])
	}
	if resp.VMs[1].RegisteredTemplateID != nil {
		t.Errorf("z-vm should NOT be registered: %+v", resp.VMs[1])
	}
	if resp.Cached {
		t.Errorf("first call should not be cached")
	}
}

func TestTemplatesFolderHandler_CacheHit(t *testing.T) {
	fe := &fakeEnumerator{vms: []vcenter.FolderVM{{Name: "a"}}}
	h := newHandlerWithStubs(t, fe, nil)

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("iter %d: status = %d, body = %s", i, rr.Code, rr.Body.String())
		}
	}
	if got := fe.calls.Load(); got != 1 {
		t.Errorf("enumerator hit %d times; want 1 (cache should absorb the rest)", got)
	}
}

func TestTemplatesFolderHandler_ForceRefresh(t *testing.T) {
	fe := &fakeEnumerator{vms: []vcenter.FolderVM{{Name: "a"}}}
	h := newHandlerWithStubs(t, fe, nil)

	for i := 0; i < 3; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x?refresh=true", nil)
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("iter %d: status = %d", i, rr.Code)
		}
	}
	if got := fe.calls.Load(); got != 3 {
		t.Errorf("enumerator hit %d times; want 3 (refresh should bypass cache)", got)
	}
}

func TestTemplatesFolderHandler_VCenterError(t *testing.T) {
	fe := &fakeEnumerator{err: errors.New("vcenter exploded")}
	h := newHandlerWithStubs(t, fe, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "vcenter exploded") {
		t.Errorf("error not surfaced: %s", rr.Body.String())
	}
}

func TestTemplatesFolderHandler_NoEnumerator(t *testing.T) {
	h := NewTemplatesFolderHandler(nil, "/whatever", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestTemplatesFolderHandler_DBError(t *testing.T) {
	fe := &fakeEnumerator{vms: []vcenter.FolderVM{{Name: "a"}}}
	h := NewTemplatesFolderHandler(fe, "/x", func(ctx context.Context) ([]vcenterTemplateRow, error) {
		return nil, errors.New("db down")
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

func TestTemplateFolderCache_Expiry(t *testing.T) {
	c := &templateFolderCache{ttl: 10 * time.Millisecond}
	c.set([]vcenter.FolderVM{{Name: "x"}})

	if _, _, ok := c.get(); !ok {
		t.Fatal("expected cache hit immediately after set")
	}
	time.Sleep(15 * time.Millisecond)
	if _, _, ok := c.get(); ok {
		t.Fatal("expected cache miss after TTL")
	}
}

func TestTemplateFolderCache_Invalidate(t *testing.T) {
	c := &templateFolderCache{ttl: time.Hour}
	c.set([]vcenter.FolderVM{{Name: "x"}})
	c.Invalidate()
	if _, _, ok := c.get(); ok {
		t.Error("expected cache miss after Invalidate")
	}
}

func TestBuildRegistrationMap_SkipsEmptyVCenterTemplate(t *testing.T) {
	h := NewTemplatesFolderHandler(nil, "/x", func(ctx context.Context) ([]vcenterTemplateRow, error) {
		return []vcenterTemplateRow{
			{ID: "1", Name: "A", VCenterTemplate: "vm-a"},
			{ID: "2", Name: "B", VCenterTemplate: ""},
			{ID: "3", Name: "C", VCenterTemplate: "vm-c"},
		}, nil
	})
	m, err := h.buildRegistrationMap(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(m), 2; got != want {
		t.Errorf("len = %d, want %d (empty vcenter_template should be skipped)", got, want)
	}
	if _, ok := m["vm-a"]; !ok {
		t.Error("vm-a missing")
	}
	if _, ok := m["vm-c"]; !ok {
		t.Error("vm-c missing")
	}
}

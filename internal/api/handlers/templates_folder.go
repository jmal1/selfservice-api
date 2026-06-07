package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// VCenterFolderEnumerator is the subset of *vcenter.Client behavior needed by
// the template folder enumeration handler. Defined as an interface so handler
// tests can supply a fake without standing up a real vCenter connection.
type VCenterFolderEnumerator interface {
	ListVMsInFolder(ctx context.Context, folderPath string) ([]vcenter.FolderVM, error)
}

// TemplatesFolderQueries narrows *database.Queries to just the methods the
// template-folder handler needs; satisfied by *database.Queries automatically.
type TemplatesFolderQueries interface {
	ListAllTemplates(ctx context.Context) ([]vcenterTemplateRow, error)
}

// vcenterTemplateRow is intentionally untyped here — the handler reads only
// the fields it needs via a JSON marshal/unmarshal pass against the real
// templates payload so we don't pin the production type into this file.
type vcenterTemplateRow struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	VCenterTemplate string `json:"vcenter_template"`
}

// templatesFolderResponse is the JSON shape returned by GET
// /api/v1/admin/vcenter/templates-folder.
type templatesFolderResponse struct {
	VMs             []folderVMWithRegistration `json:"vms"`
	FolderPath      string                     `json:"folder_path"`
	Cached          bool                       `json:"cached"`
	CacheAgeSeconds int                        `json:"cache_age_seconds"`
}

// folderVMWithRegistration extends a vcenter.FolderVM with cross-reference
// fields populated by joining against the local templates table on
// vcenter_template = vm.name.
type folderVMWithRegistration struct {
	vcenter.FolderVM
	RegisteredTemplateID   *string `json:"registered_template_id"`
	RegisteredTemplateName *string `json:"registered_template_name"`
}

// templateFolderCache caches the raw vCenter folder listing (without the DB
// cross-reference, which is always recomputed) for a configurable TTL.
type templateFolderCache struct {
	mu        sync.RWMutex
	data      []vcenter.FolderVM
	fetchedAt time.Time
	ttl       time.Duration
}

// get returns the cached listing and the age in seconds, or (nil, 0, false)
// if no cache entry is present or the entry has expired.
func (c *templateFolderCache) get() ([]vcenter.FolderVM, int, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.data == nil {
		return nil, 0, false
	}
	age := time.Since(c.fetchedAt)
	if age > c.ttl {
		return nil, 0, false
	}
	return c.data, int(age.Seconds()), true
}

func (c *templateFolderCache) set(vms []vcenter.FolderVM) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = vms
	c.fetchedAt = time.Now()
}

// Invalidate clears the cached folder listing so the next request re-queries
// vCenter. Call after template create/update/delete to keep registration
// status fresh.
func (c *templateFolderCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = nil
}

// TemplatesFolderHandler bundles dependencies for the folder enum endpoint.
type TemplatesFolderHandler struct {
	enumerator     VCenterFolderEnumerator
	folderPath     string
	cache          *templateFolderCache
	listTemplates  func(ctx context.Context) ([]vcenterTemplateRow, error)
}

// NewTemplatesFolderHandler constructs a handler with a 5-minute cache TTL.
// folderPath must be the absolute vCenter inventory path (e.g.
// "/JMAL-Datacenter/vm/Templates"); enumerator may be nil if vCenter is not
// configured, in which case the handler returns 503.
func NewTemplatesFolderHandler(
	enumerator VCenterFolderEnumerator,
	folderPath string,
	listTemplates func(ctx context.Context) ([]vcenterTemplateRow, error),
) *TemplatesFolderHandler {
	return &TemplatesFolderHandler{
		enumerator:    enumerator,
		folderPath:    folderPath,
		cache:         &templateFolderCache{ttl: 5 * time.Minute},
		listTemplates: listTemplates,
	}
}

// ServeHTTP handles GET /api/v1/admin/vcenter/templates-folder. Supports
// ?refresh=true to bypass cache.
func (h *TemplatesFolderHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.enumerator == nil {
		http.Error(w, "vCenter not configured", http.StatusServiceUnavailable)
		return
	}

	forceRefresh := r.URL.Query().Get("refresh") == "true"

	var (
		vms    []vcenter.FolderVM
		age    int
		cached bool
	)
	if !forceRefresh {
		if cachedVMs, cachedAge, ok := h.cache.get(); ok {
			vms, age, cached = cachedVMs, cachedAge, true
		}
	}
	if vms == nil {
		fresh, err := h.enumerator.ListVMsInFolder(r.Context(), h.folderPath)
		if err != nil {
			http.Error(w, "failed to enumerate vCenter folder: "+err.Error(), http.StatusBadGateway)
			return
		}
		h.cache.set(fresh)
		vms = fresh
		cached = false
	}

	// Build DB cross-reference (always live, even with a cache hit on the
	// vCenter side) so newly-registered templates show up immediately.
	regByName, err := h.buildRegistrationMap(r.Context())
	if err != nil {
		http.Error(w, "failed to read templates table: "+err.Error(), http.StatusInternalServerError)
		return
	}

	enriched := make([]folderVMWithRegistration, 0, len(vms))
	for _, vm := range vms {
		row := folderVMWithRegistration{FolderVM: vm}
		if reg, ok := regByName[vm.Name]; ok {
			id, name := reg.ID, reg.Name
			row.RegisteredTemplateID = &id
			row.RegisteredTemplateName = &name
		}
		enriched = append(enriched, row)
	}
	// Deterministic ordering — sort by name; instructors will be scanning visually.
	sort.Slice(enriched, func(i, j int) bool { return enriched[i].Name < enriched[j].Name })

	resp := templatesFolderResponse{
		VMs:             enriched,
		FolderPath:      h.folderPath,
		Cached:          cached,
		CacheAgeSeconds: age,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// buildRegistrationMap reads the templates table and indexes rows by the
// vcenter_template column. Empty vcenter_template values are skipped.
func (h *TemplatesFolderHandler) buildRegistrationMap(ctx context.Context) (map[string]vcenterTemplateRow, error) {
	if h.listTemplates == nil {
		return nil, errors.New("listTemplates not wired")
	}
	rows, err := h.listTemplates(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]vcenterTemplateRow, len(rows))
	for _, row := range rows {
		if row.VCenterTemplate == "" {
			continue
		}
		out[row.VCenterTemplate] = row
	}
	return out, nil
}

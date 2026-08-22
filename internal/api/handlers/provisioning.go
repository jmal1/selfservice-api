package handlers

import (
	"net/http"
	"strings"

	"github.com/jmal1/selfservice-api/internal/provisioning"
)

const (
	ProvisioningAvailableMessage   = provisioning.AvailableMessage
	ProvisioningMaintenanceMessage = provisioning.MaintenanceMessage
	ProvisioningRetryAfterSeconds  = provisioning.RetryAfterSeconds

	provisioningRoutePodCreate       = "pod_create"
	provisioningRouteBlueprintDeploy = "blueprint_deploy"
	provisioningRouteVMAdd           = "vm_add"
)

var provisioningRoutes = []string{
	provisioningRouteBlueprintDeploy,
	provisioningRoutePodCreate,
	provisioningRouteVMAdd,
}

// ProvisioningStatusResponse is the stable UI-facing maintenance contract.
type ProvisioningStatusResponse = provisioning.Status

type provisioningAdmissionMetrics interface {
	RecordRejected(route string)
}

func (h *Handler) provisioningStatus() ProvisioningStatusResponse {
	enabled := !h.provisioningConfigured || h.provisioningEnabled
	return provisioning.StatusFor(enabled)
}

// GetProvisioningStatus returns the current API admission state.
func (h *Handler) GetProvisioningStatus(w http.ResponseWriter, _ *http.Request) {
	respondJSON(w, http.StatusOK, h.provisioningStatus())
}

func (h *Handler) rejectProvisioning(w http.ResponseWriter, r *http.Request, route string) bool {
	if h.provisioningStatus().Enabled {
		return false
	}
	if h.provisioningMetrics != nil {
		h.provisioningMetrics.RecordRejected(route)
	}
	w.Header().Set("Retry-After", ProvisioningRetryAfterSeconds)
	respondError(w, r, http.StatusServiceUnavailable, ProvisioningMaintenanceMessage)
	return true
}

func provisioningRoute(r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		return "", false
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 3 && parts[0] == "api" && parts[1] == "v1" && parts[2] == "pods":
		return provisioningRoutePodCreate, true
	case len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" &&
		parts[2] == "blueprints" && parts[4] == "deploy":
		return provisioningRouteBlueprintDeploy, true
	case len(parts) == 5 && parts[0] == "api" && parts[1] == "v1" &&
		parts[2] == "pods" && parts[4] == "vms":
		return provisioningRouteVMAdd, true
	default:
		return "", false
	}
}

// ProvisioningAdmission rejects provisioning requests before request auditing
// can touch the database. The handler-level guards remain as defense in depth.
func (h *Handler) ProvisioningAdmission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if route, ok := provisioningRoute(r); ok && h.rejectProvisioning(w, r, route) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

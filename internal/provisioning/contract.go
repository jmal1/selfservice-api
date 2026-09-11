// Package provisioning defines the stable API maintenance contract shared by
// the gateway and its read-only synthetic monitor.
package provisioning

const (
	AvailableMessage   = "Provisioning is available."
	MaintenanceMessage = "Provisioning is temporarily unavailable for maintenance."
	RetryAfterSeconds  = "300"
)

// Status is returned by authenticated GET /api/v1/provisioning/status.
type Status struct {
	Enabled bool   `json:"enabled"`
	Message string `json:"message"`
}

// StatusFor returns the cohesive enabled/message pair for a maintenance state.
func StatusFor(enabled bool) Status {
	message := MaintenanceMessage
	if enabled {
		message = AvailableMessage
	}
	return Status{Enabled: enabled, Message: message}
}

package models

import (
	"fmt"
	"time"
)

// Resource-conservation limits (role-scoped pod lifetime, idle, snapshots).
const (
	MaxPodExtensions          = 2
	MaxUserSnapshots          = 1 // non-initial only; is_initial is separate
	DefaultIdleTimeoutSeconds = 7200
	SuspendedDestroyAfter     = 5 * 24 * time.Hour
	TemplateOrphanInactiveAge = 72 * time.Hour
	DestroyFailedRetryAfter   = 30 * time.Minute
	MaxDestroyFailedRetries   = 5
)

// PodTTL returns the default pod lifetime for a role. Unknown roles fail closed
// with an error (callers must not invent a generous default).
func PodTTL(role string) (time.Duration, error) {
	switch role {
	case RoleStudent:
		return 5 * 24 * time.Hour, nil
	case RoleInstructor, RoleAdmin:
		return 14 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown role %q for pod TTL", role)
	}
}

// PodExtend returns how far an extension moves expires_at from now.
func PodExtend(role string) (time.Duration, error) {
	switch role {
	case RoleStudent:
		return 5 * 24 * time.Hour, nil
	case RoleInstructor, RoleAdmin:
		return 7 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unknown role %q for pod extend", role)
	}
}

// IdleSuspendApplies is true only for student-owned pods.
func IdleSuspendApplies(role string) bool {
	return role == RoleStudent
}

// NewExpiresAt returns a non-nil expires_at for known roles.
func NewExpiresAt(role string, now time.Time) (*time.Time, error) {
	ttl, err := PodTTL(role)
	if err != nil {
		return nil, err
	}
	t := now.UTC().Add(ttl)
	return &t, nil
}

// ExtendExpiresAt returns now + PodExtend(role).
func ExtendExpiresAt(role string, now time.Time) (time.Time, time.Duration, error) {
	ext, err := PodExtend(role)
	if err != nil {
		return time.Time{}, 0, err
	}
	return now.UTC().Add(ext), ext, nil
}

// RoleLimits is the policy surface exposed on /auth/me for UI display.
type RoleLimits struct {
	PodTTLDays          int  `json:"pod_ttl_days"`
	ExtendDays          int  `json:"extend_days"`
	MaxExtensions       int  `json:"max_extensions"`
	IdleSuspendHours    *int `json:"idle_suspend_hours"`
	IdleSuspendApplies  bool `json:"idle_suspend_applies"`
	SuspendedDeleteDays int  `json:"suspended_delete_days"`
	MaxUserSnapshots    int  `json:"max_user_snapshots"`
	MaxPods             int  `json:"max_pods"`
	MaxVCPUs            int  `json:"max_vcpus"`
	MaxRAMMB            int  `json:"max_ram_mb"`
	TemplateOrphanDays  int  `json:"template_orphan_days"`
}

// LimitsForRole builds RoleLimits from the central constants and DefaultQuotas.
func LimitsForRole(role string) (RoleLimits, error) {
	ttl, err := PodTTL(role)
	if err != nil {
		return RoleLimits{}, err
	}
	ext, err := PodExtend(role)
	if err != nil {
		return RoleLimits{}, err
	}
	q, ok := DefaultQuotas[role]
	if !ok {
		return RoleLimits{}, fmt.Errorf("unknown role %q for quotas", role)
	}
	lim := RoleLimits{
		PodTTLDays:          int(ttl.Hours() / 24),
		ExtendDays:          int(ext.Hours() / 24),
		MaxExtensions:       MaxPodExtensions,
		IdleSuspendApplies:  IdleSuspendApplies(role),
		SuspendedDeleteDays: int(SuspendedDestroyAfter.Hours() / 24),
		MaxUserSnapshots:    MaxUserSnapshots,
		MaxPods:             q.MaxPods,
		MaxVCPUs:            q.MaxVCPUs,
		MaxRAMMB:            q.MaxRAMMB,
		TemplateOrphanDays:  int(TemplateOrphanInactiveAge.Hours() / 24),
	}
	if lim.IdleSuspendApplies {
		h := DefaultIdleTimeoutSeconds / 3600
		lim.IdleSuspendHours = &h
	}
	return lim, nil
}

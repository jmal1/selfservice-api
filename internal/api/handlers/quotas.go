package handlers

import (
	"errors"
	"fmt"

	"github.com/jmal1/selfservice-api/internal/models"
)

// QuotaError is returned when a request would push the user past one of
// their resource quotas. It's a distinct type so callers can switch on the
// kind for HTTP status mapping (currently always 409 Conflict) and the
// UI can colour-code the failing dimension.
type QuotaError struct {
	// Kind identifies which quota dimension was exceeded. Stable string
	// values — the UI parses these to drive its error messages.
	Kind string // "pods" | "vcpus" | "ram_mb"
	// Limit is the user's quota cap for that dimension.
	Limit int
	// Used is the user's current usage before the new request.
	Used int
	// Requested is the additional resources the request would consume.
	Requested int
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("%s quota exceeded: %d used + %d requested > %d limit",
		e.Kind, e.Used, e.Requested, e.Limit)
}

// IsQuotaError reports whether err is a *QuotaError. Provided so callers
// don't have to import errors.As at every call site.
func IsQuotaError(err error) bool {
	var qe *QuotaError
	return errors.As(err, &qe)
}

// ValidateQuotas returns a *QuotaError if granting requestedPods,
// requestedVCPUs, and requestedRAMMB on top of the user's current usage
// would exceed their per-user caps. Pass requestedPods=0 for code paths
// that add VMs to an existing pod (e.g. AddVM) — that path doesn't grow
// the pod count and shouldn't trip the pod-quota check.
//
// Checks are ordered pods -> vcpus -> ram so the user always sees the
// most user-facing limit first (a pod-count breach is easier to reason
// about than a vCPU breach hidden behind a pod-count breach).
//
// The function is pure: no I/O, no goroutines. Make sure usage and user
// are loaded from the same transaction or close enough in time that the
// snapshot is meaningful — concurrent pod creates can still race past
// this check (we accept that; the alternative is a row-level lock on
// every user record).
func ValidateQuotas(usage *models.ResourceUsage, user *models.User, requestedPods, requestedVCPUs, requestedRAMMB int) error {
	if user == nil {
		// Defensive: callers should never pass nil. Returning an error
		// rather than panicking lets the http handler surface a 500 with
		// a meaningful message instead of a stack trace.
		return errors.New("validateQuotas: nil user")
	}
	if usage == nil {
		return errors.New("validateQuotas: nil usage")
	}
	if usage.ActivePods+requestedPods > user.MaxPods {
		return &QuotaError{
			Kind:      "pods",
			Limit:     user.MaxPods,
			Used:      usage.ActivePods,
			Requested: requestedPods,
		}
	}
	if usage.UsedVCPUs+requestedVCPUs > user.MaxVCPUs {
		return &QuotaError{
			Kind:      "vcpus",
			Limit:     user.MaxVCPUs,
			Used:      usage.UsedVCPUs,
			Requested: requestedVCPUs,
		}
	}
	if usage.UsedRAMMB+requestedRAMMB > user.MaxRAMMB {
		return &QuotaError{
			Kind:      "ram_mb",
			Limit:     user.MaxRAMMB,
			Used:      usage.UsedRAMMB,
			Requested: requestedRAMMB,
		}
	}
	return nil
}

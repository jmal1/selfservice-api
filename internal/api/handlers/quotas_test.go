package handlers

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

func makeUser(maxPods, maxVCPUs, maxRAMMB int) *models.User {
	return &models.User{
		ID:       uuid.New(),
		MaxPods:  maxPods,
		MaxVCPUs: maxVCPUs,
		MaxRAMMB: maxRAMMB,
	}
}

func makeUsage(activePods, usedVCPUs, usedRAMMB int) *models.ResourceUsage {
	return &models.ResourceUsage{
		ActivePods: activePods,
		UsedVCPUs:  usedVCPUs,
		UsedRAMMB:  usedRAMMB,
	}
}

// TestValidateQuotas covers every branch of the quota gate. Pod creation
// (requestedPods=1) and add-VM (requestedPods=0) flows both go through
// this function in production, so the table exercises both.
func TestValidateQuotas(t *testing.T) {
	tests := []struct {
		name            string
		user            *models.User
		usage           *models.ResourceUsage
		requestedPods   int
		requestedVCPUs  int
		requestedRAMMB  int
		wantErr         bool
		wantKind        string // "" if no error expected
		wantNonQuotaErr bool   // for the nil-arg defensive branches
	}{
		// --- Happy paths ---
		{
			name:           "fresh_user_within_all_caps",
			user:           makeUser(2, 4, 8192),
			usage:          makeUsage(0, 0, 0),
			requestedPods:  1,
			requestedVCPUs: 2,
			requestedRAMMB: 4096,
		},
		{
			name:           "exact_match_all_dimensions_ok",
			user:           makeUser(2, 4, 8192),
			usage:          makeUsage(1, 2, 4096),
			requestedPods:  1,
			requestedVCPUs: 2,
			requestedRAMMB: 4096,
		},
		{
			name:           "add_vm_no_pod_growth",
			user:           makeUser(2, 4, 8192),
			usage:          makeUsage(2, 1, 2048), // already at pod cap
			requestedPods:  0,                     // add-vm path
			requestedVCPUs: 1,
			requestedRAMMB: 2048,
		},

		// --- Pod cap ---
		{
			name:           "pod_cap_breach_create",
			user:           makeUser(2, 99, 999999),
			usage:          makeUsage(2, 0, 0),
			requestedPods:  1,
			requestedVCPUs: 1,
			requestedRAMMB: 1024,
			wantErr:        true,
			wantKind:       "pods",
		},
		{
			name:           "pod_cap_not_checked_when_requesting_zero",
			user:           makeUser(1, 99, 999999),
			usage:          makeUsage(1, 0, 0), // exactly at cap already
			requestedPods:  0,                  // add-vm
			requestedVCPUs: 1,
			requestedRAMMB: 1024,
		},

		// --- vCPU cap ---
		{
			name:           "vcpu_cap_breach",
			user:           makeUser(99, 4, 999999),
			usage:          makeUsage(0, 3, 0),
			requestedPods:  1,
			requestedVCPUs: 2,
			requestedRAMMB: 1024,
			wantErr:        true,
			wantKind:       "vcpus",
		},
		{
			name:           "vcpu_cap_exact_match_ok",
			user:           makeUser(99, 4, 999999),
			usage:          makeUsage(0, 3, 0),
			requestedPods:  1,
			requestedVCPUs: 1,
			requestedRAMMB: 1024,
		},

		// --- RAM cap ---
		{
			name:           "ram_cap_breach",
			user:           makeUser(99, 99, 8192),
			usage:          makeUsage(0, 0, 4096),
			requestedPods:  1,
			requestedVCPUs: 1,
			requestedRAMMB: 4097,
			wantErr:        true,
			wantKind:       "ram_mb",
		},

		// --- Ordering: pod breach reported before vcpu/ram breach ---
		{
			name:           "pod_breach_wins_over_vcpu_breach",
			user:           makeUser(1, 1, 1024),
			usage:          makeUsage(1, 1, 1024),
			requestedPods:  1,
			requestedVCPUs: 10, // would also breach vcpu
			requestedRAMMB: 10000,
			wantErr:        true,
			wantKind:       "pods",
		},
		{
			name:           "vcpu_breach_wins_over_ram_breach",
			user:           makeUser(10, 4, 1024),
			usage:          makeUsage(0, 3, 1024),
			requestedPods:  1,
			requestedVCPUs: 2,
			requestedRAMMB: 10000,
			wantErr:        true,
			wantKind:       "vcpus",
		},

		// --- Defensive branches ---
		{
			name:            "nil_user_is_non_quota_error",
			user:            nil,
			usage:           makeUsage(0, 0, 0),
			requestedPods:   1,
			requestedVCPUs:  1,
			requestedRAMMB:  1024,
			wantErr:         true,
			wantNonQuotaErr: true,
		},
		{
			name:            "nil_usage_is_non_quota_error",
			user:            makeUser(1, 1, 1024),
			usage:           nil,
			requestedPods:   1,
			requestedVCPUs:  1,
			requestedRAMMB:  1024,
			wantErr:         true,
			wantNonQuotaErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateQuotas(tc.usage, tc.user, tc.requestedPods, tc.requestedVCPUs, tc.requestedRAMMB)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err: want=%v, got=%v", tc.wantErr, err)
			}
			if !tc.wantErr {
				return
			}
			if tc.wantNonQuotaErr {
				if IsQuotaError(err) {
					t.Errorf("want non-quota error, got *QuotaError")
				}
				return
			}
			var qe *QuotaError
			if !errors.As(err, &qe) {
				t.Fatalf("expected *QuotaError, got %T (%v)", err, err)
			}
			if qe.Kind != tc.wantKind {
				t.Errorf("kind: want %s, got %s", tc.wantKind, qe.Kind)
			}
		})
	}
}

// TestQuotaError_Format guards the error string format — the message is
// surfaced verbatim in the HTTP response and the UI may pattern-match.
func TestQuotaError_Format(t *testing.T) {
	qe := &QuotaError{Kind: "vcpus", Limit: 4, Used: 3, Requested: 2}
	got := qe.Error()
	want := "vcpus quota exceeded: 3 used + 2 requested > 4 limit"
	if got != want {
		t.Errorf("format:\n  want=%q\n  got =%q", want, got)
	}
}

func TestIsQuotaError(t *testing.T) {
	if !IsQuotaError(&QuotaError{Kind: "pods"}) {
		t.Error("IsQuotaError(*QuotaError) = false; want true")
	}
	if IsQuotaError(errors.New("not a quota err")) {
		t.Error("IsQuotaError(non-quota) = true; want false")
	}
	if IsQuotaError(nil) {
		t.Error("IsQuotaError(nil) = true; want false")
	}
}

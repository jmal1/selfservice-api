package database

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func validateMaintenanceClaimSQL(query string) error {
	sql := strings.ToUpper(query)
	for _, fragment := range []string{
		"WHERE STATUS = 'PENDING'",
		"$2",
		"TYPE NOT IN ('POD_CREATE', 'VM_ADD')",
		"TYPE = 'POD_CREATE' AND PAYLOAD->>'CLEANUP_ONLY' = 'TRUE'",
		"FOR UPDATE SKIP LOCKED",
	} {
		if !strings.Contains(sql, fragment) {
			return fmt.Errorf("missing %q", fragment)
		}
	}
	return nil
}

func TestClaimJobMaintenancePolicyFiltersBeforeClaim(t *testing.T) {
	if err := validateMaintenanceClaimSQL(claimJobSQL); err != nil {
		t.Fatal(err)
	}
}

func TestClaimJobMaintenancePolicySabotageIsDetected(t *testing.T) {
	sabotages := map[string]string{
		"ordinary pod create allowed": strings.Replace(
			claimJobSQL,
			"type NOT IN ('pod_create', 'vm_add')",
			"type <> 'vm_add'",
			1,
		),
		"cleanup retry blocked": strings.Replace(
			claimJobSQL,
			"OR (type = 'pod_create' AND payload->>'cleanup_only' = 'true')",
			"",
			1,
		),
		"all vm add allowed": strings.Replace(
			claimJobSQL,
			"type NOT IN ('pod_create', 'vm_add')",
			"type <> 'pod_create'",
			1,
		),
	}

	for name, sabotaged := range sabotages {
		t.Run(name, func(t *testing.T) {
			if err := validateMaintenanceClaimSQL(sabotaged); err == nil {
				t.Fatal("sabotaged claim policy unexpectedly passed validation")
			}
		})
	}
}

func TestRetryJobSQLPersistsCleanupOnlyMarker(t *testing.T) {
	body := strings.ToUpper(retryJobSQL)
	for _, fragment := range []string{
		"WHEN $3 THEN JSONB_SET(PAYLOAD, '{CLEANUP_ONLY}', 'TRUE'::JSONB, TRUE)",
		"ELSE PAYLOAD",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("retryJobSQL missing %q", fragment)
		}
	}
}

func TestPodCreateCleanupSafeStates(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{status: models.PodStatusError, want: true},
		{status: models.PodStatusDestroying, want: true},
		{status: models.PodStatusDestroyFailed, want: true},
		{status: models.PodStatusDestroyed, want: true},
		{status: models.PodStatusPending, want: false},
		{status: models.PodStatusProvisioning, want: false},
		{status: models.PodStatusActive, want: false},
		{status: "unexpected", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			if got := podCreateCleanupStatusSafe(tc.status); got != tc.want {
				t.Fatalf("podCreateCleanupStatusSafe(%q) = %t, want %t", tc.status, got, tc.want)
			}
		})
	}
}

func TestPodCreateCleanupTransitionSQLIsAtomic(t *testing.T) {
	for _, fragment := range []string{
		"jsonb_set(payload, '{cleanup_only}', 'true'::jsonb, true)",
		"WHERE id = $1 AND type = 'pod_create'",
	} {
		if !strings.Contains(markPodCreateCleanupOnlySQL, fragment) {
			t.Errorf("markPodCreateCleanupOnlySQL missing %q", fragment)
		}
	}
}

package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

func validateMaintenanceClaimSQL(query string) error {
	sql := strings.ToUpper(query)
	for _, fragment := range []string{
		"WHERE STATUS = 'PENDING'",
		"$2",
		"TYPE NOT IN ('POD_CREATE', 'VM_ADD')",
		"TYPE IN ('POD_CREATE', 'VM_ADD') AND PAYLOAD->>'CLEANUP_ONLY' = 'TRUE'",
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
			"OR (type IN ('pod_create', 'vm_add') AND payload->>'cleanup_only' = 'true')",
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
		"WITH UPDATED AS",
		"STATUS IN ('CLAIMED', 'IN_PROGRESS')",
		"WHEN $3 AND $4::JSONB IS NOT NULL THEN JSONB_SET(",
		"'{CLEANUP_TARGET}'",
		"WHEN $3 THEN JSONB_SET(PAYLOAD, '{CLEANUP_ONLY}', 'TRUE'::JSONB, TRUE)",
		"ELSE PAYLOAD",
		"STATUS = 'PENDING'",
		"PAYLOAD->>'CLEANUP_ONLY' = 'TRUE'",
		"PAYLOAD->'CLEANUP_TARGET' = $4::JSONB",
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("retryJobSQL missing %q", fragment)
		}
	}
}

func TestCompletedJobStatusAllowsPayloadWithoutCleanupMarker(t *testing.T) {
	const required = "COALESCE(payload->>'cleanup_only', 'false') = 'true'"
	body, err := os.ReadFile("queries.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), required) {
		t.Fatalf("normal job completion is not null-safe; missing %q", required)
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

func validateLeaseRecoverySQL(query string) error {
	sql := strings.ToUpper(query)
	for _, fragment := range []string{
		"STATUS IN ('IN_PROGRESS', 'CLAIMED')",
		"COMPLETED_AT IS NULL",
		"CLAIMED_AT IS NULL OR CLAIMED_AT < NOW() - ($1 * INTERVAL '1 SECOND')",
	} {
		if !strings.Contains(sql, fragment) {
			return fmt.Errorf("missing %q", fragment)
		}
	}
	if strings.Contains(sql, "WHERE STATUS IN ('IN_PROGRESS', 'CLAIMED') AND COMPLETED_AT IS NULL\n") {
		return errors.New("recovery query still contains the broad pre-lease reset")
	}
	return nil
}

func TestRecoverStaleJobsRequiresExpiredLease(t *testing.T) {
	if err := validateLeaseRecoverySQL(recoverStaleJobsSQL); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverStaleJobsLeaseGuardSabotageIsDetected(t *testing.T) {
	sabotaged := strings.Replace(
		recoverStaleJobsSQL,
		"AND (claimed_at IS NULL OR claimed_at < now() - ($1 * interval '1 second'))",
		"",
		1,
	)
	if err := validateLeaseRecoverySQL(sabotaged); err == nil {
		t.Fatal("broad active-job reset unexpectedly passed lease validation")
	}
}

func TestVMCloneDestructionHandoffPreservesConcurrentTargets(t *testing.T) {
	primary := persistedVMCloneTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-100",
	}
	lost := persistedVMCloneTarget{
		PodID:       primary.PodID,
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-200",
	}
	payload, err := json.Marshal(map[string]any{
		"pod_id":            primary.PodID,
		"cleanup_target":    primary,
		"cleanup_completed": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := json.Marshal(lost)
	if err != nil {
		t.Fatal(err)
	}

	staged, alreadyDestroyed, err := prepareVMCloneDestructionPayload(payload, target)
	if err != nil {
		t.Fatal(err)
	}
	if alreadyDestroyed {
		t.Fatal("new exact target was mistaken for completed destruction")
	}
	var got struct {
		CleanupOnly           bool                     `json:"cleanup_only"`
		CleanupTarget         persistedVMCloneTarget   `json:"cleanup_target"`
		CleanupHandoffTargets []persistedVMCloneTarget `json:"cleanup_handoff_targets"`
		CleanupCompleted      bool                     `json:"cleanup_completed"`
	}
	if err := json.Unmarshal(staged, &got); err != nil {
		t.Fatal(err)
	}
	if !got.CleanupOnly || got.CleanupCompleted {
		t.Fatalf("staged handoff flags = cleanup_only:%t cleanup_completed:%t", got.CleanupOnly, got.CleanupCompleted)
	}
	if !samePersistedVMCloneTarget(got.CleanupTarget, primary) {
		t.Fatalf("primary target changed: %+v", got.CleanupTarget)
	}
	if len(got.CleanupHandoffTargets) != 1 ||
		!samePersistedVMCloneTarget(got.CleanupHandoffTargets[0], lost) {
		t.Fatalf("handoff targets = %+v, want exact lost target", got.CleanupHandoffTargets)
	}

	completed, alreadyRecorded, err := completeVMCloneDestructionPayload(staged, target)
	if err != nil {
		t.Fatal(err)
	}
	if alreadyRecorded {
		t.Fatal("new destruction proof was treated as a duplicate")
	}
	var finished struct {
		CleanupOnly           bool                     `json:"cleanup_only"`
		CleanupTarget         persistedVMCloneTarget   `json:"cleanup_target"`
		CleanupHandoffTargets []persistedVMCloneTarget `json:"cleanup_handoff_targets"`
		DestroyedTargets      []persistedVMCloneTarget `json:"destroyed_cleanup_targets"`
		CleanupCompleted      bool                     `json:"cleanup_completed"`
	}
	if err := json.Unmarshal(completed, &finished); err != nil {
		t.Fatal(err)
	}
	if !samePersistedVMCloneTarget(finished.CleanupTarget, primary) ||
		len(finished.CleanupHandoffTargets) != 0 ||
		len(finished.DestroyedTargets) != 1 ||
		!samePersistedVMCloneTarget(finished.DestroyedTargets[0], lost) {
		t.Fatalf("completed handoff payload = %+v", finished)
	}
	if finished.CleanupCompleted {
		t.Fatal("different pending cleanup target was incorrectly marked completed")
	}
}

func TestVMCloneDestructionProofIsIdempotent(t *testing.T) {
	target := persistedVMCloneTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-300",
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"cleanup_only":              true,
		"cleanup_completed":         true,
		"destroyed_cleanup_targets": []persistedVMCloneTarget{target},
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, alreadyRecorded, err := completeVMCloneDestructionPayload(payload, targetJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !alreadyRecorded {
		t.Fatal("existing exact destruction proof was not recognized")
	}
	if string(updated) != string(payload) {
		t.Fatal("idempotent destruction proof changed the payload")
	}
}

func TestCompletingPrimaryCleanupPromotesNextHandoffTarget(t *testing.T) {
	primary := persistedVMCloneTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-400",
	}
	secondary := persistedVMCloneTarget{
		PodID:       primary.PodID,
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-500",
	}
	payload, err := json.Marshal(map[string]any{
		"cleanup_only":            true,
		"cleanup_target":          primary,
		"cleanup_handoff_targets": []persistedVMCloneTarget{secondary},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := json.Marshal(primary)
	if err != nil {
		t.Fatal(err)
	}
	updated, alreadyRecorded, err := completeVMCloneDestructionPayload(payload, target)
	if err != nil {
		t.Fatal(err)
	}
	if alreadyRecorded {
		t.Fatal("new primary cleanup was treated as already completed")
	}
	var got struct {
		CleanupTarget         persistedVMCloneTarget   `json:"cleanup_target"`
		CleanupHandoffTargets []persistedVMCloneTarget `json:"cleanup_handoff_targets"`
		CleanupCompleted      bool                     `json:"cleanup_completed"`
	}
	if err := json.Unmarshal(updated, &got); err != nil {
		t.Fatal(err)
	}
	if !samePersistedVMCloneTarget(got.CleanupTarget, secondary) {
		t.Fatalf("promoted target = %+v, want %+v", got.CleanupTarget, secondary)
	}
	if len(got.CleanupHandoffTargets) != 0 || got.CleanupCompleted {
		t.Fatalf("remaining handoffs = %+v, cleanup_completed = %t", got.CleanupHandoffTargets, got.CleanupCompleted)
	}
}

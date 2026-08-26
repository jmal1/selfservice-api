package database

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

func validateMaintenanceClaimSQL(query string) error {
	sql := strings.ToUpper(query)
	for _, fragment := range []string{
		"WHERE STATUS = 'PENDING'",
		"$2",
		"PAYLOAD->>'CLEANUP_ONLY' = 'TRUE'",
		"FOR UPDATE SKIP LOCKED",
	} {
		if !strings.Contains(sql, fragment) {
			return fmt.Errorf("missing %q", fragment)
		}
	}
	for _, jobType := range []string{
		"POD_CREATE",
		"VM_ADD",
		"TEMPLATE_PROVISION",
		"TEMPLATE_GENERALIZE",
		"TEMPLATE_VERIFY",
		"TEMPLATE_REVALIDATE",
		"TEMPLATE_HEALTH_CONFIRM",
		"TEMPLATE_REPLICA_BUILD",
		"IMAGE_IMPORT",
	} {
		if strings.Count(sql, "'"+jobType+"'") != 2 {
			return fmt.Errorf("withheld job type %s must appear in both claim predicates", jobType)
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
			"'pod_create',",
			"",
			1,
		),
		"cleanup retry blocked": strings.Replace(
			claimJobSQL,
			"'pod_create',",
			"",
			2,
		),
		"template staging allowed": strings.Replace(
			claimJobSQL,
			"'template_provision',",
			"",
			1,
		),
		"image import allowed": strings.Replace(
			claimJobSQL,
			"'image_import'",
			"'safe_job'",
			1,
		),
		"all vm add allowed": strings.Replace(
			claimJobSQL,
			"'vm_add',",
			"",
			1,
		),
		"smoke clone allowed": strings.Replace(
			claimJobSQL,
			"'template_revalidate',",
			"",
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

func validateRetryJobSQL(query string) error {
	body := strings.ToUpper(query)
	for _, fragment := range []string{
		"WITH UPDATED AS",
		"STATUS IN ('CLAIMED', 'IN_PROGRESS')",
		"WHEN $3 AND $4::JSONB IS NOT NULL THEN JSONB_SET(",
		"'{CLEANUP_TARGET}'",
		"WHEN $3 THEN JSONB_SET(PAYLOAD, '{CLEANUP_ONLY}', 'TRUE'::JSONB, TRUE)",
		"ELSE PAYLOAD - 'CLEANUP_ONLY' - 'CLEANUP_COMPLETED'",
		"STATUS = 'PENDING'",
		"PAYLOAD->>'CLEANUP_ONLY' = 'TRUE'",
		"PAYLOAD->'CLEANUP_TARGET' = $4::JSONB",
	} {
		if !strings.Contains(body, fragment) {
			return fmt.Errorf("retryJobSQL missing %q", fragment)
		}
	}
	return nil
}

func TestRetryJobSQLSeparatesForwardAndCleanupRetries(t *testing.T) {
	if err := validateRetryJobSQL(retryJobSQL); err != nil {
		t.Fatal(err)
	}
	sabotages := map[string]string{
		"cleanup-marker": strings.ReplaceAll(
			retryJobSQL,
			"WHEN $3 THEN jsonb_set(payload, '{cleanup_only}', 'true'::jsonb, true)",
			"WHEN $3 THEN payload",
		),
		"forward-marker-clear": strings.ReplaceAll(
			retryJobSQL,
			"ELSE payload - 'cleanup_only' - 'cleanup_completed'",
			"ELSE payload",
		),
	}
	for name, sabotaged := range sabotages {
		t.Run(name, func(t *testing.T) {
			if err := validateRetryJobSQL(sabotaged); err == nil {
				t.Fatal("sabotaged retry SQL unexpectedly passed validation")
			}
		})
	}
}

func TestCompletedJobStatusAllowsPayloadWithoutCleanupMarker(t *testing.T) {
	required := []string{
		"COALESCE(payload->>'cleanup_only', 'false') = 'true'",
		"type <> 'template_replica_build'",
	}
	body, err := os.ReadFile("queries.go")
	if err != nil {
		t.Fatal(err)
	}

	for _, fragment := range required {
		if !strings.Contains(string(body), fragment) {
			t.Fatalf("normal job completion guard is incomplete; missing %q", fragment)
		}
	}
}

func TestAppendRollbackReceiptUsesSemanticJSONEquality(t *testing.T) {
	existing := []byte(`[{"name":"network","data":{"b":2,"a":1}}]`)
	reordered := []byte(`{"data":{"a":1,"b":2},"name":"network"}`)
	updated, duplicate, err := appendRollbackReceipt(existing, reordered)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate {
		t.Fatal("semantically identical JSON receipt was appended twice")
	}
	if string(updated) != string(existing) {
		t.Fatalf("duplicate receipt changed payload: %s", updated)
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

func TestCompletingCloneCleanupRemovesMatchingOperationIdentity(t *testing.T) {
	target := persistedVMCloneTarget{
		PodID:       uuid.NewString(),
		PodVMID:     uuid.NewString(),
		VCenterVMID: "vm-301",
	}
	operation := models.VMCloneOperation{
		OperationID: uuid.NewString(),
		PodID:       target.PodID,
		PodVMID:     target.PodVMID,
		TargetName:  "target",
		SourceRef:   "vm-source",
		TaskRef:     "task-301",
		Phase:       models.VMCloneOperationSubmitted,
		PreparedAt:  time.Now(),
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"cleanup_only":    true,
		"cleanup_target":  target,
		"clone_operation": operation,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, _, err := completeVMCloneDestructionPayload(payload, targetJSON)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(updated, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["clone_operation"]; ok {
		t.Fatal("completed exact cleanup retained a resumable clone operation")
	}
	if string(fields["cleanup_completed"]) != "true" {
		t.Fatalf("cleanup completion = %s, want true", fields["cleanup_completed"])
	}
}

func TestRollbackReceiptHandoffIsAppendOnlyAndIdempotent(t *testing.T) {
	first := json.RawMessage(`{"name":"vlan_create","data":{"uuid":"vlan-1"}}`)
	second := json.RawMessage(`{"name":"portgroup_create","data":{"name":"pg-1"}}`)
	existing, err := json.Marshal([]json.RawMessage{first})
	if err != nil {
		t.Fatal(err)
	}
	updated, duplicate, err := appendRollbackReceipt(existing, second)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("new rollback receipt was treated as a duplicate")
	}
	var steps []json.RawMessage
	if err := json.Unmarshal(updated, &steps); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || string(steps[0]) != string(first) || string(steps[1]) != string(second) {
		t.Fatalf("rollback receipts = %s", updated)
	}
	again, duplicate, err := appendRollbackReceipt(updated, second)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate || string(again) != string(updated) {
		t.Fatalf("idempotent receipt changed: duplicate=%t before=%s after=%s", duplicate, updated, again)
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

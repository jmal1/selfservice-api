package database

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func portGroupReceiptJSON(t *testing.T, name string, vlanID int) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"name":    name,
		"vlan_id": vlanID,
		"hosts": []map[string]any{{
			"host_name":     "esxi1.lab.jmal.io",
			"host_moref":    "host-1002",
			"compute_moref": "domain-c9",
			"vswitch_name":  "vSwitch0",
			"security": map[string]any{
				"allow_promiscuous": nil,
				"mac_changes":       nil,
				"forged_transmits":  nil,
			},
			"preexisting": false,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPodPortGroupReceiptPostgresLifecycleSurvivesRollbackCheckpoint(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	receipt := portGroupReceiptJSON(t, "Pod-VLAN3999", 3999)

	if err := fixture.queries.PersistPodPortGroupReceipt(
		ctx,
		fixture.jobID,
		fixture.workerID,
		fixture.podID,
		receipt,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(
		ctx,
		`UPDATE jobs SET rollback_steps = '[]'::jsonb WHERE id = $1`,
		fixture.jobID,
	); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.queries.GetPodPortGroupReceipt(ctx, fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != PodPortGroupReceiptPlanned {
		t.Fatalf("new receipt state = %q, want planned", record.State)
	}
	if err := fixture.queries.BeginPodPortGroupMutation(
		ctx,
		fixture.jobID,
		fixture.workerID,
		fixture.podID,
		receipt,
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.PersistPodPortGroupKeys(
		ctx,
		fixture.podID,
		receipt,
		map[string]string{"host-1002": "key-vim.host.PortGroup-42"},
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.PersistPodPortGroupKeys(
		ctx,
		fixture.podID,
		receipt,
		map[string]string{"host-1002": "key-vim.host.PortGroup-42"},
	); err != nil {
		t.Fatalf("idempotent key persistence: %v", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pod_portgroup_receipts
		SET portgroup_keys = jsonb_build_object('host-1002', 'key-vim.host.PortGroup-sabotaged')
		WHERE pod_id = $1
	`, fixture.podID); err == nil {
		t.Fatal("database accepted stable portgroup key sabotage")
	}
	if err := fixture.queries.PersistPodPortGroupKeys(
		ctx,
		fixture.podID,
		receipt,
		map[string]string{"host-1002": "key-vim.host.PortGroup-replaced"},
	); !errors.Is(err, ErrPortGroupReceiptConflict) {
		t.Fatalf("changed stable key error = %v, want ErrPortGroupReceiptConflict", err)
	}

	record, err = fixture.queries.GetPodPortGroupReceipt(ctx, fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != PodPortGroupReceiptActive ||
		record.RemovedAt != nil ||
		record.Keys["host-1002"] != "key-vim.host.PortGroup-42" {
		t.Fatalf("active durable receipt = %+v", record)
	}
	if err := fixture.queries.MarkPodPortGroupRemoved(ctx, fixture.podID, receipt); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.MarkPodPortGroupRemoved(ctx, fixture.podID, receipt); err != nil {
		t.Fatalf("idempotent tombstone: %v", err)
	}
	record, err = fixture.queries.GetPodPortGroupReceipt(ctx, fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	if record.RemovedAt == nil {
		t.Fatal("receipt tombstone was not durable")
	}
	if record.State != PodPortGroupReceiptRemoved {
		t.Fatalf("removed receipt state = %q, want removed", record.State)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pod_portgroup_receipts
		SET removed_at = removed_at + interval '1 second'
		WHERE pod_id = $1
	`, fixture.podID); err == nil {
		t.Fatal("database accepted removal tombstone timestamp sabotage")
	}
	if err := fixture.queries.PersistPodPortGroupKeys(
		ctx,
		fixture.podID,
		receipt,
		map[string]string{"host-1002": "key-vim.host.PortGroup-42"},
	); !errors.Is(err, ErrPortGroupReceiptConflict) {
		t.Fatalf("post-removal key update error = %v, want ErrPortGroupReceiptConflict", err)
	}
}

func TestPodPortGroupReceiptPostgresMutationStateGuards(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	receipt := portGroupReceiptJSON(t, "Pod-VLAN3999", 3999)
	if err := fixture.queries.PersistPodPortGroupReceipt(
		ctx,
		fixture.jobID,
		fixture.workerID,
		fixture.podID,
		receipt,
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.PersistPodPortGroupKeys(
		ctx,
		fixture.podID,
		receipt,
		map[string]string{"host-1002": "key-vim.host.PortGroup-42"},
	); !errors.Is(err, ErrPortGroupReceiptConflict) {
		t.Fatalf("key binding before mutation intent error = %v, want ErrPortGroupReceiptConflict", err)
	}
	if _, err := fixture.pool.Exec(ctx, `
		UPDATE pod_portgroup_receipts
		SET state = 'active'
		WHERE pod_id = $1
	`, fixture.podID); err == nil {
		t.Fatal("database accepted planned-to-active state sabotage")
	}
	if err := fixture.queries.BeginPodPortGroupMutation(
		ctx,
		fixture.jobID,
		fixture.workerID,
		fixture.podID,
		receipt,
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.MarkPodPortGroupRemoved(
		ctx,
		fixture.podID,
		receipt,
	); !errors.Is(err, ErrPortGroupReceiptConflict) {
		t.Fatalf("applying removal without stable proof error = %v, want ErrPortGroupReceiptConflict", err)
	}
}

func TestPodPortGroupReceiptPostgresPlannedIntentCanTombstone(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	receipt := portGroupReceiptJSON(t, "Pod-VLAN3999", 3999)
	if err := fixture.queries.PersistPodPortGroupReceipt(
		ctx,
		fixture.jobID,
		fixture.workerID,
		fixture.podID,
		receipt,
	); err != nil {
		t.Fatal(err)
	}
	if err := fixture.queries.MarkPodPortGroupRemoved(ctx, fixture.podID, receipt); err != nil {
		t.Fatal(err)
	}
	record, err := fixture.queries.GetPodPortGroupReceipt(ctx, fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != PodPortGroupReceiptRemoved || record.RemovedAt == nil {
		t.Fatalf("planned receipt tombstone = %+v", record)
	}
}

func TestPodPortGroupReceiptPostgresConcurrentConflictFailsClosed(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	receipts := []json.RawMessage{
		portGroupReceiptJSON(t, "Pod-VLAN3999", 3999),
		portGroupReceiptJSON(t, "Pod-VLAN3998", 3998),
	}
	start := make(chan struct{})
	results := make(chan error, len(receipts))
	var wg sync.WaitGroup
	for _, receipt := range receipts {
		receipt := receipt
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- fixture.queries.PersistPodPortGroupReceipt(
				ctx,
				fixture.jobID,
				fixture.workerID,
				fixture.podID,
				receipt,
			)
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var succeeded, conflicted int
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrPortGroupReceiptConflict):
			conflicted++
		default:
			t.Fatalf("unexpected concurrent persistence error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("concurrent results success=%d conflict=%d, want 1/1", succeeded, conflicted)
	}

	record, err := fixture.queries.GetPodPortGroupReceipt(ctx, fixture.podID)
	if err != nil {
		t.Fatal(err)
	}
	var stored struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(record.Receipt, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Name != "Pod-VLAN3999" && stored.Name != "Pod-VLAN3998" {
		t.Fatalf("unexpected winning receipt %q", stored.Name)
	}
}

func TestPodPortGroupReceiptPostgresRejectsWrongRemovalProof(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	ctx := context.Background()
	receipt := portGroupReceiptJSON(t, "Pod-VLAN3999", 3999)
	if err := fixture.queries.PersistPodPortGroupReceipt(
		ctx,
		fixture.jobID,
		fixture.workerID,
		fixture.podID,
		receipt,
	); err != nil {
		t.Fatal(err)
	}
	wrong := portGroupReceiptJSON(t, "Pod-VLAN3998", 3998)
	if err := fixture.queries.MarkPodPortGroupRemoved(
		ctx,
		fixture.podID,
		wrong,
	); !errors.Is(err, ErrPortGroupReceiptConflict) {
		t.Fatalf("wrong removal proof error = %v, want ErrPortGroupReceiptConflict", err)
	}
}

func TestPodPortGroupReceiptPostgresMissingReceiptFailsClosed(t *testing.T) {
	fixture := newPlacementPostgresFixture(t, 1)
	_, err := fixture.queries.GetPodPortGroupReceipt(context.Background(), uuid.New())
	if !errors.Is(err, ErrPortGroupReceiptNotFound) {
		t.Fatalf("missing receipt error = %v, want ErrPortGroupReceiptNotFound", err)
	}
}

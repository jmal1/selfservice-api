package database

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

// PodVMLink is the minimum information the vCenter orphan reconciler needs to
// classify a single inventory VM against the database. It's a read-only join
// across pod_vms and pods, keyed by vcenter_vm_id (the MoRef), so the
// reconciler can do a single bulk query instead of one round-trip per VM.
//
// PodUpdatedAt is the pod's last status transition, used to give in-flight
// destroys a grace window before the reconciler classifies a terminal pod's
// VM as a safe-to-destroy orphan.
type PodVMLink struct {
	VCenterVMID  string
	PodID        uuid.UUID
	PodName      string
	PodStatus    string
	PodUpdatedAt time.Time
	VMStatus     string
	VMCreatedAt  time.Time
}

// PodVMIPCandidate is the minimal information the pod-VM IP reconciler needs to
// keep a running VM's recorded DHCP address in sync with vCenter. CurrentIP is
// the address presently stored in pod_vms (empty string when NULL/unset).
type PodVMIPCandidate struct {
	PodVMID     uuid.UUID
	VCenterVMID string
	CurrentIP   string
}

// ListActivePodVMsForIPRefresh returns running pod_vms that belong to active
// pods, whose template assigns an IP (assign_ip=true), and that carry a
// vcenter_vm_id. These are exactly the VMs whose address the provisioner is
// responsible for recording, so the IP reconciler can refresh pod_vms.ip_address
// whenever a DHCP lease changes or was missed at provisioning time.
//
// VMs from assign_ip=false templates are excluded: their networking is
// owner-managed, so the provisioner has no address to record and must not.
func (q *Queries) ListActivePodVMsForIPRefresh(ctx context.Context) ([]PodVMIPCandidate, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT pv.id, pv.vcenter_vm_id, COALESCE(pv.ip_address, '')
		FROM pod_vms pv
		JOIN pods p ON pv.pod_id = p.id
		JOIN templates t ON pv.template_id = t.id
		WHERE pv.status = 'running'
		  AND p.status = 'active'
		  AND t.assign_ip = TRUE
		  AND pv.vcenter_vm_id IS NOT NULL AND pv.vcenter_vm_id <> ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PodVMIPCandidate
	for rows.Next() {
		var c PodVMIPCandidate
		if err := rows.Scan(&c.PodVMID, &c.VCenterVMID, &c.CurrentIP); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListPodVMLinksByVCenterID returns every pod_vms row that has a non-empty
// vcenter_vm_id, joined to its parent pod, keyed by vcenter_vm_id (MoRef).
//
// Designed for the orphan reconciler: one query, scoped to rows the reconciler
// can act on. Rows with a NULL or empty vcenter_vm_id are filtered out because
// they cannot be correlated to a vCenter inventory VM anyway.
func (q *Queries) ListPodVMLinksByVCenterID(ctx context.Context) (map[string]PodVMLink, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT pv.vcenter_vm_id, pv.pod_id, p.name, p.status, p.updated_at,
		       pv.status, pv.created_at
		FROM pod_vms pv
		JOIN pods p ON pv.pod_id = p.id
		WHERE pv.vcenter_vm_id IS NOT NULL AND pv.vcenter_vm_id <> ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]PodVMLink)
	for rows.Next() {
		var link PodVMLink
		if err := rows.Scan(&link.VCenterVMID, &link.PodID, &link.PodName,
			&link.PodStatus, &link.PodUpdatedAt, &link.VMStatus, &link.VMCreatedAt); err != nil {
			return nil, err
		}
		// If a single MoRef somehow appears twice (e.g. a duplicated
		// vcenter_vm_id from a bug), the latest pod_vms row wins. The
		// reconciler treats this conservatively: a single active linkage
		// is enough to skip destruction.
		if existing, ok := out[link.VCenterVMID]; ok {
			if existing.PodStatus != "destroyed" && existing.PodStatus != "destroy_failed" {
				continue
			}
		}
		out[link.VCenterVMID] = link
	}
	return out, rows.Err()
}

// ---------- Idle-suspend support (migration 000025) ----------

// IdleSuspendCandidate is the minimal per-VM information the idle evaluator
// needs. It avoids loading every PodVM column on every reconcile tick.
type IdleSuspendCandidate struct {
	PodVMID        uuid.UUID
	PodID          uuid.UUID
	PodStatus      string
	VCenterVMID    string
	DisplayName    string
	LastActivityAt *time.Time // nil means no activity recorded since migration
}

// ListRunningPodVMsForIdleEval returns running pod_vms that belong to active
// pods and have a vCenter reference. These are the candidates the idle
// evaluator inspects on each tick.
//
// last_activity_at is COALESCEd to created_at deliberately. A NULL here means
// "no activity has ever been recorded", which the evaluator reads as infinitely
// idle — so before migration 000028 a VM created seconds ago was immediately
// eligible for suspension. created_at is the earliest moment the VM could
// plausibly have been used, so falling back to it can only ever DELAY a
// suspension, never cause one. 000028 also sets a column DEFAULT; this COALESCE
// is the second layer, so an insert path that bypasses the default still cannot
// produce a VM that looks infinitely idle.
func (q *Queries) ListRunningPodVMsForIdleEval(ctx context.Context) ([]IdleSuspendCandidate, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT pv.id, pv.pod_id, p.status, pv.vcenter_vm_id,
		       pv.display_name, COALESCE(pv.last_activity_at, pv.created_at)
		FROM pod_vms pv
		JOIN pods p ON pv.pod_id = p.id
		JOIN users u ON u.id = p.owner_id
		WHERE pv.status = 'running'
		  AND p.status = 'active'
		  AND u.role = 'student'
		  AND pv.vcenter_vm_id IS NOT NULL AND pv.vcenter_vm_id <> ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []IdleSuspendCandidate
	for rows.Next() {
		var c IdleSuspendCandidate
		if err := rows.Scan(&c.PodVMID, &c.PodID, &c.PodStatus,
			&c.VCenterVMID, &c.DisplayName, &c.LastActivityAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (q *Queries) IdleSuspendCandidateStillEligible(ctx context.Context, podVMID uuid.UUID) (bool, error) {
	var eligible bool
	err := q.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pod_vms pv
			JOIN pods p ON p.id = pv.pod_id
			JOIN users u ON u.id = p.owner_id
			WHERE pv.id = $1
			  AND pv.status = 'running'
			  AND p.status = 'active'
			  AND u.role = 'student'
		)
	`, podVMID).Scan(&eligible)
	return eligible, err
}

// TouchVMConsoleAt records a console-session activity timestamp. Called by the
// WebMKS proxy on WS open, every heartbeat interval, and on WS close. Both
// last_console_at and last_activity_at are set to t so the idle evaluator sees
// the VM as recently active.
func (q *Queries) TouchVMConsoleAt(ctx context.Context, id uuid.UUID, t time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE pod_vms
		SET last_console_at  = $2,
		    last_activity_at = $2
		WHERE id = $1
	`, id, t)
	return err
}

// TouchVMActivityAt updates only last_activity_at. Called by the idle evaluator
// when it observes above-threshold CPU or network utilisation for a VM, keeping
// the clock fresh without touching last_console_at.
func (q *Queries) TouchVMActivityAt(ctx context.Context, id uuid.UUID, t time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE pod_vms SET last_activity_at = $2 WHERE id = $1
	`, id, t)
	return err
}

// SetVMSuspended marks a VM as suspended, recording the timestamp and reason.
func (q *Queries) SetVMSuspended(ctx context.Context, id uuid.UUID, t time.Time, reason string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE pod_vms
		SET status         = 'suspended',
		    suspended_at   = $2,
		    suspend_reason = $3
		WHERE id = $1
	`, id, t, reason)
	return err
}

// ClearVMSuspendedAt clears suspend metadata when a VM is powered on again.
func (q *Queries) ClearVMSuspendedAt(ctx context.Context, id uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE pod_vms
		SET suspended_at = NULL, suspend_reason = NULL
		WHERE id = $1
	`, id)
	return err
}

// HasActiveJobForVM reports whether any non-terminal job is currently
// targeting this VM (keyed by pod_vm_id in the JSON payload). Used as a
// suspension guard: suspending a VM mid-job would leave the job in a
// permanently broken state.
func (q *Queries) HasActiveJobForVM(ctx context.Context, podVMID uuid.UUID) (bool, error) {
	var exists bool
	err := q.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM jobs
			WHERE payload->>'pod_vm_id' = $1
			  AND status NOT IN ('completed', 'failed')
		)
	`, podVMID.String()).Scan(&exists)
	return exists, err
}

// HasInFlightRunForVM reports whether an assessment run is currently executing
// against this VM as its target. Used as a suspension guard: suspending a VM
// during a graded assessment run would corrupt the result.
func (q *Queries) HasInFlightRunForVM(ctx context.Context, podVMID uuid.UUID) (bool, error) {
	var exists bool
	err := q.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM runs
			WHERE target_pod_vm_id = $1
			  AND status NOT IN ('completed', 'failed', 'cancelled', 'timeout')
		)
	`, podVMID).Scan(&exists)
	return exists, err
}

// GetIdleTimeoutSeconds returns the effective idle timeout in seconds for the
// given pod. It returns the per-pod override if one exists in suspend_settings,
// the global default otherwise, and 21600 (6 hours) if no rows exist at all.
func (q *Queries) GetIdleTimeoutSeconds(ctx context.Context, podID uuid.UUID) (int, error) {
	var secs int
	err := q.pool.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT idle_timeout_seconds FROM suspend_settings
			 WHERE scope = 'pod' AND scope_id = $1),
			(SELECT idle_timeout_seconds FROM suspend_settings
			 WHERE scope = 'global'),
			$2
		)
	`, podID, models.DefaultIdleTimeoutSeconds).Scan(&secs)
	if err != nil {
		return models.DefaultIdleTimeoutSeconds, err
	}
	return secs, nil
}

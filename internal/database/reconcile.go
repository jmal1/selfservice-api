package database

import (
	"context"
	"time"

	"github.com/google/uuid"
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

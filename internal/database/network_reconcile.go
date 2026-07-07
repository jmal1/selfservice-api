package database

import (
	"context"

	"github.com/google/uuid"
)

// AllocatedVLAN is one vlan_pool allocation joined with its owning pod.
// Used by the network reconciler as the source of truth for desired state.
type AllocatedVLAN struct {
	VLANTag   int
	Subnet    string
	PodID     uuid.UUID
	PodName   string
	PodStatus string
}

// ListAllocatedVLANs returns every non-null vlan_pool allocation joined to
// pods so reconciler logic can classify terminal vs non-terminal pod state.
func (q *Queries) ListAllocatedVLANs(ctx context.Context) ([]AllocatedVLAN, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT vp.vlan_tag, vp.subnet, vp.pod_id, p.name, p.status
		FROM vlan_pool vp
		JOIN pods p ON p.id = vp.pod_id
		WHERE vp.pod_id IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AllocatedVLAN
	for rows.Next() {
		var row AllocatedVLAN
		if err := rows.Scan(&row.VLANTag, &row.Subnet, &row.PodID, &row.PodName, &row.PodStatus); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

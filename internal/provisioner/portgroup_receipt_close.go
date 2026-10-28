package provisioner

import (
	"context"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

// closeAbsentApplyingReceipt closes an applying receipt that never bound a
// stable key once every owned portgroup is verified absent. Create
// compensation removes the groups it added before keys are bound; without this
// the keyed cleanup path can never succeed and the pod keeps its VLAN forever
// (jmal1/Crucible#78). Any present owned group leaves the receipt to the keyed
// path, which refuses to delete an unkeyed group.
func (p *Provisioner) closeAbsentApplyingReceipt(
	ctx context.Context,
	podID uuid.UUID,
	record *database.PodPortGroupReceipt,
	receipt vcenter.PortGroupReceipt,
) (bool, error) {
	if record.RemovedAt != nil ||
		record.State != database.PodPortGroupReceiptApplying ||
		len(record.Keys) > 0 {
		return false, nil
	}
	closed := false
	err := p.db.WithVCenterPortGroupMutationLock(ctx, func(lockCtx context.Context) error {
		absent, err := p.vc.OwnedPortGroupsAbsent(lockCtx, receipt)
		if err != nil || !absent {
			return err
		}
		if err := p.db.MarkPodPortGroupAbsentRemoved(lockCtx, podID, record.Receipt); err != nil {
			return err
		}
		closed = true
		return nil
	})
	return closed, err
}

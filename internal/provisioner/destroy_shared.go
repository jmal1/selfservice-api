package provisioner

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// finishSharedPodDestroy marks a Single VM destroyed without releasing the
// stripe. The VLAN, DHCP subnet, firewall rule, and port group stay in place.
func (p *Provisioner) finishSharedPodDestroy(ctx context.Context, podID, jobID uuid.UUID, claimOwner string, destroyErrors []error) error {
	if len(destroyErrors) > 0 {
		return p.failPodDestroy(ctx, podID, destroyErrors)
	}
	if err := p.db.FinalizePodDestroy(ctx, podID, jobID, claimOwner); err != nil {
		return fmt.Errorf("finalize shared pod %s destruction: %w", podID, err)
	}
	p.publishProgress(jobID, "destroyed", "Single VM destroyed")
	return nil
}

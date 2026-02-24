package provisioner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jmal1/selfservice-api/internal/models"
)

// DestroyPodPayload is the expected shape of job.Payload for pod_destroy.
type DestroyPodPayload struct {
	PodID string `json:"pod_id"`
}

// DestroyPod executes the pod destruction workflow.
// Destruction is best-effort — we continue even if individual steps fail
// because we want to clean up as much as possible.
func (p *Provisioner) DestroyPod(ctx context.Context, job *models.Job) error {
	var payload DestroyPodPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("parse pod_destroy payload: %w", err)
	}

	podID, err := parseUUID(payload.PodID)
	if err != nil {
		return fmt.Errorf("invalid pod_id: %w", err)
	}

	pod, err := p.db.GetPodByID(ctx, podID)
	if err != nil {
		return fmt.Errorf("get pod: %w", err)
	}

	vlanTag := pod.VLANID
	podIndex := pod.PodIndex
	pgName := fmt.Sprintf("Pod-%03d-VLAN%d", podIndex, vlanTag)
	subnet := fmt.Sprintf("10.100.%d.0/24", podIndex)
	var errors []error

	// Mark pod as destroying
	_ = p.db.UpdatePodStatus(ctx, pod.ID, "destroying", "")
	p.publishProgress(job.ID, "destroying", "Starting pod destruction")

	// --- Step 1: Power off all VMs ---
	p.publishProgress(job.ID, "vm_poweroff", "Powering off all VMs")
	vms, err := p.db.ListPodVMs(ctx, pod.ID)
	if err != nil {
		errors = append(errors, fmt.Errorf("list pod VMs: %w", err))
	} else {
		for _, vm := range vms {
			if vm.VCenterVMID != nil && *vm.VCenterVMID != "" {
				if err := p.vc.PowerOffVM(ctx, *vm.VCenterVMID); err != nil {
					p.logger.Warn("failed to power off VM", "moref", *vm.VCenterVMID, "error", err)
					// Continue — VM may already be off
				}
			}
		}
	}

	// --- Step 2: Destroy all VMs ---
	p.publishProgress(job.ID, "vm_destroy", "Destroying all VMs")
	if vms != nil {
		for _, vm := range vms {
			if vm.VCenterVMID != nil && *vm.VCenterVMID != "" {
				if err := p.vc.DestroyVM(ctx, *vm.VCenterVMID); err != nil {
					p.logger.Warn("failed to destroy VM", "moref", *vm.VCenterVMID, "error", err)
					errors = append(errors, fmt.Errorf("destroy VM %s: %w", *vm.VCenterVMID, err))
				} else {
					_ = p.db.UpdatePodVMStatus(ctx, vm.ID, "deleted")
				}
			}
		}
	}

	// --- Step 3: Delete port groups from all ESXi hosts ---
	p.publishProgress(job.ID, "portgroup_delete", fmt.Sprintf("Deleting port group %s", pgName))
	if err := p.vc.DeletePortGroupOnAllHosts(ctx, pgName); err != nil {
		p.logger.Warn("failed to delete port groups", "name", pgName, "error", err)
		errors = append(errors, fmt.Errorf("delete port groups: %w", err))
	}

	// --- Step 4: Remove DHCP subnet ---
	p.publishProgress(job.ID, "dhcp_delete", fmt.Sprintf("Removing DHCP subnet %s", subnet))
	existingDHCP, _ := p.opn.GetDHCPSubnetByNetwork(ctx, subnet)
	if existingDHCP != nil {
		if err := p.opn.DeleteDHCPSubnet(ctx, existingDHCP.UUID); err != nil {
			p.logger.Warn("failed to delete DHCP subnet", "subnet", subnet, "error", err)
			errors = append(errors, fmt.Errorf("delete DHCP: %w", err))
		} else {
			_ = p.opn.ReconfigureDHCP(ctx)
		}
	}

	// --- Step 5: Remove OPNsense interface (SSH) ---
	p.publishProgress(job.ID, "interface_delete", "Removing OPNsense interface")
	// We need to find which optN interface corresponds to this VLAN
	// For now, we look for the interface by its VLAN device name
	// This is stored in the job's rollback_steps if the pod was created by us
	if job.RollbackSteps != nil {
		var steps []struct {
			Name string          `json:"name"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(job.RollbackSteps, &steps); err == nil {
			for _, step := range steps {
				if step.Name == "interface_assign" {
					var d struct{ IfName string `json:"if_name"` }
					if json.Unmarshal(step.Data, &d) == nil && d.IfName != "" {
						if err := p.opnSSH.UnassignInterface(ctx, d.IfName); err != nil {
							p.logger.Warn("failed to unassign interface", "interface", d.IfName, "error", err)
							errors = append(errors, fmt.Errorf("unassign interface: %w", err))
						}
					}
				}
			}
		}
	}

	// --- Step 6: Delete VLAN ---
	p.publishProgress(job.ID, "vlan_delete", fmt.Sprintf("Deleting VLAN %d", vlanTag))
	existingVLAN, _ := p.opn.GetVLANByTag(ctx, vlanTag)
	if existingVLAN != nil {
		if err := p.opn.DeleteVLAN(ctx, existingVLAN.UUID); err != nil {
			p.logger.Warn("failed to delete VLAN", "tag", vlanTag, "error", err)
			errors = append(errors, fmt.Errorf("delete VLAN: %w", err))
		} else {
			_ = p.opn.ReconfigureVLANs(ctx)
		}
	}

	// --- Step 7: Mark pod as destroyed ---
	if len(errors) > 0 {
		errMsg := fmt.Sprintf("%d cleanup errors occurred", len(errors))
		_ = p.db.UpdatePodStatus(ctx, pod.ID, "destroyed", errMsg)
		p.logger.Warn("pod destroyed with errors", "pod_id", pod.ID, "error_count", len(errors))
		return fmt.Errorf("pod destroyed with %d errors: %v", len(errors), errors)
	}

	_ = p.db.UpdatePodStatus(ctx, pod.ID, "destroyed", "")
	p.publishProgress(job.ID, "destroyed", "Pod destroyed successfully")
	p.logger.Info("pod destroyed successfully", "pod_id", pod.ID, "vlan", vlanTag)
	return nil
}

package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/database"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/rollback"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

type vmPlacementSpec struct {
	PodVMID   uuid.UUID
	SourceRef string
}

type placementMetricsSink interface {
	RecordVMPlacement(host, compute, source string)
	SetVMPlacementHeadroom(host string, megabytes int64)
	RecordVMPlacementDrift(kind string)
	RecordVMPlacementRejection(reason string)
}

func (p *Provisioner) prepareVMPlacementPlan(
	ctx context.Context,
	job *models.Job,
	specs []vmPlacementSpec,
	network string,
	allowMissingNetwork bool,
	targetHostMoRefs []string,
) ([]models.VMPlacement, error) {
	workerID, _, err := claimedJobLease(job)
	if err != nil {
		return nil, err
	}
	podVMIDs := make([]uuid.UUID, 0, len(specs))
	for _, spec := range specs {
		podVMIDs = append(podVMIDs, spec.PodVMID)
	}
	existing, err := p.db.LoadVMPlacementPlan(ctx, job.ID, workerID, podVMIDs)
	if err != nil {
		return nil, fmt.Errorf("load durable VM placement plan: %w", err)
	}
	if len(existing) > 0 {
		return existing, nil
	}

	plannedMemory := make(map[string]int64)
	candidates := make([]models.VMPlacement, 0, len(specs))
	for _, spec := range specs {
		podVM, err := p.db.GetPodVM(ctx, spec.PodVMID)
		if err != nil {
			return nil, fmt.Errorf("load pod VM %s for placement: %w", spec.PodVMID, err)
		}
		if podVM.RAMMB <= 0 {
			return nil, fmt.Errorf("pod VM %s has invalid configured RAM %d MB", spec.PodVMID, podVM.RAMMB)
		}
		template, err := p.db.GetTemplateByID(ctx, podVM.TemplateID)
		if err != nil {
			return nil, fmt.Errorf("load template %s for placement: %w", podVM.TemplateID, err)
		}
		replicas, err := p.db.ListTemplateSourceReplicas(ctx, template.ID)
		if err != nil {
			return nil, err
		}

		sourceRef := spec.SourceRef
		if sourceRef == "" {
			sourceRef = template.VCenterRef()
		}
		var sourceCandidates []vcenter.CloneSource
		replicaModeEnabled := len(replicas) > 0
		if len(replicas) == 0 {
			replicaModeEnabled, err = p.db.TemplateSourceReplicaModeEnabled(ctx, template.ID)
			if err != nil {
				return nil, err
			}
			if replicaModeEnabled {
				return nil, fmt.Errorf(
					"template %s has source-replica mode enabled but no replicas are configured",
					template.ID,
				)
			}
			sourceCandidates = []vcenter.CloneSource{{Ref: sourceRef}}
		} else {
			for _, replica := range replicas {
				if replica.Status != models.TemplateSourceReplicaReady {
					continue
				}
				sourceCandidates = append(sourceCandidates, vcenter.CloneSource{
					ReplicaID:            replica.ID.String(),
					Ref:                  replica.SourceVMMoref,
					ComputeResourceType:  replica.ComputeResourceType,
					ComputeResourceMoRef: replica.ComputeResourceMoref,
				})
			}
			if len(sourceCandidates) == 0 {
				return nil, fmt.Errorf(
					"template %s has source replicas configured but none are ready",
					template.ID,
				)
			}
		}

		resolveParams := vcenter.CloneVMParams{
			LogicalTemplateID:     template.ID.String(),
			TemplateName:          sourceRef,
			VCPUs:                 int32(podVM.VCPUs),
			RAMmb:                 int64(podVM.RAMMB),
			Network:               network,
			AllowMissingNetwork:   allowMissingNetwork,
			PlannedMemoryMBByHost: plannedMemory,
			TargetHostMoRefs:      targetHostMoRefs,
			SourceCandidates:      sourceCandidates,
		}
		existingVMMoref := ""
		if podVM.VCenterVMID != nil {
			existingVMMoref = *podVM.VCenterVMID
		}
		capacityObservedAt, err := p.db.BeginHostCapacityObservation(ctx)
		if err != nil {
			return nil, err
		}
		var resolved vcenter.CloneVMParams
		if existingVMMoref != "" {
			if replicaModeEnabled {
				return nil, fmt.Errorf(
					"pod VM %s predates durable placement but template %s has source-replica mode enabled; source identity cannot be reconstructed safely",
					spec.PodVMID,
					template.ID,
				)
			}
			resolved, err = p.vc.ResolveExistingClonePlacement(ctx, existingVMMoref, resolveParams)
		} else {
			resolved, err = p.vc.ResolveClonePlacement(ctx, resolveParams)
		}
		if err != nil {
			if errors.Is(err, vcenter.ErrReservedHeadroom) {
				if metrics, ok := p.pipeline.(placementMetricsSink); ok {
					metrics.RecordVMPlacementRejection("reserved_headroom")
				}
			}
			return nil, fmt.Errorf("resolve placement for pod VM %s: %w", spec.PodVMID, err)
		}
		var sourceReplicaID *uuid.UUID
		if resolved.SourceReplicaID != "" {
			parsed, err := uuid.Parse(resolved.SourceReplicaID)
			if err != nil {
				return nil, fmt.Errorf("resolved source replica has invalid ID %q: %w", resolved.SourceReplicaID, err)
			}
			sourceReplicaID = &parsed
		}
		capacityReservationMB := int64(podVM.RAMMB)
		if existingVMMoref != "" {
			capacityReservationMB = 0
		}
		candidates = append(candidates, models.VMPlacement{
			PodVMID:               spec.PodVMID,
			JobID:                 job.ID,
			TemplateID:            template.ID,
			SourceReplicaID:       sourceReplicaID,
			SourceRef:             resolved.TemplateName,
			ComputeResourceType:   resolved.ComputeResourceType,
			ComputeResourceMoref:  resolved.ComputeResourceMoRef,
			ResourcePoolMoref:     resolved.ResourcePoolMoRef,
			HostMoref:             resolved.HostMoRef,
			HostName:              resolved.HostName,
			DRSControl:            resolved.DRSControl,
			ObservedFreeMemoryMB:  resolved.ObservedFreeMemoryMB,
			ReservedMemoryMB:      resolved.ReservedMemoryMB,
			CapacityReservationMB: capacityReservationMB,
			CapacityObservedAt:    capacityObservedAt,
			LegacyAdoptionPending: existingVMMoref != "",
		})
		if existingVMMoref == "" {
			plannedMemory[resolved.HostMoRef] += int64(podVM.RAMMB)
		}
	}

	persisted, err := p.db.PrepareVMPlacementPlan(ctx, job.ID, workerID, candidates)
	if err != nil {
		if errors.Is(err, database.ErrHostCapacityAdmission) {
			if metrics, ok := p.pipeline.(placementMetricsSink); ok {
				metrics.RecordVMPlacementRejection("reserved_headroom")
			}
		}
		return nil, fmt.Errorf("persist durable VM placement plan: %w", err)
	}
	if metrics, ok := p.pipeline.(placementMetricsSink); ok {
		headroomByHost := make(map[string]int64)
		for _, placement := range persisted {
			source := "legacy"
			if placement.SourceReplicaID != nil {
				source = placement.SourceReplicaID.String()
			}
			metrics.RecordVMPlacement(
				placement.HostName,
				placement.ComputeResourceMoref,
				source,
			)
			headroom, exists := headroomByHost[placement.HostMoref]
			if !exists || placement.AdmittedHeadroomMB < headroom {
				headroomByHost[placement.HostMoref] = placement.AdmittedHeadroomMB
			}
		}
		for host, headroom := range headroomByHost {
			metrics.SetVMPlacementHeadroom(host, headroom)
		}
	}
	return persisted, nil
}

func (p *Provisioner) enforceExistingVMPlacements(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
	placements []models.VMPlacement,
) error {
	for _, placement := range placements {
		podVM, err := p.db.GetPodVM(ctx, placement.PodVMID)
		if err != nil {
			return fmt.Errorf("load pod VM %s for placement enforcement: %w", placement.PodVMID, err)
		}
		if podVM.VCenterVMID == nil || *podVM.VCenterVMID == "" {
			continue
		}
		if placement.LegacyAdoptionPending {
			if err := p.enforceLegacyVMPlacement(
				ctx,
				jobID,
				workerID,
				*podVM.VCenterVMID,
				placement,
			); err != nil {
				return err
			}
			continue
		}
		if err := p.verifyPersistedVMPlacement(ctx, *podVM.VCenterVMID, placement); err != nil {
			return classifyPlacementValidationFailure(fmt.Errorf(
				"enforce placement for existing pod VM %s: %w",
				placement.PodVMID,
				err,
			))
		}
	}
	return nil
}

func (p *Provisioner) enforceLegacyVMPlacement(
	ctx context.Context,
	jobID uuid.UUID,
	workerID string,
	vmMoref string,
	placement models.VMPlacement,
) error {
	if err := p.vc.EnsureVMPlacementControl(
		ctx,
		vmMoref,
		placement.HostMoref,
		placement.ComputeResourceType,
		placement.ComputeResourceMoref,
		placement.DRSControl,
	); err != nil {
		p.recordVMPlacementDrift(err)
		return classifyPlacementValidationFailure(fmt.Errorf(
			"enforce placement for adopted pod VM %s: %w",
			placement.PodVMID,
			err,
		))
	}
	if err := p.db.CompleteLegacyVMPlacementAdoption(
		ctx,
		jobID,
		workerID,
		placement.PodVMID,
	); err != nil {
		return fmt.Errorf("persist completed placement adoption for pod VM %s: %w", placement.PodVMID, err)
	}
	return nil
}

func placementPlanByPodVM(placements []models.VMPlacement) map[uuid.UUID]models.VMPlacement {
	byVM := make(map[uuid.UUID]models.VMPlacement, len(placements))
	for _, placement := range placements {
		byVM[placement.PodVMID] = placement
	}
	return byVM
}

func selectedPlacementHosts(placements []models.VMPlacement) []string {
	unique := make(map[string]struct{}, len(placements))
	for _, placement := range placements {
		unique[placement.HostMoref] = struct{}{}
	}
	hosts := make([]string, 0, len(unique))
	for host := range unique {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

func validatePortGroupReceiptScope(
	receipt vcenter.PortGroupReceipt,
	pgName string,
	vlanID int,
	selectedHosts []string,
) (*vcenter.PortGroupReceipt, error) {
	engine := rollback.New(uuid.Nil, nil, slog.Default())
	raw, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	engine.LoadSteps([]rollback.Step{{Name: "portgroup_create", Data: raw}})
	return existingPortGroupReceipt(engine, pgName, vlanID, selectedHosts)
}

func cloneParamsFromPlacement(params vcenter.CloneVMParams, placement models.VMPlacement) vcenter.CloneVMParams {
	params.LogicalTemplateID = placement.TemplateID.String()
	params.TemplateName = placement.SourceRef
	if placement.SourceReplicaID != nil {
		params.SourceReplicaID = placement.SourceReplicaID.String()
	}
	params.ComputeResourceType = placement.ComputeResourceType
	params.ComputeResourceMoRef = placement.ComputeResourceMoref
	params.ResourcePoolMoRef = placement.ResourcePoolMoref
	params.HostMoRef = placement.HostMoref
	params.HostName = placement.HostName
	params.DRSControl = placement.DRSControl
	params.ObservedFreeMemoryMB = placement.ObservedFreeMemoryMB
	params.ReservedMemoryMB = placement.ReservedMemoryMB
	return params
}

func (p *Provisioner) validatePersistedVMPlacement(
	ctx context.Context,
	podVMID uuid.UUID,
	vmMoref string,
) error {
	placement, err := p.db.GetVMPlacement(ctx, podVMID)
	if err != nil {
		return fmt.Errorf("load durable VM placement: %w", err)
	}
	if placement == nil {
		err = p.vc.ValidateVMPlacement(ctx, vmMoref, "")
	} else {
		err = p.vc.ValidateVMPlacementControl(
			ctx,
			vmMoref,
			placement.HostMoref,
			placement.ComputeResourceType,
			placement.ComputeResourceMoref,
			placement.DRSControl,
		)
	}
	if err != nil {
		p.recordVMPlacementDrift(err)
	}
	return classifyPlacementValidationFailure(err)
}

func classifyPlacementValidationFailure(err error) error {
	if err == nil {
		return nil
	}
	if isManualCleanupRequired(err) ||
		isCompensationRetry(err) ||
		isCompensatedJobError(err) {
		return err
	}
	if errors.Is(err, vcenter.ErrPlacementDrift) ||
		errors.Is(err, vcenter.ErrDRSControlDrift) {
		return &manualCleanupRequiredError{err: err}
	}
	return err
}

func classifyCloneOperationFailure(err error) error {
	if err == nil || isManualCleanupRequired(err) || isCompensationRetry(err) {
		return err
	}
	if errors.Is(err, vcenter.ErrAmbiguousVMOwnership) {
		return &manualCleanupRequiredError{err: err}
	}
	return classifyPlacementValidationFailure(err)
}

func (p *Provisioner) verifyPersistedVMPlacement(
	ctx context.Context,
	vmMoref string,
	placement models.VMPlacement,
) error {
	err := p.vc.ValidateVMPlacementControl(
		ctx,
		vmMoref,
		placement.HostMoref,
		placement.ComputeResourceType,
		placement.ComputeResourceMoref,
		placement.DRSControl,
	)
	if err == nil {
		return nil
	}
	p.recordVMPlacementDrift(err)
	return classifyPlacementValidationFailure(err)
}

func (p *Provisioner) recordVMPlacementDrift(err error) {
	if metrics, ok := p.pipeline.(placementMetricsSink); ok {
		var kind string
		var drift *vcenter.PlacementDriftError
		switch {
		case errors.As(err, &drift):
			kind = string(drift.Kind)
		case errors.Is(err, vcenter.ErrDRSControlDrift):
			kind = "drs"
		case errors.Is(err, vcenter.ErrPlacementDrift):
			kind = "host"
		default:
			return
		}
		metrics.RecordVMPlacementDrift(kind)
	}
}

func existingPortGroupReceipt(
	rb *rollback.Engine,
	pgName string,
	vlanID int,
	selectedHosts []string,
) (*vcenter.PortGroupReceipt, error) {
	selected := make(map[string]struct{}, len(selectedHosts))
	for _, host := range selectedHosts {
		selected[host] = struct{}{}
	}
	var found *vcenter.PortGroupReceipt
	for _, step := range rb.Steps() {
		if step.Name != "portgroup_create" {
			continue
		}
		var receipt vcenter.PortGroupReceipt
		if err := json.Unmarshal(step.Data, &receipt); err != nil {
			return nil, fmt.Errorf("decode persisted port group receipt: %w", err)
		}
		if found != nil {
			continue
		}
		found = &receipt
	}
	if found == nil {
		return nil, nil
	}
	if found.Name != pgName || found.VLANID != vlanID {
		return nil, fmt.Errorf(
			"persisted port group receipt is for %s VLAN %d, expected %s VLAN %d",
			found.Name,
			found.VLANID,
			pgName,
			vlanID,
		)
	}
	for _, host := range found.Hosts {
		delete(selected, host.HostMoRef)
	}
	if len(selected) > 0 {
		missing := make([]string, 0, len(selected))
		for host := range selected {
			missing = append(missing, host)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf(
			"persisted port group receipt does not cover selected hosts: %v",
			missing,
		)
	}
	return found, nil
}

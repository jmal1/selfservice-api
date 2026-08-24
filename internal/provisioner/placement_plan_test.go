package provisioner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
	"github.com/jmal1/selfservice-api/internal/rollback"
	"github.com/jmal1/selfservice-api/internal/vcenter"
)

func TestClassifyPlacementValidationFailurePreservesRetriesForOperationalErrors(t *testing.T) {
	transient := fmt.Errorf("read VM placement: %w", context.DeadlineExceeded)
	classified := classifyPlacementValidationFailure(transient)
	if isManualCleanupRequired(classified) {
		t.Fatalf("transient placement read became manual cleanup: %v", classified)
	}
	if !errors.Is(classified, context.DeadlineExceeded) {
		t.Fatalf("transient cause was lost: %v", classified)
	}
	retryable, reason := ClassifyError(classified, models.JobTypeVMSuspend)
	if !retryable || reason != RetryReasonConnection {
		t.Fatalf("transient classification = (%v, %q), want retryable connection", retryable, reason)
	}

	drift := classifyPlacementValidationFailure(fmt.Errorf(
		"persisted host mismatch: %w",
		vcenter.ErrPlacementDrift,
	))
	if !isManualCleanupRequired(drift) {
		t.Fatalf("proven placement drift remained retryable: %v", drift)
	}
	if retryable, _ := ClassifyError(drift, models.JobTypeVMSuspend); retryable {
		t.Fatalf("proven placement drift was classified as retryable: %v", drift)
	}
}

func TestPlacementPlanningPreservesOperationalRetriesAndTerminalizesDrift(t *testing.T) {
	t.Parallel()

	job := &models.Job{
		Type:       models.JobTypePodCreate,
		RetryCount: 0,
		MaxRetries: 3,
	}
	transient := fmt.Errorf(
		"read persisted placement: %w: %w",
		vcenter.ErrPlacementValidationUnavailable,
		context.DeadlineExceeded,
	)
	if !jobRetryAvailable(job, transient) {
		t.Fatalf("transient persisted-placement validation lost its retry: %v", transient)
	}
	capacityRelease := fmt.Errorf("%w: deadlock detected", errVMPlacementCapacityRelease)
	if !jobRetryAvailable(job, capacityRelease) {
		t.Fatalf("capacity-release persistence failure lost its retry: %v", capacityRelease)
	}

	drift := &manualCleanupRequiredError{
		err: &vcenter.PlacementDriftError{
			Kind:   vcenter.PlacementDriftDRS,
			Detail: "DRS override missing",
		},
	}
	if jobRetryAvailable(job, drift) {
		t.Fatalf("confirmed persisted-placement drift retained a retry: %v", drift)
	}

	job.RetryCount = job.MaxRetries
	if jobRetryAvailable(job, transient) {
		t.Fatal("exhausted operational placement failure retained a retry")
	}
	if jobRetryAvailable(job, errors.New("initial placement resolution failed")) {
		t.Fatal("unclassified placement failure became retryable")
	}
}

func TestClassifyCloneOperationFailurePreservesPriority(t *testing.T) {
	t.Parallel()

	drift := classifyCloneOperationFailure(&vcenter.PlacementDriftError{
		Kind:   vcenter.PlacementDriftHost,
		Detail: "wrong host",
	})
	if !isManualCleanupRequired(drift) {
		t.Fatalf("clone drift = %v, want manual cleanup", drift)
	}

	retry := &compensationRetryError{err: context.DeadlineExceeded}
	if got := classifyCloneOperationFailure(retry); got != retry {
		t.Fatalf("cleanup retry classification changed from %p to %p", retry, got)
	}

	ambiguous := classifyCloneOperationFailure(vcenter.ErrAmbiguousVMOwnership)
	if !isManualCleanupRequired(ambiguous) {
		t.Fatalf("ambiguous clone ownership = %v, want manual cleanup", ambiguous)
	}

	cleanupDrift := &compensationRetryError{
		err: &vcenter.PlacementDriftError{
			Kind:   vcenter.PlacementDriftHost,
			Detail: "cleanup target moved",
		},
	}
	if got := classifyPlacementValidationFailure(cleanupDrift); got != cleanupDrift {
		t.Fatalf("cleanup drift classification changed from %p to %p", cleanupDrift, got)
	}
}

func TestVMOperationPlacementGuardsDoNotWrapOperationalErrorsAsManualCleanup(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"power.go", "snapshot.go", "suspend.go"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		source := string(body)
		if !strings.Contains(source, "validatePersistedVMPlacement") {
			t.Fatalf("%s no longer validates persisted placement", path)
		}
		if strings.Contains(source, "&manualCleanupRequiredError") {
			t.Fatalf("%s directly wraps placement validation as manual cleanup", path)
		}
	}
}

func TestProvisioningResumePathsClassifyPlacementFailuresBeforeCleanup(t *testing.T) {
	t.Parallel()
	checks := map[string][]string{
		"create.go": {
			"jobRetryAvailable(job, planningErr)",
			"return p.failPodCreateWithCleanup(",
			"classifyCloneOperationFailure",
		},
		"vm_ops.go": {
			"jobRetryAvailable(job, jobErr)",
			"classifiedErr := classifyPlacementValidationFailure",
			"classifyCloneOperationFailure",
		},
		"clone_operation.go": {
			"classifiedErr := classifyPlacementValidationFailure",
			"if isManualCleanupRequired(classifiedErr)",
		},
		"../vcenter/inventory_lookup.go": {
			"newPlacementDrift(",
			"PlacementDriftHost",
		},
		"../vcenter/client.go": {
			"ValidateVMPlacementCleanupControl(",
			"ValidateVMCloneIdentity(",
		},
		"../vcenter/drs.go": {
			"ErrDRSControlMissing",
			"return c.EnsureVMPlacementControl(",
		},
	}
	for path, required := range checks {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range required {
			if !strings.Contains(string(body), needle) {
				t.Fatalf("%s is missing placement classification guard %q", path, needle)
			}
		}
	}
}

func validatePlacementRecoveryWiring(createSrc, vmOpsSrc, cloneSrc string) error {
	requiredCreate := []string{
		"if jobRetryAvailable(job, planningErr) {",
		`"placement planning",`,
		"return p.failPodCreateWithCleanup(",
	}
	for _, required := range requiredCreate {
		if !strings.Contains(createSrc, required) {
			return fmt.Errorf("pod_create recovery is missing %q", required)
		}
	}
	if strings.Count(createSrc, "jobRetryAvailable(job, classifiedErr)") != 1 {
		return errors.New("only post-persistence power-on may preserve an operational retry")
	}
	for _, stage := range []string{
		`"resume placement validation",`,
		`"power-on placement validation",`,
	} {
		stageIndex := strings.Index(createSrc, stage)
		if stageIndex < 0 {
			return fmt.Errorf("pod_create accepted-clone compensation is missing stage %s", stage)
		}
		start := max(0, stageIndex-500)
		if !strings.Contains(createSrc[start:stageIndex], "failPodCreateAfterCloneError(") {
			return fmt.Errorf("pod_create accepted-clone stage %s does not enter exact compensation", stage)
		}
	}
	requiredVMAdd := []string{
		"func (p *Provisioner) AddVM(ctx context.Context, job *models.Job) (retErr error)",
		"shouldCompensateVMAddFailure(job, retErr, vmRunningPersisted)",
		"!vmRunningPersisted",
		"vmRunningPersisted = true",
		"!jobRetryAvailable(job, jobErr)",
		"retErr = p.failVMAddWithCleanup(",
		"retErr = &compensatedJobError{err: retErr}",
		"jobRetryAvailable(job, jobErr)",
		"p.completeVMAddWithoutClone(",
		"p.finalizeVMAddCompensation(",
		"return p.failVMAddWithCleanup(",
		"p.vc.DestroyVMWithPlacement(",
	}
	for _, required := range requiredVMAdd {
		if !strings.Contains(vmOpsSrc, required) {
			return fmt.Errorf("vm_add recovery is missing %q", required)
		}
	}
	powerStart := strings.LastIndex(vmOpsSrc, "// Step 2: Power on")
	if powerStart < 0 {
		return errors.New("vm_add power-on phase is missing")
	}
	powerBody := vmOpsSrc[powerStart:]
	release := strings.Index(powerBody, "p.releaseVMPlacementCapacity(")
	markRunning := strings.Index(powerBody, "p.db.UpdatePodVMStatusFrom(")
	if release < 0 || markRunning < 0 || release > markRunning {
		return errors.New("vm_add must release materialized capacity before persisting running state")
	}
	reconcileStart := strings.Index(cloneSrc, "func reconcileCloneOperationTargetForCleanup(")
	if reconcileStart < 0 {
		return errors.New("clone cleanup reconciliation function is missing")
	}
	reconcileBody := cloneSrc[reconcileStart:]
	stage := strings.LastIndex(reconcileBody, "stageCloneCleanupTarget(")
	validate := strings.LastIndex(reconcileBody, "client.ValidateVMPlacement(")
	if stage < 0 || validate < 0 || stage > validate {
		return errors.New("clone cleanup must stage the exact target before placement validation")
	}
	return nil
}

func TestPlacementRecoveryWiringRejectsGuardSabotage(t *testing.T) {
	t.Parallel()
	read := func(path string) string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	createSrc := read("create.go")
	vmOpsSrc := read("vm_ops.go")
	cloneSrc := read("clone_operation.go")
	if err := validatePlacementRecoveryWiring(createSrc, vmOpsSrc, cloneSrc); err != nil {
		t.Fatal(err)
	}

	sabotages := map[string][3]string{
		"pod-create-retry": {
			strings.ReplaceAll(createSrc, "if jobRetryAvailable(job, planningErr) {", "if false {"),
			vmOpsSrc,
			cloneSrc,
		},
		"vm-add-terminal-status": {
			createSrc,
			strings.ReplaceAll(vmOpsSrc, "p.completeVMAddWithoutClone(", "removedCompleteVMAddWithoutClone("),
			cloneSrc,
		},
		"vm-add-terminal-compensation": {
			createSrc,
			strings.ReplaceAll(vmOpsSrc, "retErr = p.failVMAddWithCleanup(", "retErr = fmt.Errorf("),
			cloneSrc,
		},
		"vm-add-running-compensation-gate": {
			createSrc,
			strings.ReplaceAll(
				vmOpsSrc,
				"shouldCompensateVMAddFailure(job, retErr, vmRunningPersisted)",
				"true",
			),
			cloneSrc,
		},
		"exact-placement-destroy": {
			createSrc,
			strings.ReplaceAll(vmOpsSrc, "p.vc.DestroyVMWithPlacement(", "p.vc.DestroyVM("),
			cloneSrc,
		},
		"stage-before-drift": {
			createSrc,
			vmOpsSrc,
			strings.ReplaceAll(cloneSrc, "stageCloneCleanupTarget(", "removedStageCloneCleanupTarget("),
		},
	}
	for name, sources := range sabotages {
		t.Run(name, func(t *testing.T) {
			if err := validatePlacementRecoveryWiring(sources[0], sources[1], sources[2]); err == nil {
				t.Fatal("sabotaged placement recovery wiring unexpectedly passed")
			}
		})
	}
}

type cloneForwardRetryWiring struct {
	clone    string
	cloneDB  string
	create   string
	vmOps    string
	template string
	retrySQL string
}

func validateCloneForwardRetryWiring(src cloneForwardRetryWiring) error {
	required := map[string][]string{
		"clone operation": {
			"errors.Is(err, vcenter.ErrCloneTaskFailed)",
			`return "", newCloneForwardRetryError(fmt.Errorf(`,
			"return moref, cloneRecoveryError(validationErr, target)",
			"return moref, cloneRecoveryError(classifiedErr, target)",
			"func cloneForwardFailureToCompensation(err error) error",
			"store.GetVMCloneOperation(ctx, jobID, workerID)",
			"func persistedClonePreambleFailure(",
		},
		"clone operation DB": {
			"func (q *Queries) GetVMCloneOperation(",
			"AND claimed_by = $2",
			"validateVMCloneOperation(op)",
		},
		"pod create": {
			"if cloneForwardRetryAvailable(job, cloneErr) {",
			"return p.failPodCreateAfterCloneError(",
			"clonePreamblePending := payload.CloneOperation != nil",
			"retErr = persistedClonePreambleFailure(job, payload.CloneOperation, retErr)",
			"resumeCloneOperation := payload.CloneOperation != nil &&",
			"if existingVMMoref != \"\" && !resumeCloneOperation {",
			"pod became active before its persisted clone operation was reconciled",
			"refuse to overwrite existing pod VM reference %s with resumed clone %s",
			"persisted clone state reconciliation",
		},
		"VM add": {
			"if cloneForwardRetryAvailable(job, jobErr) {",
			"if isCloneForwardRetry(jobErr) {",
			"return cloneForwardFailureToCompensation(jobErr)",
			"clonePreamblePending := payload.CloneOperation != nil",
			"retErr = persistedClonePreambleFailure(job, payload.CloneOperation, retErr)",
			"payload.CloneOperation == nil",
			"pod entered %s before persisted clone reconciliation",
			"refuse to overwrite existing added VM reference %s with resumed clone %s",
		},
		"template smoke": {
			"isCloneForwardRetry(resultErr) &&",
			"cloneForwardRetryAvailable(job, resultErr) {",
			"if cloneForwardRetryAvailable(job, cloneErr) {",
			"return cloneForwardFailureToCompensation(cloneErr)",
			"if cloneForwardRetryAvailable(job, checkErr) {",
			"if isCloneForwardRetry(checkErr) {",
			"retErr = persistedClonePreambleFailure(job, payload.CloneOperation, retErr)",
			"err = persistedClonePreambleFailure(job, payload.CloneOperation, err)",
		},
		"retry SQL": {
			"ELSE payload - 'cleanup_only' - 'cleanup_completed'",
			"AND (vcenter_vm_id IS NULL OR vcenter_vm_id = $1)",
		},
	}
	bodies := map[string]string{
		"clone operation":    src.clone,
		"clone operation DB": src.cloneDB,
		"pod create":         src.create,
		"VM add":             src.vmOps,
		"template smoke":     src.template,
		"retry SQL":          src.retrySQL,
	}
	for component, fragments := range required {
		for _, fragment := range fragments {
			if !strings.Contains(bodies[component], fragment) {
				return fmt.Errorf("%s is missing clone forward-retry guard %q", component, fragment)
			}
		}
	}
	executeStart := strings.Index(src.clone, "func executeDurableVMClone(")
	executeEnd := strings.Index(src.clone, "func cloneCleanupTarget(")
	if executeStart < 0 || executeEnd <= executeStart {
		return errors.New("durable clone execution body is missing")
	}
	executeBody := src.clone[executeStart:executeEnd]
	load := strings.Index(executeBody, "store.GetVMCloneOperation(ctx, jobID, workerID)")
	freshOnly := strings.Index(executeBody, "if op == nil {")
	resolve := strings.Index(executeBody, "client.ResolveClonePlacement(ctx, params)")
	if load < 0 || freshOnly < 0 || resolve < 0 ||
		load > freshOnly || freshOnly > resolve {
		return errors.New("persisted clone operation must load before fresh placement resolution")
	}
	revalidateStart := strings.Index(src.template, "func revalidateL1TemplateJob(")
	revalidateEnd := strings.Index(src.template, "func (p *Provisioner) RevalidateL1Template(")
	if revalidateStart < 0 || revalidateEnd <= revalidateStart {
		return errors.New("template revalidation body is missing")
	}
	revalidateBody := src.template[revalidateStart:revalidateEnd]
	forwardReturn := strings.Index(revalidateBody, "if isCloneForwardRetry(checkErr) {")
	recordOutcome := strings.Index(revalidateBody, "revalidateL1TemplateCore(")
	if forwardReturn < 0 || recordOutcome < 0 || forwardReturn > recordOutcome {
		return errors.New("template revalidation records an outcome before returning clone forward retry")
	}
	if strings.Count(src.template, "payload.VMMoref = payload.CloneOperation.SourceRef") < 2 {
		return errors.New("template verify and revalidate do not both reuse the persisted source")
	}
	return nil
}

func TestCloneForwardRetryWiringRejectsGuardSabotage(t *testing.T) {
	t.Parallel()
	read := func(path string) string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	src := cloneForwardRetryWiring{
		clone:    read("clone_operation.go"),
		cloneDB:  read("../database/clone_operations.go"),
		create:   read("create.go"),
		vmOps:    read("vm_ops.go"),
		template: read("template_jobs.go"),
		retrySQL: read("../database/queries.go"),
	}
	if err := validateCloneForwardRetryWiring(src); err != nil {
		t.Fatal(err)
	}

	sabotages := map[string]cloneForwardRetryWiring{}
	sabotaged := src
	sabotaged.clone = strings.Replace(
		src.clone,
		`return "", newCloneForwardRetryError(fmt.Errorf(`,
		`return "", cloneRecoveryError(fmt.Errorf(`,
		1,
	)
	sabotages["task-wait"] = sabotaged
	sabotaged = src
	sabotaged.clone = strings.Replace(
		src.clone,
		"errors.Is(err, vcenter.ErrCloneTaskFailed)",
		"false",
		1,
	)
	sabotages["terminal-task"] = sabotaged
	sabotaged = src
	sabotaged.clone = strings.Replace(
		src.clone,
		"return moref, cloneRecoveryError(validationErr, target)",
		"return moref, newCloneForwardRetryError(validationErr, target)",
		1,
	)
	sabotages["post-clone-validation"] = sabotaged
	sabotaged = src
	sabotaged.clone = strings.Replace(
		src.clone,
		"return moref, cloneRecoveryError(classifiedErr, target)",
		"return moref, newCloneForwardRetryError(classifiedErr, target)",
		1,
	)
	sabotages["post-clone-configuration"] = sabotaged
	sabotaged = src
	sabotaged.create = strings.Replace(
		src.create,
		"if cloneForwardRetryAvailable(job, cloneErr) {",
		"if false {",
		1,
	)
	sabotages["pod-create-budget"] = sabotaged
	sabotaged = src
	sabotaged.vmOps = strings.Replace(
		src.vmOps,
		"return cloneForwardFailureToCompensation(jobErr)",
		"return jobErr",
		1,
	)
	sabotages["vm-add-exhaustion"] = sabotaged
	sabotaged = src
	sabotaged.template = strings.Replace(
		src.template,
		"isCloneForwardRetry(resultErr) &&",
		"false &&",
		1,
	)
	sabotages["smoke-defer"] = sabotaged
	sabotaged = src
	sabotaged.template = strings.Replace(
		src.template,
		"return cloneForwardFailureToCompensation(cloneErr)",
		"return cloneErr",
		1,
	)
	sabotages["smoke-exhaustion"] = sabotaged
	sabotaged = src
	sabotaged.template = strings.Replace(
		src.template,
		"if cloneForwardRetryAvailable(job, checkErr) {",
		"if false {",
		1,
	)
	sabotages["template-verify-state"] = sabotaged
	sabotaged = src
	sabotaged.retrySQL = strings.Replace(
		src.retrySQL,
		"ELSE payload - 'cleanup_only' - 'cleanup_completed'",
		"ELSE payload",
		1,
	)
	sabotages["forward-marker"] = sabotaged
	sabotaged = src
	sabotaged.clone = strings.Replace(
		src.clone,
		"store.GetVMCloneOperation(ctx, jobID, workerID)",
		"store.RemovedVMCloneOperation(ctx, jobID, workerID)",
		1,
	)
	sabotages["resume-before-resolve"] = sabotaged
	sabotaged = src
	executeStart := strings.Index(src.clone, "func executeDurableVMClone(")
	if executeStart < 0 {
		t.Fatal("durable clone execution body is missing")
	}
	sabotaged.clone = src.clone[:executeStart] + strings.Replace(
		src.clone[executeStart:],
		"if op == nil {",
		"if true {",
		1,
	)
	sabotages["fresh-resolution-guard"] = sabotaged
	sabotaged = src
	sabotaged.cloneDB = strings.Replace(
		src.cloneDB,
		"func (q *Queries) GetVMCloneOperation(",
		"func (q *Queries) RemovedVMCloneOperation(",
		1,
	)
	sabotages["claimed-operation-load"] = sabotaged
	sabotaged = src
	sabotaged.create = strings.Replace(
		src.create,
		"retErr = persistedClonePreambleFailure(job, payload.CloneOperation, retErr)",
		"retErr = retErr",
		1,
	)
	sabotages["pod-create-preamble"] = sabotaged
	sabotaged = src
	sabotaged.vmOps = strings.Replace(
		src.vmOps,
		"retErr = persistedClonePreambleFailure(job, payload.CloneOperation, retErr)",
		"retErr = retErr",
		1,
	)
	sabotages["vm-add-preamble"] = sabotaged
	sabotaged = src
	sabotaged.template = strings.Replace(
		src.template,
		"if isCloneForwardRetry(checkErr) {",
		"if false {",
		1,
	)
	sabotages["revalidation-outcome"] = sabotaged
	sabotaged = src
	sabotaged.template = strings.ReplaceAll(
		src.template,
		"payload.VMMoref = payload.CloneOperation.SourceRef",
		"payload.VMMoref = payload.VMMoref",
	)
	sabotages["persisted-template-source"] = sabotaged
	sabotaged = src
	sabotaged.create = strings.Replace(
		src.create,
		"pod became active before its persisted clone operation was reconciled",
		"pod became active and clone operation was ignored",
		1,
	)
	sabotages["pod-create-active-persisted-op"] = sabotaged
	sabotaged = src
	sabotaged.vmOps = strings.Replace(
		src.vmOps,
		"pod entered %s before persisted clone reconciliation",
		"pod entered %s and clone operation was ignored",
		1,
	)
	sabotages["vm-add-nonactive-persisted-op"] = sabotaged
	sabotaged = src
	sabotaged.create = strings.Replace(
		src.create,
		"refuse to overwrite existing pod VM reference %s with resumed clone %s",
		"overwrite existing pod VM reference %s with resumed clone %s",
		1,
	)
	sabotages["pod-create-reference-mismatch"] = sabotaged
	sabotaged = src
	sabotaged.vmOps = strings.Replace(
		src.vmOps,
		"refuse to overwrite existing added VM reference %s with resumed clone %s",
		"overwrite existing added VM reference %s with resumed clone %s",
		1,
	)
	sabotages["vm-add-reference-mismatch"] = sabotaged
	sabotaged = src
	sabotaged.retrySQL = strings.Replace(
		src.retrySQL,
		"AND (vcenter_vm_id IS NULL OR vcenter_vm_id = $1)",
		"AND true",
		1,
	)
	sabotages["atomic-clone-adoption-reference"] = sabotaged

	for name, candidate := range sabotages {
		t.Run(name, func(t *testing.T) {
			if err := validateCloneForwardRetryWiring(candidate); err == nil {
				t.Fatal("sabotaged clone forward-retry wiring unexpectedly passed")
			}
		})
	}
}

func TestSelectedPlacementHostsReturnsSortedUnion(t *testing.T) {
	placements := []models.VMPlacement{
		{HostMoref: "host-3"},
		{HostMoref: "host-1"},
		{HostMoref: "host-3"},
		{HostMoref: "host-2"},
	}
	got := selectedPlacementHosts(placements)
	want := []string{"host-1", "host-2", "host-3"}
	if len(got) != len(want) {
		t.Fatalf("selected hosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selected hosts = %v, want %v", got, want)
		}
	}
}

func TestExistingPortGroupReceiptRequiresSelectedHostUnion(t *testing.T) {
	jobID := uuid.New()
	engine := rollback.New(jobID, nil, nil)
	receipt := vcenter.PortGroupReceipt{
		Name:   "Pod-VLAN500",
		VLANID: 500,
		Hosts: []vcenter.PortGroupHostReceipt{{
			HostName:     "esxi1",
			HostMoRef:    "host-1",
			ComputeMoRef: "domain-c1",
		}},
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	engine.LoadSteps([]rollback.Step{{
		Name: "portgroup_create",
		Data: data,
	}})

	_, err = existingPortGroupReceipt(engine, receipt.Name, receipt.VLANID, []string{"host-1", "host-2"})
	if err == nil || !strings.Contains(err.Error(), "host-2") {
		t.Fatalf("missing host coverage error = %v", err)
	}
}

func TestExistingPortGroupReceiptUsesFirstAuthoritativeReceipt(t *testing.T) {
	engine := rollback.New(uuid.New(), nil, nil)
	first := vcenter.PortGroupReceipt{
		Name:   "Pod-VLAN500",
		VLANID: 500,
		Hosts: []vcenter.PortGroupHostReceipt{{
			HostName:     "esxi1",
			HostMoRef:    "host-1",
			ComputeMoRef: "domain-c1",
		}},
	}
	retry := vcenter.PortGroupReceipt{
		Name:   first.Name,
		VLANID: first.VLANID,
		Hosts: []vcenter.PortGroupHostReceipt{{
			HostName:     "esxi2",
			HostMoRef:    "host-2",
			ComputeMoRef: "domain-c1",
			Preexisting:  true,
		}},
	}
	firstData, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	retryData, err := json.Marshal(retry)
	if err != nil {
		t.Fatal(err)
	}
	engine.LoadSteps([]rollback.Step{
		{Name: "portgroup_create", Data: firstData},
		{Name: "portgroup_create", Data: retryData},
	})

	got, err := existingPortGroupReceipt(engine, first.Name, first.VLANID, []string{"host-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got.Hosts) != 1 || got.Hosts[0].HostMoRef != "host-1" {
		t.Fatalf("selected receipt = %+v, want first authoritative receipt", got)
	}
}

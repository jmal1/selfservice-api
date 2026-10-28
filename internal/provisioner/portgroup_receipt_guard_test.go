package provisioner

import (
	"errors"
	"os"
	"strings"
	"testing"
)

type portGroupReceiptWiring struct {
	create     string
	destroy    string
	database   string
	portgroups string
	closer     string
}

func validatePortGroupReceiptWiring(src portGroupReceiptWiring) error {
	createStart := strings.Index(src.create, "func (p *Provisioner) CreatePod(")
	if createStart < 0 {
		return errors.New("pod create implementation is missing")
	}
	createBody := src.create[createStart:]
	plan := strings.Index(createBody, "prepareVMPlacementPlan(")
	drsPreflight := strings.Index(createBody, "ValidateDRSPlacementPrivileges(")
	vlanMutation := strings.Index(createBody, "CreateVLAN(")
	if plan < 0 || drsPreflight < 0 || vlanMutation < 0 ||
		plan > drsPreflight || drsPreflight > vlanMutation {
		return errors.New("complete placement and mandatory DRS privilege validation must precede network mutation")
	}
	persist := strings.Index(createBody, "PersistPodPortGroupReceipt(")
	rollback := strings.Index(createBody, `rb.Record(lockCtx, "portgroup_create"`)
	begin := strings.Index(createBody, "BeginPodPortGroupMutation(")
	apply := strings.Index(createBody, "ApplyPortGroupMutation(")
	capture := strings.Index(createBody, "CapturePortGroupKeys(")
	bind := strings.Index(createBody, "PersistPodPortGroupKeys(")
	if persist < 0 || rollback < 0 || begin < 0 || apply < 0 || capture < 0 || bind < 0 ||
		persist > rollback || rollback > begin || begin > apply || apply > capture || capture > bind {
		return errors.New("create must persist its plan and rollback before authorizing mutation, then bind the stable key before cloning")
	}
	for _, fragment := range []string{
		"FROM pod_portgroup_receipts",
		"FOR UPDATE",
		"receipt = $2::jsonb",
		"current != key",
		"removedAt != nil",
		"PodPortGroupReceiptLegacy",
		"PodPortGroupReceiptPlanned",
		"PodPortGroupReceiptApplying",
	} {
		if !strings.Contains(src.database, fragment) {
			return errors.New("database receipt ledger is missing conflict or tombstone fencing")
		}
	}
	for _, fragment := range []string{
		"receiptRecord.RemovedAt == nil",
		"PortGroupReceiptWithKeys(receipt, receiptRecord.Keys)",
		"DeletePortGroupMutation(lockCtx, receipt)",
		"MarkPodPortGroupRemoved(lockCtx, pod.ID, receiptRecord.Receipt)",
	} {
		if !strings.Contains(src.destroy, fragment) {
			return errors.New("destroy is missing exact keyed cleanup or tombstone completion")
		}
	}
	if !strings.Contains(src.create, "closeAbsentApplyingReceipt(ctx, podID, record, receipt)") ||
		!strings.Contains(src.destroy, "closeAbsentApplyingReceipt(ctx, pod.ID, receiptRecord, receipt)") ||
		!strings.Contains(src.destroy, "!absentClosed") {
		return errors.New("create rollback and destroy must close verified-absent applying receipts before keyed cleanup")
	}
	lock := strings.Index(src.closer, "WithVCenterPortGroupMutationLock(")
	absent := strings.Index(src.closer, "OwnedPortGroupsAbsent(lockCtx")
	mark := strings.Index(src.closer, "MarkPodPortGroupAbsentRemoved(lockCtx")
	if lock < 0 || absent < 0 || mark < 0 || lock > absent || absent > mark ||
		!strings.Contains(src.closer, "len(record.Keys) > 0") {
		return errors.New("absent receipt close must verify absence under the mutation lock before tombstoning, and never for keyed receipts")
	}
	if strings.Contains(src.create, "case database.PodPortGroupReceiptLegacy") ||
		strings.Contains(src.destroy, "receiptRecord.State == database.PodPortGroupReceiptLegacy") {
		return errors.New("legacy receipt state must not reach inventory-based key capture or deletion")
	}
	for _, fragment := range []string{
		"findPortGroupOnHostByIdentity(",
		"host.PortGroupKey",
		"absent without a stable key",
		"host.Preexisting",
	} {
		if !strings.Contains(src.portgroups, fragment) {
			return errors.New("vCenter cleanup is missing stable-key or preexisting-resource protection")
		}
	}
	return nil
}

func TestPortGroupReceiptWiringRejectsSabotage(t *testing.T) {
	t.Parallel()
	read := func(path string) string {
		t.Helper()
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	src := portGroupReceiptWiring{
		create:     read("create.go"),
		destroy:    read("destroy.go"),
		database:   read("../database/portgroup_receipts.go"),
		portgroups: read("../vcenter/portgroups.go"),
		closer:     read("portgroup_receipt_close.go"),
	}
	if err := validatePortGroupReceiptWiring(src); err != nil {
		t.Fatal(err)
	}

	sabotages := map[string]portGroupReceiptWiring{}
	sabotaged := src
	sabotaged.create = strings.Replace(
		src.create,
		"PersistPodPortGroupReceipt(",
		"RemovedPodPortGroupReceiptPersistence(",
		1,
	)
	sabotages["durable-plan-before-mutation"] = sabotaged
	sabotaged = src
	sabotaged.create = strings.Replace(
		src.create,
		"BeginPodPortGroupMutation(",
		"RemovedPodPortGroupMutationIntent(",
		1,
	)
	sabotages["durable-mutation-intent"] = sabotaged
	sabotaged = src
	sabotaged.portgroups = strings.ReplaceAll(
		src.portgroups,
		"findPortGroupOnHostByIdentity(",
		"findPortGroupOnHostByNameOnly(",
	)
	sabotages["stable-key-cleanup"] = sabotaged
	sabotaged = src
	sabotaged.create = strings.Replace(
		src.create,
		"ValidateDRSPlacementPrivileges(",
		"RemovedDRSPrivilegePreflight(",
		1,
	)
	sabotages["drs-preflight-before-network"] = sabotaged
	sabotaged = src
	sabotaged.create = strings.Replace(
		src.create,
		"case database.PodPortGroupReceiptApplying, database.PodPortGroupReceiptActive:",
		"case database.PodPortGroupReceiptLegacy, database.PodPortGroupReceiptApplying, database.PodPortGroupReceiptActive:",
		1,
	)
	sabotages["legacy-inventory-adoption"] = sabotaged
	sabotaged = src
	sabotaged.closer = strings.Replace(src.closer, "OwnedPortGroupsAbsent(lockCtx", "AssumePortGroupsAbsent(lockCtx", 1)
	sabotages["absent-close-without-verification"] = sabotaged
	sabotaged = src
	sabotaged.closer = strings.Replace(src.closer, "len(record.Keys) > 0", "false", 1)
	sabotages["absent-close-on-keyed-receipt"] = sabotaged
	sabotaged = src
	sabotaged.destroy = strings.Replace(src.destroy, "closeAbsentApplyingReceipt(ctx, pod.ID, receiptRecord, receipt)", "skipAbsentClose()", 1)
	sabotages["destroy-cannot-close-absent-receipt"] = sabotaged

	for name, candidate := range sabotages {
		t.Run(name, func(t *testing.T) {
			if err := validatePortGroupReceiptWiring(candidate); err == nil {
				t.Fatal("sabotaged portgroup receipt wiring unexpectedly passed")
			}
		})
	}
}

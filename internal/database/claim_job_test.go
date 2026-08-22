package database

import (
	"strings"
	"testing"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestClaimJobMaintenanceFilterExcludesOnlyCreateAndAdd(t *testing.T) {
	blocked := blockedJobTypesForClaim(false)
	want := map[string]bool{
		models.JobTypePodCreate: true,
		models.JobTypeVMAdd:     true,
	}
	if len(blocked) != len(want) {
		t.Fatalf("blocked = %v, want exactly pod_create and vm_add", blocked)
	}
	for _, jobType := range blocked {
		if !want[jobType] {
			t.Errorf("unexpected blocked job type %q", jobType)
		}
		delete(want, jobType)
	}
	for jobType := range want {
		t.Errorf("expected blocked job type %q is missing", jobType)
	}

	for _, claimable := range []string{
		models.JobTypePodDestroy,
		models.JobTypeVMDestroy,
		models.JobTypeVMStart,
		models.JobTypeVMStop,
		models.JobTypeVMSnapshotDelete,
		models.JobTypeImageImport,
	} {
		for _, jobType := range blocked {
			if claimable == jobType {
				t.Errorf("cleanup/safe job %q was blocked", claimable)
			}
		}
	}
}

func TestClaimJobEnabledBlocksNothing(t *testing.T) {
	if blocked := blockedJobTypesForClaim(true); len(blocked) != 0 {
		t.Fatalf("blocked = %v, want no exclusions when claims are enabled", blocked)
	}
}

func TestClaimJobSQLFiltersBeforeClaim(t *testing.T) {
	sql := strings.ToUpper(claimJobSQL)
	for _, fragment := range []string{
		"WHERE STATUS = 'PENDING'",
		"TYPE <> ALL($2)",
		"FOR UPDATE SKIP LOCKED",
	} {
		if !strings.Contains(sql, fragment) {
			t.Errorf("claimJobSQL missing %q; provisioning jobs could be claimed then abandoned", fragment)
		}
	}
}

func TestClaimJobSQLFilterSabotageIsDetected(t *testing.T) {
	sabotaged := strings.Replace(claimJobSQL, "AND type <> ALL($2)", "", 1)
	if strings.Contains(strings.ToUpper(sabotaged), "TYPE <> ALL($2)") {
		t.Fatal("sabotage fixture still contains the claim-time maintenance filter")
	}
}

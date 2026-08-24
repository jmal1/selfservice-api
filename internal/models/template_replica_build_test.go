package models

import "testing"

func TestTemplateReplicaBuildForwardResumePhaseAllowlist(t *testing.T) {
	for _, phase := range []string{
		TemplateReplicaBuildPhasePending,
		TemplateReplicaBuildPhaseCloneSubmitting,
		TemplateReplicaBuildPhaseCloneSubmitted,
		TemplateReplicaBuildPhaseValidating,
		TemplateReplicaBuildPhaseSnapshotSubmitting,
		TemplateReplicaBuildPhaseSnapshotSubmitted,
		TemplateReplicaBuildPhaseCanaryPrepared,
		TemplateReplicaBuildPhaseCanarySubmitting,
		TemplateReplicaBuildPhaseCanarySubmitted,
		TemplateReplicaBuildPhaseCleanupPrepared,
		TemplateReplicaBuildPhaseCleanupSubmitting,
		TemplateReplicaBuildPhaseCleanupSubmitted,
		TemplateReplicaBuildPhaseFinalizing,
	} {
		if !IsTemplateReplicaBuildForwardPhase(phase) {
			t.Fatalf("forward phase %q is not resumable", phase)
		}
	}
	for _, phase := range []string{
		TemplateReplicaBuildPhaseResiduePrepared,
		TemplateReplicaBuildPhaseResidueSubmitting,
		TemplateReplicaBuildPhaseResidueSubmitted,
		TemplateReplicaBuildPhaseResidueCleaned,
		TemplateReplicaBuildPhaseReady,
		TemplateReplicaBuildPhaseFailed,
		TemplateReplicaBuildPhaseCleanupRequired,
		TemplateReplicaBuildPhaseRetired,
		"invalid",
	} {
		if IsTemplateReplicaBuildForwardPhase(phase) {
			t.Fatalf("cleanup or terminal phase %q is resumable", phase)
		}
	}
}

// Package worklease coordinates exclusive reconciler runs across worker replicas
// using Postgres row leases (claim token + heartbeat). There is no process-wide
// leader: each named unit of work is claimed independently.
package worklease

// Stable lease names. Guard-tested so code and migration seeds cannot drift.
const (
	RetryFailedDestroys   = "retry_failed_destroys"
	ExpireStale           = "expire_stale"
	StuckUploads          = "stuck_uploads"
	VCenterOrphan         = "vcenter_orphan"
	TemplateOrphan        = "template_orphan"
	PodVMIP               = "podvm_ip"
	NetworkReconcile      = "network_reconcile"
	IdleEval              = "idle_eval"
	TemplateMetrics       = "template_metrics"
	RetryPending          = "retry_pending"
	L1Validation          = "l1_validation"
	TemplateHealth        = "template_health"
	TemplateHealthConfirm = "template_health_confirm"
	SuspendMetrics        = "suspend_metrics"
)

// AllNames is the canonical set seeded by migration 000040.
func AllNames() []string {
	return []string{
		RetryFailedDestroys,
		ExpireStale,
		StuckUploads,
		VCenterOrphan,
		TemplateOrphan,
		PodVMIP,
		NetworkReconcile,
		IdleEval,
		TemplateMetrics,
		RetryPending,
		L1Validation,
		TemplateHealth,
		TemplateHealthConfirm,
		SuspendMetrics,
	}
}

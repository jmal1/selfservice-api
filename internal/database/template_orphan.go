package database

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

// StaleWizardTemplateVM is a wizard row whose staging VM is eligible for
// background cleanup: state is error or draft, a moref is still recorded, and
// the row has not been touched within the inactivity window.
type StaleWizardTemplateVM struct {
	ID          uuid.UUID
	Name        string
	State       string
	VCenterVMID string
	UpdatedAt   time.Time
}

// ListStaleWizardTemplateVMs returns error/draft templates that still point at
// a vCenter VM and whose updated_at is older than olderThan.
func (q *Queries) ListStaleWizardTemplateVMs(ctx context.Context, olderThan time.Duration) ([]StaleWizardTemplateVM, error) {
	if olderThan <= 0 {
		olderThan = 24 * time.Hour
	}
	rows, err := q.pool.Query(ctx, `
		SELECT id, name, template_state, vcenter_vm_id, updated_at
		FROM templates
		WHERE template_state = ANY($1)
		  AND vcenter_vm_id IS NOT NULL
		  AND vcenter_vm_id <> ''
		  AND updated_at < now() - $2::interval
		ORDER BY updated_at ASC
	`, []string{models.TemplateStateError, models.TemplateStateDraft},
		fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StaleWizardTemplateVM
	for rows.Next() {
		var row StaleWizardTemplateVM
		if err := rows.Scan(&row.ID, &row.Name, &row.State, &row.VCenterVMID, &row.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListOwnedTemplateFolderMorefs returns every MoRef Crucible currently claims
// in the Templates folder: live template staging/published VMs, retained
// source replicas, and in-flight replica-build destination/canary VMs that
// have not finished cleanup.
func (q *Queries) ListOwnedTemplateFolderMorefs(ctx context.Context) (map[string]struct{}, error) {
	out := make(map[string]struct{})

	add := func(moref string) {
		if moref != "" {
			out[moref] = struct{}{}
		}
	}

	tmplRows, err := q.pool.Query(ctx, `
		SELECT vcenter_vm_id
		FROM templates
		WHERE vcenter_vm_id IS NOT NULL AND vcenter_vm_id <> ''
	`)
	if err != nil {
		return nil, fmt.Errorf("list template morefs: %w", err)
	}
	defer tmplRows.Close()
	for tmplRows.Next() {
		var moref string
		if err := tmplRows.Scan(&moref); err != nil {
			return nil, err
		}
		add(moref)
	}
	if err := tmplRows.Err(); err != nil {
		return nil, err
	}

	replicaRows, err := q.pool.Query(ctx, `
		SELECT source_vm_moref
		FROM template_source_replicas
		WHERE source_vm_moref IS NOT NULL AND source_vm_moref <> ''
	`)
	if err != nil {
		return nil, fmt.Errorf("list source replica morefs: %w", err)
	}
	defer replicaRows.Close()
	for replicaRows.Next() {
		var moref string
		if err := replicaRows.Scan(&moref); err != nil {
			return nil, err
		}
		add(moref)
	}
	if err := replicaRows.Err(); err != nil {
		return nil, err
	}

	buildRows, err := q.pool.Query(ctx, `
		SELECT destination_vm_moref, canary_vm_moref
		FROM template_replica_builds
		WHERE (
			(destination_vm_moref <> '' AND residue_cleaned_at IS NULL)
			OR (canary_vm_moref <> '' AND cleanup_completed_at IS NULL)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("list replica build morefs: %w", err)
	}
	defer buildRows.Close()
	for buildRows.Next() {
		var dest, canary string
		if err := buildRows.Scan(&dest, &canary); err != nil {
			return nil, err
		}
		add(dest)
		add(canary)
	}
	if err := buildRows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

// ClearTemplateVCenterVMIfMatch clears templates.vcenter_vm_id only when it
// still equals expectedMoref. Prevents a race where a concurrent provision
// wrote a new moref after this reconciler decided to destroy an older one.
func (q *Queries) ClearTemplateVCenterVMIfMatch(ctx context.Context, id uuid.UUID, expectedMoref string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE templates
		SET vcenter_vm_id = '',
		    guest_credentials_verified_at = NULL,
		    updated_at = NOW()
		WHERE id = $1
		  AND vcenter_vm_id = $2
	`, id, expectedMoref)
	return err
}

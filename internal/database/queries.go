package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jmal1/selfservice-api/internal/models"
)

// Queries provides type-safe database operations.
type Queries struct {
	pool *pgxpool.Pool
}

// NewQueries creates a new Queries instance.
func NewQueries(pool *pgxpool.Pool) *Queries {
	return &Queries{pool: pool}
}

// Pool exposes the underlying connection pool (e.g. for transactions in handlers).
func (q *Queries) Pool() *pgxpool.Pool {
	return q.pool
}

// ErrTemplateStale is returned by UpdateTemplate when the caller passed
// an ExpectedUpdatedAt that no longer matches the current row, meaning
// another admin has saved an edit since the caller's last read. The
// handler should translate this into HTTP 409 Conflict so the UI can
// prompt the operator to re-load and reconcile.
var ErrTemplateStale = errors.New("template was modified by another user")

// ErrImageUploadStale is returned by the image_uploads guarded status
// transitions when the row is not in the expected state — e.g. two
// clients raced an import, or a retry arrived after the first attempt
// already moved the row. Handlers should surface HTTP 409.
var ErrImageUploadStale = errors.New("image upload is not in the expected state")

// templateSelectCols is the canonical list of columns returned by every
// Template SELECT / INSERT RETURNING / UPDATE RETURNING. Keep in lockstep
// with scanTemplate so the order matches the Scan() argument list.
// Migration 000018 added template_state, created_by, vcenter_vm_id,
// source_type, source_ref, staging_network. Migration 000019 added
// is_internal.
const templateSelectCols = `id, name, vcenter_template, os_type, default_vcpus, default_ram_mb,
		default_disk_gb, min_vcpus, min_ram_mb, COALESCE(description, ''), COALESCE(icon_url, ''),
		default_username, default_password, kind, assign_ip, is_active,
		template_state, created_by, vcenter_vm_id, source_type, source_ref, staging_network,
		is_internal,
		unattend_mode, unattend_config, guest_id,
		created_at, updated_at,
		trust_tier, last_validated_at, last_validation_result`

// scanTemplate populates t from a row whose columns are in templateSelectCols
// order. Centralizes the column ordering so adding a column in the future
// only requires updating this function + templateSelectCols above.
func scanTemplate(row pgx.Row, t *models.Template) error {
	return row.Scan(
		&t.ID, &t.Name, &t.VCenterTemplate, &t.OSType, &t.DefaultVCPUs, &t.DefaultRAMMB,
		&t.DefaultDiskGB, &t.MinVCPUs, &t.MinRAMMB, &t.Description, &t.IconURL,
		&t.DefaultUsername, &t.DefaultPassword, &t.Kind, &t.AssignIP, &t.IsActive,
		&t.TemplateState, &t.CreatedBy, &t.VCenterVMID, &t.SourceType, &t.SourceRef, &t.StagingNetwork,
		&t.IsInternal,
		&t.UnattendMode, &t.UnattendConfig, &t.GuestID,
		&t.CreatedAt, &t.UpdatedAt,
		&t.TrustTier, &t.LastValidatedAt, &t.LastValidationResult,
	)
}

// --- Users ---

// UpsertUser creates or updates a user from OIDC claims.
func (q *Queries) UpsertUser(ctx context.Context, u *models.User) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO users (oidc_sub, username, email, display_name, role, max_vcpus, max_ram_mb, max_pods)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (oidc_sub) DO UPDATE SET
			username = EXCLUDED.username,
			email = EXCLUDED.email,
			display_name = EXCLUDED.display_name,
			role = EXCLUDED.role,
			updated_at = now()
		RETURNING id, created_at, updated_at
	`, u.OIDCSub, u.Username, u.Email, u.DisplayName, u.Role, u.MaxVCPUs, u.MaxRAMMB, u.MaxPods)
	return err
}

// GetUserBySub retrieves a user by OIDC subject ID.
func (q *Queries) GetUserBySub(ctx context.Context, sub string) (*models.User, error) {
	var u models.User
	err := q.pool.QueryRow(ctx, `
		SELECT id, oidc_sub, username, email, COALESCE(display_name, ''), role,
		       max_vcpus, max_ram_mb, max_pods, is_active, created_at, updated_at
		FROM users WHERE oidc_sub = $1
	`, sub).Scan(
		&u.ID, &u.OIDCSub, &u.Username, &u.Email, &u.DisplayName, &u.Role,
		&u.MaxVCPUs, &u.MaxRAMMB, &u.MaxPods, &u.IsActive, &u.CreatedAt, &u.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &u, err
}

// GetUserByID retrieves a user by UUID.
func (q *Queries) GetUserByID(ctx context.Context, id uuid.UUID) (*models.User, error) {
	var u models.User
	err := q.pool.QueryRow(ctx, `
		SELECT id, oidc_sub, username, email, COALESCE(display_name, ''), role,
		       max_vcpus, max_ram_mb, max_pods, is_active, created_at, updated_at
		FROM users WHERE id = $1
	`, id).Scan(
		&u.ID, &u.OIDCSub, &u.Username, &u.Email, &u.DisplayName, &u.Role,
		&u.MaxVCPUs, &u.MaxRAMMB, &u.MaxPods, &u.IsActive, &u.CreatedAt, &u.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &u, err
}

// ListUsers returns all users (admin).
func (q *Queries) ListUsers(ctx context.Context) ([]models.User, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, oidc_sub, username, email, COALESCE(display_name, ''), role,
		       max_vcpus, max_ram_mb, max_pods, is_active, created_at, updated_at
		FROM users ORDER BY username
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.User
	for rows.Next() {
		var u models.User
		if err := rows.Scan(
			&u.ID, &u.OIDCSub, &u.Username, &u.Email, &u.DisplayName, &u.Role,
			&u.MaxVCPUs, &u.MaxRAMMB, &u.MaxPods, &u.IsActive, &u.CreatedAt, &u.UpdatedAt,
		); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

// UpdateUserQuotas updates a user's resource quotas (admin).
func (q *Queries) UpdateUserQuotas(ctx context.Context, id uuid.UUID, req models.UpdateQuotaRequest) error {
	query := "UPDATE users SET updated_at = now()"
	args := []any{}
	argIdx := 1

	if req.MaxVCPUs != nil {
		query += fmt.Sprintf(", max_vcpus = $%d", argIdx)
		args = append(args, *req.MaxVCPUs)
		argIdx++
	}
	if req.MaxRAMMB != nil {
		query += fmt.Sprintf(", max_ram_mb = $%d", argIdx)
		args = append(args, *req.MaxRAMMB)
		argIdx++
	}
	if req.MaxPods != nil {
		query += fmt.Sprintf(", max_pods = $%d", argIdx)
		args = append(args, *req.MaxPods)
		argIdx++
	}

	query += fmt.Sprintf(" WHERE id = $%d", argIdx)
	args = append(args, id)

	_, err := q.pool.Exec(ctx, query, args...)
	return err
}

// --- Templates ---

// CreateTemplate inserts a new template.
func (q *Queries) CreateTemplate(ctx context.Context, req models.CreateTemplateRequest) (*models.Template, error) {
	kind := req.Kind
	if kind == "" {
		kind = models.TemplateKindCloneWithCustomize
	}
	assignIP := true
	if req.AssignIP != nil {
		assignIP = *req.AssignIP
	}
	var t models.Template
	err := scanTemplate(q.pool.QueryRow(ctx, `
		INSERT INTO templates (name, vcenter_template, os_type, default_vcpus, default_ram_mb,
		                       default_disk_gb, min_vcpus, min_ram_mb, description, icon_url,
		                       default_username, default_password, kind, assign_ip)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		RETURNING `+templateSelectCols+`
	`, req.Name, req.VCenterTemplate, req.OSType, req.DefaultVCPUs, req.DefaultRAMMB,
		req.DefaultDiskGB, req.MinVCPUs, req.MinRAMMB, req.Description, req.IconURL,
		req.DefaultUsername, req.DefaultPassword, kind, assignIP,
	), &t)
	return &t, err
}

// ListTemplatesForUser returns templates accessible to a user based on their
// role. A template is visible if any of the following hold:
//
//   - it has no template_access rules (open to all)
//   - the user's role appears in template_access.role
//   - the user's id appears in template_access.user_id
//   - 'admin' appears in template_access.role (admins see everything)
//
// Uses EXISTS subqueries instead of a LEFT JOIN so the SELECT list (sharing
// the unqualified `templateSelectCols` const) is unambiguous. The previous
// LEFT JOIN form silently broke once a real template_access row existed
// because `id` resolves to both templates.id and template_access.id.
func (q *Queries) ListTemplatesForUser(ctx context.Context, userID uuid.UUID, role string) ([]models.Template, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT `+templateSelectCols+`
		FROM templates t
		WHERE t.is_active = true
		  AND (NOT EXISTS (SELECT 1 FROM template_access ta WHERE ta.template_id = t.id)
		       OR EXISTS (SELECT 1 FROM template_access ta
		                  WHERE ta.template_id = t.id
		                    AND (ta.role = $1 OR ta.user_id = $2 OR ta.role = 'admin')))
		ORDER BY t.name
	`, role, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []models.Template
	for rows.Next() {
		var t models.Template
		if err := scanTemplate(rows, &t); err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// ListExplicitTemplateAccessForUser returns the set of template IDs for which
// the user has an explicit per-user-id template_access grant (i.e. NOT a
// role-based grant and NOT the "no rules = open" fallback). Callers use this
// to selectively bypass is_internal filtering — internal templates remain
// hidden from the public picker unless the user was specifically granted
// access by user_id (e.g. the synthetic monitor user).
func (q *Queries) ListExplicitTemplateAccessForUser(ctx context.Context, userID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	rows, err := q.pool.Query(ctx,
		`SELECT DISTINCT template_id FROM template_access WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]struct{})
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, nil
}

// ListAllTemplates returns all templates (admin).
func (q *Queries) ListAllTemplates(ctx context.Context) ([]models.Template, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT `+templateSelectCols+`
		FROM templates ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []models.Template
	for rows.Next() {
		var t models.Template
		if err := scanTemplate(rows, &t); err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// SetTemplateAccess replaces all access rules for a template.
func (q *Queries) SetTemplateAccess(ctx context.Context, templateID uuid.UUID, rules []models.AccessRule) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Clear existing rules
	if _, err := tx.Exec(ctx, "DELETE FROM template_access WHERE template_id = $1", templateID); err != nil {
		return err
	}

	// Insert new rules
	for _, rule := range rules {
		if _, err := tx.Exec(ctx,
			"INSERT INTO template_access (template_id, user_id, role) VALUES ($1, $2, $3)",
			templateID, rule.UserID, rule.Role,
		); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// UpdateTemplate partially updates a template by ID.
func (q *Queries) UpdateTemplate(ctx context.Context, id uuid.UUID, req models.UpdateTemplateRequest) (*models.Template, error) {
	// Optimistic concurrency: if the caller provided ExpectedUpdatedAt,
	// gate the UPDATE on the row still being at that version. A
	// mismatch returns 0 rows from the UPDATE, which we differentiate
	// from "row does not exist" by a follow-up existence probe so the
	// handler can return 409 (stale) vs 404 (gone).
	var t models.Template
	err := scanTemplate(q.pool.QueryRow(ctx, `
		UPDATE templates SET
			name = COALESCE($2, name),
			description = COALESCE($3, description),
			icon_url = COALESCE($4, icon_url),
			default_vcpus = COALESCE($5, default_vcpus),
			default_ram_mb = COALESCE($6, default_ram_mb),
			default_disk_gb = COALESCE($7, default_disk_gb),
			is_active = COALESCE($8, is_active),
			default_username = COALESCE($9, default_username),
			default_password = COALESCE($10, default_password),
			kind = COALESCE($11, kind),
			assign_ip = COALESCE($12, assign_ip)
		WHERE id = $1
		  AND ($13::timestamptz IS NULL OR updated_at = $13)
		RETURNING `+templateSelectCols+`
	`, id, req.Name, req.Description, req.IconURL, req.DefaultVCPUs, req.DefaultRAMMB, req.DefaultDiskGB, req.IsActive,
		req.DefaultUsername, req.DefaultPassword, req.Kind, req.AssignIP, req.ExpectedUpdatedAt,
	), &t)
	if err == pgx.ErrNoRows {
		// Distinguish missing-row from version-mismatch. If the caller
		// did not supply ExpectedUpdatedAt at all the only reason
		// UPDATE returned 0 rows is that the row truly doesn't exist.
		if req.ExpectedUpdatedAt == nil {
			return nil, nil
		}
		var exists bool
		if err2 := q.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM templates WHERE id = $1)`, id,
		).Scan(&exists); err2 != nil {
			return nil, err2
		}
		if !exists {
			return nil, nil
		}
		return nil, ErrTemplateStale
	}
	return &t, err
}

// DeleteTemplate removes a template by ID.
func (q *Queries) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	_, err := q.pool.Exec(ctx, "DELETE FROM templates WHERE id = $1", id)
	return err
}

// DeleteTemplateWithHistory removes a template together with any lingering
// pod_vms rows that still reference it. It exists because pod_vms.template_id
// is a NOT NULL / NO ACTION foreign key: historical rows from long-destroyed
// pods keep pointing at the template, so a plain DELETE FROM templates is
// rejected with "violates foreign key constraint pod_vms_template_id_fkey"
// (SQLSTATE 23503) and the operator gets a 500 — after the staging VM was
// already destroyed. Callers MUST first ensure no ACTIVE pod depends on the
// template (see ListTemplateDependents); this only mops up the leftover
// destroyed-pod audit rows so the template row can go. Both deletes run in
// one transaction so we never orphan half the delete.
func (q *Queries) DeleteTemplateWithHistory(ctx context.Context, id uuid.UUID) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "DELETE FROM pod_vms WHERE template_id = $1", id); err != nil {
		return fmt.Errorf("delete pod_vms history: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM templates WHERE id = $1", id); err != nil {
		return fmt.Errorf("delete template: %w", err)
	}
	return tx.Commit(ctx)
}

// CountTemplateBlueprintRefs returns how many blueprint VM definitions
// reference a template. blueprint_vms.template_id is a NOT NULL / NO ACTION
// FK, so a blueprint reference is a genuine active dependency that would make
// the template delete fail with SQLSTATE 23503; the delete handler refuses
// (409) when this is > 0 rather than 500 or silently break the blueprint.
func (q *Queries) CountTemplateBlueprintRefs(ctx context.Context, id uuid.UUID) (int, error) {
	var n int
	err := q.pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM blueprint_vms WHERE template_id = $1", id).Scan(&n)
	return n, err
}

// UpdateTemplateLifecycleState transitions a template's template_state column.
// Used exclusively by the T4 wizard worker jobs and the lifecycle-aware
// API handlers. The caller MUST have validated the transition against
// internal/templates.CanTransition first — this query simply persists
// the new value, gated on `from` matching the current row so concurrent
// workers don't overwrite each other.
//
// Returns ErrTemplateStale when the row exists but its current
// template_state is no longer `from` (a different worker advanced it).
// Returns pgx.ErrNoRows when the template was deleted underneath us.
func (q *Queries) UpdateTemplateLifecycleState(ctx context.Context, id uuid.UUID, from, to string) error {
	tag, err := q.pool.Exec(ctx, `
		UPDATE templates
		SET template_state = $3, updated_at = NOW()
		WHERE id = $1 AND template_state = $2
	`, id, from, to)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "row gone" from "state moved underneath us".
		var exists bool
		if e := q.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM templates WHERE id = $1)`, id,
		).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			return pgx.ErrNoRows
		}
		return ErrTemplateStale
	}
	return nil
}

// SetTemplateVCenterVM stores the freshly-cloned VM's moref against the
// template row. Called by template_provision after a successful clone so
// subsequent jobs (generalize, snapshot) know which VM to act on. Safe
// to call multiple times — overwrites whatever was previously set.
func (q *Queries) SetTemplateVCenterVM(ctx context.Context, id uuid.UUID, vcenterVMID string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE templates
		SET vcenter_vm_id = $2, updated_at = NOW()
		WHERE id = $1
	`, id, vcenterVMID)
	return err
}

// SetTemplateActive flips the templates.is_active flag. Called by the
// template_verify worker on smoke-test success (active=true) so a template
// only becomes visible to students AFTER an L3 clone was proven to boot.
// The API's publish handler no longer flips is_active directly — the
// verify gate owns that transition.
func (q *Queries) SetTemplateActive(ctx context.Context, id uuid.UUID, active bool) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE templates
		SET is_active = $2, updated_at = NOW()
		WHERE id = $1
	`, id, active)
	return err
}

// ListAllJobs returns all jobs ordered by creation time (admin).
func (q *Queries) ListAllJobs(ctx context.Context) ([]models.Job, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, type, payload, status, claimed_by, claimed_at, started_at,
		       completed_at, result, retry_count, max_retries, rollback_steps, created_at
		FROM jobs ORDER BY created_at DESC LIMIT 100
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []models.Job
	for rows.Next() {
		var j models.Job
		if err := rows.Scan(
			&j.ID, &j.Type, &j.Payload, &j.Status, &j.ClaimedBy, &j.ClaimedAt, &j.StartedAt,
			&j.CompletedAt, &j.Result, &j.RetryCount, &j.MaxRetries, &j.RollbackSteps, &j.CreatedAt,
		); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// ListAuditLog returns recent audit log entries (admin).
func (q *Queries) ListAuditLog(ctx context.Context) ([]models.AuditLog, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT a.id, a.user_id, u.display_name as user_display_name, u.email as user_email,
		       a.action, a.resource_type, a.resource_id, a.details,
		       CAST(a.ip_address AS TEXT), a.created_at
		FROM audit_log a
		LEFT JOIN users u ON a.user_id = u.id
		ORDER BY a.created_at DESC LIMIT 200
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []models.AuditLog
	for rows.Next() {
		var e models.AuditLog
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.UserDisplayName, &e.UserEmail,
			&e.Action, &e.ResourceType, &e.ResourceID, &e.Details, &e.IPAddress, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// --- Pods ---

// GetResourceUsage returns a user's current resource consumption.
func (q *Queries) GetResourceUsage(ctx context.Context, userID uuid.UUID) (*models.ResourceUsage, error) {
	var usage models.ResourceUsage

	err := q.pool.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(pv.vcpus), 0),
			COALESCE(SUM(pv.ram_mb), 0),
			COALESCE(SUM(pv.disk_gb), 0),
			COUNT(DISTINCT p.id)
		FROM pods p
		LEFT JOIN pod_vms pv ON p.id = pv.pod_id AND pv.status NOT IN ('deleted', 'error')
		WHERE p.owner_id = $1 AND p.status NOT IN ('destroyed', 'error')
	`, userID).Scan(&usage.UsedVCPUs, &usage.UsedRAMMB, &usage.UsedStorageGB, &usage.ActivePods)

	return &usage, err
}

// CheckoutVLAN atomically reserves a random available VLAN from the pool.
// hostScope filters VLANs by host compatibility: "all" for any host, "switch1" for esxi1/esxi2 only.
// Pass empty string to accept any VLAN regardless of scope.
func (q *Queries) CheckoutVLAN(ctx context.Context, tx pgx.Tx, podID uuid.UUID, hostScope string) (int, string, error) {
	var vlanTag int
	var subnet string

	query := `
		UPDATE vlan_pool SET pod_id = $1, allocated_at = now()
		WHERE id = (
			SELECT id FROM vlan_pool
			WHERE pod_id IS NULL
	`
	args := []any{podID}

	// Prefer 'all' scope VLANs first (work on every host), fall back to any available
	if hostScope == "all" {
		query += ` AND host_scope = 'all'`
	} else if hostScope != "" {
		query += fmt.Sprintf(` AND host_scope = '%s'`, hostScope)
	}

	query += `
			ORDER BY RANDOM()
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING vlan_tag, subnet
	`

	err := tx.QueryRow(ctx, query, args...).Scan(&vlanTag, &subnet)
	if err == pgx.ErrNoRows {
		return 0, "", fmt.Errorf("no available VLANs in pool")
	}
	return vlanTag, subnet, err
}

// ReleaseVLAN returns a pod's VLAN back to the available pool.
func (q *Queries) ReleaseVLAN(ctx context.Context, podID uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE vlan_pool SET pod_id = NULL, allocated_at = NULL WHERE pod_id = $1
	`, podID)
	return err
}

// CreatePod inserts a pod record with a checked-out VLAN.
func (q *Queries) CreatePod(ctx context.Context, tx pgx.Tx, pod *models.Pod) error {
	return tx.QueryRow(ctx, `
		INSERT INTO pods (owner_id, name, salt, vlan_id, subnet, status, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at, updated_at
	`, pod.OwnerID, pod.Name, pod.Salt, pod.VLANID, pod.Subnet, pod.Status, pod.ExpiresAt,
	).Scan(&pod.ID, &pod.CreatedAt, &pod.UpdatedAt)
}

// GetPodByID retrieves a pod with its VMs and owner.
func (q *Queries) GetPodByID(ctx context.Context, id uuid.UUID) (*models.Pod, error) {
	var p models.Pod
	err := q.pool.QueryRow(ctx, `
		SELECT id, owner_id, name, salt, vlan_id, subnet, status,
		       error_message, expires_at, blueprint_id, allow_vm_additions, created_at, updated_at
		FROM pods WHERE id = $1
	`, id).Scan(
		&p.ID, &p.OwnerID, &p.Name, &p.Salt, &p.VLANID, &p.Subnet, &p.Status,
		&p.ErrorMessage, &p.ExpiresAt, &p.BlueprintID, &p.AllowVMAdditions, &p.CreatedAt, &p.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Load owner
	owner, err := q.GetUserByID(ctx, p.OwnerID)
	if err == nil && owner != nil {
		p.Owner = owner
	}

	// Load VMs
	rows, err := q.pool.Query(ctx, `
		SELECT pv.id, pv.pod_id, pv.template_id, pv.display_name, pv.vcenter_vm_name, pv.vcenter_vm_id,
		       pv.vcpus, pv.ram_mb, pv.disk_gb, pv.ip_address, pv.status,
		       COALESCE(t.default_username, ''), COALESCE(t.default_password, ''),
		       pv.generated_username, pv.generated_password, pv.boot_order, pv.created_at,
		       COALESCE(t.name, ''), COALESCE(t.os_type, '')
		FROM pod_vms pv
		LEFT JOIN templates t ON pv.template_id = t.id
		WHERE pv.pod_id = $1 AND pv.status != 'deleted' ORDER BY pv.boot_order, pv.created_at
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var vm models.PodVM
		if err := rows.Scan(
			&vm.ID, &vm.PodID, &vm.TemplateID, &vm.DisplayName, &vm.VCenterVMName, &vm.VCenterVMID,
			&vm.VCPUs, &vm.RAMMB, &vm.DiskGB, &vm.IPAddress, &vm.Status,
			&vm.DefaultUsername, &vm.DefaultPassword,
			&vm.GeneratedUsername, &vm.GeneratedPassword, &vm.BootOrder, &vm.CreatedAt,
			&vm.TemplateName, &vm.OSType,
		); err != nil {
			return nil, err
		}
		p.VMs = append(p.VMs, vm)
	}

	return &p, nil
}

// ListPodsByOwner returns all non-destroyed pods for a user (with VMs and owner).
func (q *Queries) ListPodsByOwner(ctx context.Context, ownerID uuid.UUID) ([]models.Pod, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT p.id, p.owner_id, p.name, p.salt, p.vlan_id, p.subnet, p.status,
		       p.error_message, p.expires_at, p.blueprint_id, p.allow_vm_additions, p.created_at, p.updated_at,
		       u.id, u.username, u.email, COALESCE(u.display_name, ''), u.role
		FROM pods p
		JOIN users u ON u.id = p.owner_id
		WHERE p.owner_id = $1 AND p.status != 'destroyed'
		ORDER BY p.created_at DESC
	`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pods []models.Pod
	for rows.Next() {
		var p models.Pod
		var owner models.User
		if err := rows.Scan(
			&p.ID, &p.OwnerID, &p.Name, &p.Salt, &p.VLANID, &p.Subnet, &p.Status,
			&p.ErrorMessage, &p.ExpiresAt, &p.BlueprintID, &p.AllowVMAdditions, &p.CreatedAt, &p.UpdatedAt,
			&owner.ID, &owner.Username, &owner.Email, &owner.DisplayName, &owner.Role,
		); err != nil {
			return nil, err
		}
		p.Owner = &owner
		pods = append(pods, p)
	}

	// Load VMs for each pod
	for i := range pods {
		vms, err := q.listPodVMsActive(ctx, pods[i].ID)
		if err != nil {
			return nil, err
		}
		pods[i].VMs = vms
	}

	return pods, nil
}

// ListAllPods returns all non-destroyed pods (admin, with owner info).
func (q *Queries) ListAllPods(ctx context.Context) ([]models.Pod, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT p.id, p.owner_id, p.name, p.salt, p.vlan_id, p.subnet, p.status,
		       p.error_message, p.expires_at, p.blueprint_id, p.allow_vm_additions, p.created_at, p.updated_at,
		       u.id, u.username, u.email, COALESCE(u.display_name, ''), u.role
		FROM pods p
		JOIN users u ON u.id = p.owner_id
		WHERE p.status != 'destroyed'
		ORDER BY p.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pods []models.Pod
	for rows.Next() {
		var p models.Pod
		var owner models.User
		if err := rows.Scan(
			&p.ID, &p.OwnerID, &p.Name, &p.Salt, &p.VLANID, &p.Subnet, &p.Status,
			&p.ErrorMessage, &p.ExpiresAt, &p.BlueprintID, &p.AllowVMAdditions, &p.CreatedAt, &p.UpdatedAt,
			&owner.ID, &owner.Username, &owner.Email, &owner.DisplayName, &owner.Role,
		); err != nil {
			return nil, err
		}
		p.Owner = &owner
		pods = append(pods, p)
	}

	for i := range pods {
		vms, err := q.listPodVMsActive(ctx, pods[i].ID)
		if err != nil {
			return nil, err
		}
		pods[i].VMs = vms
	}

	return pods, nil
}

// listPodVMsActive returns non-deleted VMs for a pod.
func (q *Queries) listPodVMsActive(ctx context.Context, podID uuid.UUID) ([]models.PodVM, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT pv.id, pv.pod_id, pv.template_id, pv.display_name, pv.vcenter_vm_name, pv.vcenter_vm_id,
		       pv.vcpus, pv.ram_mb, pv.disk_gb, pv.ip_address, pv.status,
		       COALESCE(t.default_username, ''), COALESCE(t.default_password, ''),
		       pv.generated_username, pv.generated_password, pv.boot_order, pv.created_at,
		       COALESCE(t.name, ''), COALESCE(t.os_type, '')
		FROM pod_vms pv
		LEFT JOIN templates t ON pv.template_id = t.id
		WHERE pv.pod_id = $1 AND pv.status != 'deleted' ORDER BY pv.boot_order, pv.created_at
	`, podID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vms []models.PodVM
	for rows.Next() {
		var vm models.PodVM
		if err := rows.Scan(
			&vm.ID, &vm.PodID, &vm.TemplateID, &vm.DisplayName, &vm.VCenterVMName, &vm.VCenterVMID,
			&vm.VCPUs, &vm.RAMMB, &vm.DiskGB, &vm.IPAddress, &vm.Status,
			&vm.DefaultUsername, &vm.DefaultPassword,
			&vm.GeneratedUsername, &vm.GeneratedPassword, &vm.BootOrder, &vm.CreatedAt,
			&vm.TemplateName, &vm.OSType,
		); err != nil {
			return nil, err
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

// --- Jobs ---

// CreateJob inserts a new job and returns it.
func (q *Queries) CreateJob(ctx context.Context, jobType string, payload []byte) (*models.Job, error) {
	var j models.Job
	err := q.pool.QueryRow(ctx, `
		INSERT INTO jobs (type, payload) VALUES ($1, $2)
		RETURNING id, type, payload, status, retry_count, max_retries, rollback_steps, created_at
	`, jobType, payload).Scan(
		&j.ID, &j.Type, &j.Payload, &j.Status, &j.RetryCount, &j.MaxRetries, &j.RollbackSteps, &j.CreatedAt,
	)
	return &j, err
}

// ClaimJob atomically claims the next pending job for a worker.
// Jobs whose next_attempt_at is in the future are skipped (they are
// sleeping between retry attempts).
func (q *Queries) ClaimJob(ctx context.Context, workerID string) (*models.Job, error) {
	var j models.Job
	err := q.pool.QueryRow(ctx, `
		UPDATE jobs SET
			status = 'claimed',
			claimed_by = $1,
			claimed_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE status = 'pending'
			  AND (next_attempt_at IS NULL OR next_attempt_at <= now())
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, type, payload, status, claimed_by, claimed_at,
		          retry_count, max_retries, next_attempt_at, rollback_steps, created_at
	`, workerID).Scan(
		&j.ID, &j.Type, &j.Payload, &j.Status, &j.ClaimedBy, &j.ClaimedAt,
		&j.RetryCount, &j.MaxRetries, &j.NextAttemptAt, &j.RollbackSteps, &j.CreatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &j, err
}

// UpdateJobStatus updates a job's status and optional result.
func (q *Queries) UpdateJobStatus(ctx context.Context, id uuid.UUID, status string, result []byte) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET status = $2, result = $3,
			started_at = CASE WHEN $2 = 'in_progress' AND started_at IS NULL THEN now() ELSE started_at END,
			completed_at = CASE WHEN $2 IN ('completed', 'failed', 'rollback') THEN now() ELSE completed_at END
		WHERE id = $1
	`, id, status, result)
	return err
}

// UpdateJobRollbackSteps updates the rollback steps for a job.
func (q *Queries) UpdateJobRollbackSteps(ctx context.Context, id uuid.UUID, steps []byte) error {
	_, err := q.pool.Exec(ctx, `UPDATE jobs SET rollback_steps = $2 WHERE id = $1`, id, steps)
	return err
}

// RetryJob resets a failed job back to pending and schedules it for a
// future attempt.  retry_count is incremented; claimed_by, claimed_at,
// started_at, and completed_at are cleared; next_attempt_at is set to
// nextAt so ClaimJob ignores the row until the delay expires.
//
// Called by the worker when ProcessJob returns a retryable error and
// retry_count < max_retries.
func (q *Queries) RetryJob(ctx context.Context, id uuid.UUID, nextAt time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE jobs SET
			status          = 'pending',
			retry_count     = retry_count + 1,
			claimed_by      = NULL,
			claimed_at      = NULL,
			started_at      = NULL,
			completed_at    = NULL,
			next_attempt_at = $2
		WHERE id = $1
	`, id, nextAt)
	return err
}

// CountRetryPendingJobs returns the number of jobs currently sleeping
// between retry attempts (pending with a future next_attempt_at). Used
// to feed the crucible_job_retry_pending gauge.
func (q *Queries) CountRetryPendingJobs(ctx context.Context) (int, error) {
	var n int
	err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM jobs
		WHERE status = 'pending' AND next_attempt_at > now()
	`).Scan(&n)
	return n, err
}

// RecoverStaleJobs resets in_progress/claimed jobs that were abandoned (e.g., worker restart).
func (q *Queries) RecoverStaleJobs(ctx context.Context) (int64, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE jobs SET status = 'pending', claimed_by = NULL, claimed_at = NULL, started_at = NULL
		WHERE status IN ('in_progress', 'claimed') AND completed_at IS NULL
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// GetJob retrieves a job by ID.
func (q *Queries) GetJob(ctx context.Context, id uuid.UUID) (*models.Job, error) {
	var j models.Job
	err := q.pool.QueryRow(ctx, `
		SELECT id, type, payload, status, claimed_by, claimed_at, started_at,
		       completed_at, result, retry_count, max_retries, rollback_steps, created_at
		FROM jobs WHERE id = $1
	`, id).Scan(
		&j.ID, &j.Type, &j.Payload, &j.Status, &j.ClaimedBy, &j.ClaimedAt, &j.StartedAt,
		&j.CompletedAt, &j.Result, &j.RetryCount, &j.MaxRetries, &j.RollbackSteps, &j.CreatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &j, err
}

// ListJobsByUser returns recent jobs for a specific user.
func (q *Queries) ListJobsByUser(ctx context.Context, userID uuid.UUID) ([]models.Job, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, type, payload, status, claimed_by, claimed_at, started_at,
		       completed_at, result, retry_count, max_retries, rollback_steps, created_at
		FROM jobs WHERE payload->>'user_id' = $1
		ORDER BY created_at DESC LIMIT 20
	`, userID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []models.Job
	for rows.Next() {
		var j models.Job
		if err := rows.Scan(
			&j.ID, &j.Type, &j.Payload, &j.Status, &j.ClaimedBy, &j.ClaimedAt, &j.StartedAt,
			&j.CompletedAt, &j.Result, &j.RetryCount, &j.MaxRetries, &j.RollbackSteps, &j.CreatedAt,
		); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// GetLatestJobForTemplate returns the most recent job whose payload's
// template_id matches the given UUID, regardless of status. Used by the
// wizard state endpoint to surface which step (template_provision vs
// template_generalize) most recently ran, so the UI can mark the right
// step as the failed one when state=error.
//
// Returns (nil, nil) when no job has ever targeted this template.
func (q *Queries) GetLatestJobForTemplate(ctx context.Context, templateID uuid.UUID) (*models.Job, error) {
	var j models.Job
	err := q.pool.QueryRow(ctx, `
		SELECT id, type, payload, status, claimed_by, claimed_at, started_at,
		       completed_at, result, retry_count, max_retries, rollback_steps, created_at
		FROM jobs
		WHERE payload->>'template_id' = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, templateID.String()).Scan(
		&j.ID, &j.Type, &j.Payload, &j.Status, &j.ClaimedBy, &j.ClaimedAt, &j.StartedAt,
		&j.CompletedAt, &j.Result, &j.RetryCount, &j.MaxRetries, &j.RollbackSteps, &j.CreatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &j, err
}

// InsertAuditLog records an action in the audit log.
func (q *Queries) InsertAuditLog(ctx context.Context, entry models.AuditLog) error {
	_, err := q.pool.Exec(ctx, `
		INSERT INTO audit_log (user_id, action, resource_type, resource_id, details, ip_address)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, entry.UserID, entry.Action, entry.ResourceType, entry.ResourceID, entry.Details, entry.IPAddress)
	return err
}

// --- VLAN Pool ---

// ListVLANPool returns all VLAN pool entries.
func (q *Queries) ListVLANPool(ctx context.Context) ([]models.VLANPoolEntry, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, vlan_tag, subnet, host_scope, pod_id, allocated_at
		FROM vlan_pool ORDER BY vlan_tag
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []models.VLANPoolEntry
	for rows.Next() {
		var e models.VLANPoolEntry
		if err := rows.Scan(&e.ID, &e.VLANTag, &e.Subnet, &e.HostScope, &e.PodID, &e.AllocatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// AddVLAN inserts a new VLAN into the pool.
func (q *Queries) AddVLAN(ctx context.Context, req models.AddVLANRequest) (*models.VLANPoolEntry, error) {
	var e models.VLANPoolEntry
	err := q.pool.QueryRow(ctx, `
		INSERT INTO vlan_pool (vlan_tag, subnet, host_scope)
		VALUES ($1, $2, $3)
		RETURNING id, vlan_tag, subnet, host_scope, pod_id, allocated_at
	`, req.VLANTag, req.Subnet, req.HostScope).Scan(
		&e.ID, &e.VLANTag, &e.Subnet, &e.HostScope, &e.PodID, &e.AllocatedAt,
	)
	return &e, err
}

// UpdateVLAN updates a VLAN pool entry's scope.
func (q *Queries) UpdateVLAN(ctx context.Context, id int, req models.UpdateVLANRequest) (*models.VLANPoolEntry, error) {
	var e models.VLANPoolEntry
	err := q.pool.QueryRow(ctx, `
		UPDATE vlan_pool SET host_scope = COALESCE($2, host_scope)
		WHERE id = $1
		RETURNING id, vlan_tag, subnet, host_scope, pod_id, allocated_at
	`, id, req.HostScope).Scan(
		&e.ID, &e.VLANTag, &e.Subnet, &e.HostScope, &e.PodID, &e.AllocatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &e, err
}

// RemoveVLAN deletes a VLAN from the pool (only if not allocated).
func (q *Queries) RemoveVLAN(ctx context.Context, id int) error {
	result, err := q.pool.Exec(ctx, `
		DELETE FROM vlan_pool WHERE id = $1 AND pod_id IS NULL
	`, id)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return fmt.Errorf("VLAN not found or currently allocated to a pod")
	}
	return nil
}

// --- Pod Status ---

// UpdatePodStatus updates a pod's status and optional error message.
func (q *Queries) UpdatePodStatus(ctx context.Context, id uuid.UUID, status, errMsg string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE pods SET status = $1, error_message = $2, updated_at = now() WHERE id = $3
	`, status, errMsg, id)
	return err
}

// UpdatePodStatusFrom transitions a pod to status only if it is currently in one of
// fromStatuses, reporting whether the transition actually applied.
//
// This is a compare-and-swap, and it exists because pod create and pod destroy are
// independent worker jobs that can overlap: vCenter can take minutes to report a VM's
// IP, and a destroy issued in the meantime completes against the same pod. With an
// unconditional UPDATE the slow create won simply by finishing last, resurrecting a
// pod that destroy had already torn down into an "active" row with no VM behind it.
// Such a ghost pod is invisible -- the API and UI report it healthy, it counts against
// the owner's quota forever, and no reconciler removes it, because every component
// trusts the status column.
//
// Only forward transitions guard themselves this way. Destroy deliberately writes
// unconditionally: it is the terminal intent and must always win.
func (q *Queries) UpdatePodStatusFrom(ctx context.Context, id uuid.UUID, fromStatuses []string, status, errMsg string) (bool, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE pods SET status = $1, error_message = $2, updated_at = now()
		WHERE id = $3 AND status = ANY($4)
	`, status, errMsg, id, fromStatuses)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ListDestroyFailedPods returns pods stuck in "destroy_failed" status.
func (q *Queries) ListDestroyFailedPods(ctx context.Context) ([]models.Pod, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, owner_id, name, status, error_message, expires_at, created_at, updated_at, salt, vlan_id, subnet
		FROM pods WHERE status = 'destroy_failed'
		ORDER BY updated_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pods []models.Pod
	for rows.Next() {
		var p models.Pod
		if err := rows.Scan(&p.ID, &p.OwnerID, &p.Name, &p.Status, &p.ErrorMessage,
			&p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt, &p.Salt, &p.VLANID, &p.Subnet); err != nil {
			return nil, err
		}
		pods = append(pods, p)
	}
	return pods, rows.Err()
}

// --- Pod VMs ---

// ListPodVMs returns all VMs belonging to a pod.
func (q *Queries) ListPodVMs(ctx context.Context, podID uuid.UUID) ([]models.PodVM, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT pv.id, pv.pod_id, pv.template_id, pv.display_name, pv.vcenter_vm_name, pv.vcenter_vm_id,
		       pv.vcpus, pv.ram_mb, pv.disk_gb, pv.ip_address, pv.status,
		       COALESCE(t.default_username, ''), COALESCE(t.default_password, ''),
		       pv.generated_username, pv.generated_password, pv.created_at,
		       COALESCE(t.name, ''), COALESCE(t.os_type, '')
		FROM pod_vms pv
		LEFT JOIN templates t ON pv.template_id = t.id
		WHERE pv.pod_id = $1
	`, podID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vms []models.PodVM
	for rows.Next() {
		var vm models.PodVM
		err := rows.Scan(&vm.ID, &vm.PodID, &vm.TemplateID, &vm.DisplayName, &vm.VCenterVMName, &vm.VCenterVMID,
			&vm.VCPUs, &vm.RAMMB, &vm.DiskGB, &vm.IPAddress, &vm.Status,
			&vm.DefaultUsername, &vm.DefaultPassword,
			&vm.GeneratedUsername, &vm.GeneratedPassword, &vm.CreatedAt,
			&vm.TemplateName, &vm.OSType)
		if err != nil {
			return nil, err
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

// GetPodVM returns a single pod VM by ID.
func (q *Queries) GetPodVM(ctx context.Context, id uuid.UUID) (*models.PodVM, error) {
	var vm models.PodVM
	err := q.pool.QueryRow(ctx, `
		SELECT pv.id, pv.pod_id, pv.template_id, pv.display_name, pv.vcenter_vm_name, pv.vcenter_vm_id,
		       pv.vcpus, pv.ram_mb, pv.disk_gb, pv.ip_address, pv.status,
		       COALESCE(t.default_username, ''), COALESCE(t.default_password, ''),
		       pv.generated_username, pv.generated_password, pv.created_at,
		       COALESCE(t.name, ''), COALESCE(t.os_type, '')
		FROM pod_vms pv
		LEFT JOIN templates t ON pv.template_id = t.id
		WHERE pv.id = $1
	`, id).Scan(&vm.ID, &vm.PodID, &vm.TemplateID, &vm.DisplayName, &vm.VCenterVMName, &vm.VCenterVMID,
		&vm.VCPUs, &vm.RAMMB, &vm.DiskGB, &vm.IPAddress, &vm.Status,
		&vm.DefaultUsername, &vm.DefaultPassword,
		&vm.GeneratedUsername, &vm.GeneratedPassword, &vm.CreatedAt,
		&vm.TemplateName, &vm.OSType)
	if err != nil {
		return nil, err
	}
	return &vm, nil
}

// UpdatePodVM updates a pod VM's vCenter details after cloning.
func (q *Queries) UpdatePodVM(ctx context.Context, id uuid.UUID, vcenterVMID, vcenterVMName, status string) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE pod_vms SET vcenter_vm_id = $1, vcenter_vm_name = $2, status = $3 WHERE id = $4
	`, vcenterVMID, vcenterVMName, status, id)
	return err
}

// UpdatePodVMStatus updates a pod VM's status.
func (q *Queries) UpdatePodVMStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := q.pool.Exec(ctx, `UPDATE pod_vms SET status = $1 WHERE id = $2`, status, id)
	return err
}

// UpdatePodVMIP sets the IP address on a pod VM.
func (q *Queries) UpdatePodVMIP(ctx context.Context, id uuid.UUID, ip string) error {
	_, err := q.pool.Exec(ctx, `UPDATE pod_vms SET ip_address = $1 WHERE id = $2`, ip, id)
	return err
}

// UpdatePodVMCredentials stores the generated credentials for a pod VM.
func (q *Queries) UpdatePodVMCredentials(ctx context.Context, id uuid.UUID, username, password string) error {
	_, err := q.pool.Exec(ctx,
		`UPDATE pod_vms SET generated_username = $2, generated_password = $3 WHERE id = $1`,
		id, username, password)
	return err
}

// GetTemplateByID returns a template by its ID.
func (q *Queries) GetTemplateByID(ctx context.Context, id uuid.UUID) (*models.Template, error) {
	var t models.Template
	err := scanTemplate(q.pool.QueryRow(ctx, `
		SELECT `+templateSelectCols+`
		FROM templates WHERE id = $1
	`, id), &t)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// CreatePodVM inserts a new VM record into an existing pod.
func (q *Queries) CreatePodVM(ctx context.Context, vm *models.PodVM) error {
	return q.pool.QueryRow(ctx, `
		INSERT INTO pod_vms (pod_id, template_id, display_name, vcpus, ram_mb, disk_gb, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at
	`, vm.PodID, vm.TemplateID, vm.DisplayName, vm.VCPUs, vm.RAMMB, vm.DiskGB, vm.Status,
	).Scan(&vm.ID, &vm.CreatedAt)
}

// CountActiveVMsInPod returns the number of non-deleted VMs in a pod.
func (q *Queries) CountActiveVMsInPod(ctx context.Context, podID uuid.UUID) (int, error) {
	var count int
	err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pod_vms WHERE pod_id = $1 AND status != 'deleted'
	`, podID).Scan(&count)
	return count, err
}

// ListTemplateDependents returns all active VMs cloned from a given template.
func (q *Queries) ListTemplateDependents(ctx context.Context, templateID uuid.UUID) (string, []models.TemplateDependentVM, error) {
	// Get template name
	var templateName string
	err := q.pool.QueryRow(ctx, `SELECT name FROM templates WHERE id = $1`, templateID).Scan(&templateName)
	if err != nil {
		return "", nil, fmt.Errorf("template not found: %w", err)
	}

	rows, err := q.pool.Query(ctx, `
		SELECT pv.vcenter_vm_id, pv.vcenter_vm_name, p.name, p.id, u.display_name, pv.status
		FROM pod_vms pv
		JOIN pods p ON pv.pod_id = p.id
		JOIN users u ON p.owner_id = u.id
		WHERE pv.template_id = $1
		  AND pv.status NOT IN ('deleted', 'error')
		ORDER BY pv.created_at DESC
	`, templateID)
	if err != nil {
		return templateName, nil, err
	}
	defer rows.Close()

	var vms []models.TemplateDependentVM
	for rows.Next() {
		var vm models.TemplateDependentVM
		var vmID, vmName *string
		if err := rows.Scan(&vmID, &vmName, &vm.PodName, &vm.PodID, &vm.OwnerName, &vm.Status); err != nil {
			return templateName, nil, err
		}
		if vmID != nil {
			vm.VMID = *vmID
		}
		if vmName != nil {
			vm.VMName = *vmName
		}
		vms = append(vms, vm)
	}
	return templateName, vms, rows.Err()
}

// SetTemplateValidationState records the outcome of a template_revalidate job.
// Both last_validated_at and last_validation_result are updated together so
// queries can efficiently find templates whose most recent run was a failure.
func (q *Queries) SetTemplateValidationState(ctx context.Context, id uuid.UUID, result string, validatedAt time.Time) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE templates
		SET last_validated_at = $2, last_validation_result = $3, updated_at = NOW()
		WHERE id = $1
	`, id, validatedAt, result)
	return err
}

// ListStaleL1Templates returns active templates with trust_tier='l1' whose
// last_validated_at is NULL or older than olderThan. These are candidates for
// a new template_revalidate job enqueued by the L1 trust-validation reconciler.
func (q *Queries) ListStaleL1Templates(ctx context.Context, olderThan time.Duration) ([]models.Template, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT `+templateSelectCols+`
		FROM templates
		WHERE trust_tier = 'l1'
		  AND is_active = true
		  AND (
		      last_validated_at IS NULL
		      OR last_validated_at < now() - $1::interval
		  )
		ORDER BY COALESCE(last_validated_at, '-infinity'::timestamptz) ASC
	`, fmt.Sprintf("%d seconds", int(olderThan.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Template
	for rows.Next() {
		var t models.Template
		if err := scanTemplate(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAllActiveL1Templates returns all active templates with trust_tier='l1'.
// Used by the L1 trust-validation reconciler to populate the per-template
// staleness gauge on every pass — not just for templates that are overdue.
func (q *Queries) ListAllActiveL1Templates(ctx context.Context) ([]models.Template, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT `+templateSelectCols+`
		FROM templates
		WHERE trust_tier = 'l1' AND is_active = true
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []models.Template
	for rows.Next() {
		var t models.Template
		if err := scanTemplate(rows, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- User Sessions ---

// CreateSession inserts a new active session row. idToken is the raw OIDC
// id_token persisted so it can later be replayed to Authentik's
// end_session_endpoint as the id_token_hint on logout; pass "" when unknown.
func (q *Queries) CreateSession(ctx context.Context, id uuid.UUID, userID uuid.UUID, ip, userAgent, idToken string) error {
	var tokenArg any
	if idToken != "" {
		tokenArg = idToken
	}
	_, err := q.pool.Exec(ctx, `
		INSERT INTO user_sessions (id, user_id, ip_address, user_agent, id_token)
		VALUES ($1, $2, $3, $4, $5)
	`, id, userID, ip, userAgent, tokenArg)
	return err
}

// DeactivateSession marks a session as inactive.
func (q *Queries) DeactivateSession(ctx context.Context, sessionID uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE user_sessions SET is_active = false WHERE id = $1
	`, sessionID)
	return err
}

// IsSessionActive reports whether the given session row exists and is still
// active. Used by the auth middleware to reject a revoked/logged-out session
// that still carries an unexpired JWT.
func (q *Queries) IsSessionActive(ctx context.Context, sessionID uuid.UUID) (bool, error) {
	var active bool
	err := q.pool.QueryRow(ctx, `
		SELECT is_active FROM user_sessions WHERE id = $1
	`, sessionID).Scan(&active)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return active, nil
}

// GetSessionIDToken returns the raw OIDC id_token stored for a session, or an
// empty string if none was persisted. Used on logout to build the id_token_hint.
func (q *Queries) GetSessionIDToken(ctx context.Context, sessionID uuid.UUID) (string, error) {
	var token *string
	err := q.pool.QueryRow(ctx, `
		SELECT id_token FROM user_sessions WHERE id = $1
	`, sessionID).Scan(&token)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	if token == nil {
		return "", nil
	}
	return *token, nil
}

// TouchSession updates last_activity for a session.
func (q *Queries) TouchSession(ctx context.Context, sessionID uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `
		UPDATE user_sessions SET last_activity = now() WHERE id = $1 AND is_active = true
	`, sessionID)
	return err
}

// DeactivateStaleSessions marks sessions with no activity for the given duration as inactive.
func (q *Queries) DeactivateStaleSessions(ctx context.Context, staleMinutes int) (int64, error) {
	tag, err := q.pool.Exec(ctx, `
		UPDATE user_sessions SET is_active = false
		WHERE is_active = true AND last_activity < now() - make_interval(mins := $1)
	`, staleMinutes)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ActiveSession represents a session joined with user info for admin display.
type ActiveSession struct {
	ID           uuid.UUID `json:"id"`
	UserID       uuid.UUID `json:"user_id"`
	Username     string    `json:"username"`
	DisplayName  *string   `json:"display_name,omitempty"`
	Email        *string   `json:"email,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LastActivity time.Time `json:"last_activity"`
	IPAddress    *string   `json:"ip_address,omitempty"`
	UserAgent    *string   `json:"user_agent,omitempty"`
}

// ListActiveSessions returns all active sessions with user info.
func (q *Queries) ListActiveSessions(ctx context.Context) ([]ActiveSession, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT s.id, s.user_id, u.username, u.display_name, u.email,
		       s.created_at, s.last_activity, CAST(s.ip_address AS TEXT), s.user_agent
		FROM user_sessions s
		JOIN users u ON s.user_id = u.id
		WHERE s.is_active = true
		ORDER BY s.last_activity DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []ActiveSession
	for rows.Next() {
		var s ActiveSession
		if err := rows.Scan(&s.ID, &s.UserID, &s.Username, &s.DisplayName, &s.Email,
			&s.CreatedAt, &s.LastActivity, &s.IPAddress, &s.UserAgent); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// --- Paginated Audit Log ---

// AuditLogFilter holds optional query filters for listing audit entries.
type AuditLogFilter struct {
	Page         int
	PerPage      int
	Action       string // prefix match (e.g. "pod" matches "pod.create", "pod.delete")
	UserID       *uuid.UUID
	ResourceType string
	ResourceID   *uuid.UUID
	Since        *time.Time
	Until        *time.Time
}

// AuditLogPage holds a page of audit entries plus total count.
type AuditLogPage struct {
	Entries []models.AuditLog `json:"entries"`
	Total   int64             `json:"total"`
	Page    int               `json:"page"`
	PerPage int               `json:"per_page"`
}

// ListAuditLogPaginated returns a filtered, paginated audit log.
func (q *Queries) ListAuditLogPaginated(ctx context.Context, f AuditLogFilter) (*AuditLogPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PerPage < 1 || f.PerPage > 100 {
		f.PerPage = 50
	}

	// Build WHERE clauses dynamically
	where := "WHERE 1=1"
	args := []any{}
	argIdx := 1

	if f.Action != "" {
		where += fmt.Sprintf(" AND a.action LIKE $%d", argIdx)
		args = append(args, f.Action+"%")
		argIdx++
	}
	if f.UserID != nil {
		where += fmt.Sprintf(" AND a.user_id = $%d", argIdx)
		args = append(args, *f.UserID)
		argIdx++
	}
	if f.ResourceType != "" {
		where += fmt.Sprintf(" AND a.resource_type = $%d", argIdx)
		args = append(args, f.ResourceType)
		argIdx++
	}
	if f.ResourceID != nil {
		where += fmt.Sprintf(" AND a.resource_id = $%d", argIdx)
		args = append(args, *f.ResourceID)
		argIdx++
	}
	if f.Since != nil {
		where += fmt.Sprintf(" AND a.created_at >= $%d", argIdx)
		args = append(args, *f.Since)
		argIdx++
	}
	if f.Until != nil {
		where += fmt.Sprintf(" AND a.created_at <= $%d", argIdx)
		args = append(args, *f.Until)
		argIdx++
	}

	// Count total
	var total int64
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM audit_log a %s", where)
	if err := q.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, err
	}

	// Fetch page
	offset := (f.Page - 1) * f.PerPage
	dataQuery := fmt.Sprintf(`
		SELECT a.id, a.user_id, u.display_name, u.email,
		       a.action, a.resource_type, a.resource_id, a.details,
		       CAST(a.ip_address AS TEXT), a.created_at
		FROM audit_log a
		LEFT JOIN users u ON a.user_id = u.id
		%s
		ORDER BY a.created_at DESC
		LIMIT $%d OFFSET $%d
	`, where, argIdx, argIdx+1)
	args = append(args, f.PerPage, offset)

	rows, err := q.pool.Query(ctx, dataQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []models.AuditLog
	for rows.Next() {
		var e models.AuditLog
		if err := rows.Scan(
			&e.ID, &e.UserID, &e.UserDisplayName, &e.UserEmail,
			&e.Action, &e.ResourceType, &e.ResourceID, &e.Details, &e.IPAddress, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &AuditLogPage{
		Entries: entries,
		Total:   total,
		Page:    f.Page,
		PerPage: f.PerPage,
	}, nil
}

// --- VM Snapshots ---

// ListVMSnapshots returns all snapshots for a VM, ordered by creation time.
func (q *Queries) ListVMSnapshots(ctx context.Context, podVMID uuid.UUID) ([]models.VMSnapshot, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, pod_vm_id, name, description, vcenter_snapshot_id, is_initial, created_at
		FROM vm_snapshots
		WHERE pod_vm_id = $1
		ORDER BY created_at ASC`, podVMID)
	if err != nil {
		return nil, fmt.Errorf("list vm snapshots: %w", err)
	}
	defer rows.Close()

	var snapshots []models.VMSnapshot
	for rows.Next() {
		var s models.VMSnapshot
		if err := rows.Scan(&s.ID, &s.PodVMID, &s.Name, &s.Description,
			&s.VCenterSnapshotID, &s.IsInitial, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan vm snapshot: %w", err)
		}
		snapshots = append(snapshots, s)
	}
	return snapshots, rows.Err()
}

// GetVMSnapshot returns a single snapshot by ID, or nil if not found.
func (q *Queries) GetVMSnapshot(ctx context.Context, id uuid.UUID) (*models.VMSnapshot, error) {
	var s models.VMSnapshot
	err := q.pool.QueryRow(ctx, `
		SELECT id, pod_vm_id, name, description, vcenter_snapshot_id, is_initial, created_at
		FROM vm_snapshots
		WHERE id = $1`, id).Scan(&s.ID, &s.PodVMID, &s.Name, &s.Description,
		&s.VCenterSnapshotID, &s.IsInitial, &s.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get vm snapshot: %w", err)
	}
	return &s, nil
}

// CreateVMSnapshot inserts a snapshot and populates its ID and CreatedAt.
func (q *Queries) CreateVMSnapshot(ctx context.Context, snap *models.VMSnapshot) error {
	return q.pool.QueryRow(ctx, `
		INSERT INTO vm_snapshots (pod_vm_id, name, description, vcenter_snapshot_id, is_initial)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`,
		snap.PodVMID, snap.Name, snap.Description, snap.VCenterSnapshotID, snap.IsInitial,
	).Scan(&snap.ID, &snap.CreatedAt)
}

// DeleteVMSnapshot removes a snapshot by ID.
func (q *Queries) DeleteVMSnapshot(ctx context.Context, id uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `DELETE FROM vm_snapshots WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete vm snapshot: %w", err)
	}
	return nil
}

// CountUserSnapshots returns the number of non-initial snapshots for a VM.
func (q *Queries) CountUserSnapshots(ctx context.Context, podVMID uuid.UUID) (int, error) {
	var count int
	err := q.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM vm_snapshots
		WHERE pod_vm_id = $1 AND is_initial = false`, podVMID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count user snapshots: %w", err)
	}
	return count, nil
}

// --- Pod Expiration ---

// UpdatePodExpiry updates the expires_at timestamp for a pod.
func (q *Queries) UpdatePodExpiry(ctx context.Context, podID uuid.UUID, expiresAt time.Time) error {
	_, err := q.pool.Exec(ctx, `UPDATE pods SET expires_at = $1, updated_at = now() WHERE id = $2`, expiresAt, podID)
	return err
}

// CreatePodAttestation records a pod extension event.
func (q *Queries) CreatePodAttestation(ctx context.Context, a *models.PodAttestation) error {
	return q.pool.QueryRow(ctx, `
		INSERT INTO pod_attestations (pod_id, user_id, previous_expires_at, new_expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at
	`, a.PodID, a.UserID, a.PreviousExpiresAt, a.NewExpiresAt).Scan(&a.ID, &a.CreatedAt)
}

// ListExpiredPods returns active pods that have passed their expiration time.
func (q *Queries) ListExpiredPods(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id FROM pods
		WHERE status = 'active' AND expires_at IS NOT NULL AND expires_at < now()
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// --- Blueprints ---

// ListBlueprintsForUser returns blueprints accessible to a user based on their role.
func (q *Queries) ListBlueprintsForUser(ctx context.Context, userID uuid.UUID, role string) ([]models.Blueprint, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT DISTINCT b.id, b.name, b.description, b.created_by, b.allow_vm_additions,
		       b.is_active, b.created_at, b.updated_at
		FROM blueprints b
		LEFT JOIN blueprint_access ba ON b.id = ba.blueprint_id
		WHERE b.is_active = true
		  AND (ba.role = $1 OR ba.user_id = $2 OR $1 = 'admin'
		       OR NOT EXISTS (SELECT 1 FROM blueprint_access WHERE blueprint_id = b.id))
		ORDER BY b.name
	`, role, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blueprints []models.Blueprint
	for rows.Next() {
		var bp models.Blueprint
		if err := rows.Scan(
			&bp.ID, &bp.Name, &bp.Description, &bp.CreatedBy, &bp.AllowVMAdditions,
			&bp.IsActive, &bp.CreatedAt, &bp.UpdatedAt,
		); err != nil {
			return nil, err
		}
		blueprints = append(blueprints, bp)
	}

	// Load VMs for each blueprint
	for i := range blueprints {
		vms, err := q.listBlueprintVMs(ctx, blueprints[i].ID)
		if err != nil {
			return nil, err
		}
		blueprints[i].VMs = vms
	}

	return blueprints, nil
}

// ListAllBlueprints returns all blueprints (admin).
func (q *Queries) ListAllBlueprints(ctx context.Context) ([]models.Blueprint, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT b.id, b.name, b.description, b.created_by, b.allow_vm_additions,
		       b.is_active, b.created_at, b.updated_at,
		       u.id, u.username, u.email, COALESCE(u.display_name, ''), u.role
		FROM blueprints b
		JOIN users u ON u.id = b.created_by
		ORDER BY b.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var blueprints []models.Blueprint
	for rows.Next() {
		var bp models.Blueprint
		var creator models.User
		if err := rows.Scan(
			&bp.ID, &bp.Name, &bp.Description, &bp.CreatedBy, &bp.AllowVMAdditions,
			&bp.IsActive, &bp.CreatedAt, &bp.UpdatedAt,
			&creator.ID, &creator.Username, &creator.Email, &creator.DisplayName, &creator.Role,
		); err != nil {
			return nil, err
		}
		bp.Creator = &creator
		blueprints = append(blueprints, bp)
	}

	for i := range blueprints {
		vms, err := q.listBlueprintVMs(ctx, blueprints[i].ID)
		if err != nil {
			return nil, err
		}
		blueprints[i].VMs = vms
	}

	return blueprints, nil
}

// GetBlueprintByID retrieves a blueprint with its VMs.
func (q *Queries) GetBlueprintByID(ctx context.Context, id uuid.UUID) (*models.Blueprint, error) {
	var bp models.Blueprint
	err := q.pool.QueryRow(ctx, `
		SELECT id, name, description, created_by, allow_vm_additions,
		       is_active, created_at, updated_at
		FROM blueprints WHERE id = $1
	`, id).Scan(
		&bp.ID, &bp.Name, &bp.Description, &bp.CreatedBy, &bp.AllowVMAdditions,
		&bp.IsActive, &bp.CreatedAt, &bp.UpdatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	vms, err := q.listBlueprintVMs(ctx, bp.ID)
	if err != nil {
		return nil, err
	}
	bp.VMs = vms

	creator, err := q.GetUserByID(ctx, bp.CreatedBy)
	if err == nil && creator != nil {
		bp.Creator = creator
	}

	return &bp, nil
}

func (q *Queries) listBlueprintVMs(ctx context.Context, blueprintID uuid.UUID) ([]models.BlueprintVM, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT bv.id, bv.blueprint_id, bv.template_id, bv.display_name,
		       bv.vcpus, bv.ram_mb, bv.disk_gb, bv.boot_order, bv.quantity,
		       bv.created_at, COALESCE(t.name, '')
		FROM blueprint_vms bv
		LEFT JOIN templates t ON bv.template_id = t.id
		WHERE bv.blueprint_id = $1
		ORDER BY bv.boot_order, bv.created_at
	`, blueprintID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var vms []models.BlueprintVM
	for rows.Next() {
		var vm models.BlueprintVM
		if err := rows.Scan(
			&vm.ID, &vm.BlueprintID, &vm.TemplateID, &vm.DisplayName,
			&vm.VCPUs, &vm.RAMMB, &vm.DiskGB, &vm.BootOrder, &vm.Quantity,
			&vm.CreatedAt, &vm.TemplateName,
		); err != nil {
			return nil, err
		}
		vms = append(vms, vm)
	}
	return vms, nil
}

// CreateBlueprint creates a new blueprint with its VMs.
func (q *Queries) CreateBlueprint(ctx context.Context, bp *models.Blueprint) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	err = tx.QueryRow(ctx, `
		INSERT INTO blueprints (name, description, created_by, allow_vm_additions)
		VALUES ($1, $2, $3, $4)
		RETURNING id, is_active, created_at, updated_at
	`, bp.Name, bp.Description, bp.CreatedBy, bp.AllowVMAdditions).Scan(
		&bp.ID, &bp.IsActive, &bp.CreatedAt, &bp.UpdatedAt,
	)
	if err != nil {
		return err
	}

	for i := range bp.VMs {
		vm := &bp.VMs[i]
		err = tx.QueryRow(ctx, `
			INSERT INTO blueprint_vms (blueprint_id, template_id, display_name, vcpus, ram_mb, disk_gb, boot_order, quantity)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id, created_at
		`, bp.ID, vm.TemplateID, vm.DisplayName, vm.VCPUs, vm.RAMMB, vm.DiskGB, vm.BootOrder, vm.Quantity).Scan(
			&vm.ID, &vm.CreatedAt,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// UpdateBlueprint updates a blueprint and replaces its VMs.
func (q *Queries) UpdateBlueprint(ctx context.Context, bp *models.Blueprint) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `
		UPDATE blueprints SET name = $1, description = $2, allow_vm_additions = $3, updated_at = now()
		WHERE id = $4
	`, bp.Name, bp.Description, bp.AllowVMAdditions, bp.ID)
	if err != nil {
		return err
	}

	// Replace all VMs
	_, err = tx.Exec(ctx, `DELETE FROM blueprint_vms WHERE blueprint_id = $1`, bp.ID)
	if err != nil {
		return err
	}

	for i := range bp.VMs {
		vm := &bp.VMs[i]
		err = tx.QueryRow(ctx, `
			INSERT INTO blueprint_vms (blueprint_id, template_id, display_name, vcpus, ram_mb, disk_gb, boot_order, quantity)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id, created_at
		`, bp.ID, vm.TemplateID, vm.DisplayName, vm.VCPUs, vm.RAMMB, vm.DiskGB, vm.BootOrder, vm.Quantity).Scan(
			&vm.ID, &vm.CreatedAt,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

// DeleteBlueprint soft-deletes a blueprint by setting is_active = false.
func (q *Queries) DeleteBlueprint(ctx context.Context, id uuid.UUID) error {
	_, err := q.pool.Exec(ctx, `UPDATE blueprints SET is_active = false, updated_at = now() WHERE id = $1`, id)
	return err
}

// SetBlueprintAccess replaces all access rules for a blueprint.
func (q *Queries) SetBlueprintAccess(ctx context.Context, blueprintID uuid.UUID, rules []models.BlueprintAccess) error {
	tx, err := q.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx, `DELETE FROM blueprint_access WHERE blueprint_id = $1`, blueprintID)
	if err != nil {
		return err
	}

	for _, rule := range rules {
		_, err = tx.Exec(ctx, `
			INSERT INTO blueprint_access (blueprint_id, user_id, role) VALUES ($1, $2, $3)
		`, blueprintID, rule.UserID, rule.Role)
		if err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

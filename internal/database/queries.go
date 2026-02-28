package database

import (
	"context"
	"fmt"

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
	var t models.Template
	err := q.pool.QueryRow(ctx, `
		INSERT INTO templates (name, vcenter_template, os_type, default_vcpus, default_ram_mb,
		                       default_disk_gb, min_vcpus, min_ram_mb, description, icon_url,
		                       default_username, default_password)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		RETURNING id, name, vcenter_template, os_type, default_vcpus, default_ram_mb,
		          default_disk_gb, min_vcpus, min_ram_mb, COALESCE(description, ''), COALESCE(icon_url, ''),
		          default_username, default_password, is_active, created_at
	`, req.Name, req.VCenterTemplate, req.OSType, req.DefaultVCPUs, req.DefaultRAMMB,
		req.DefaultDiskGB, req.MinVCPUs, req.MinRAMMB, req.Description, req.IconURL,
		req.DefaultUsername, req.DefaultPassword,
	).Scan(
		&t.ID, &t.Name, &t.VCenterTemplate, &t.OSType, &t.DefaultVCPUs, &t.DefaultRAMMB,
		&t.DefaultDiskGB, &t.MinVCPUs, &t.MinRAMMB, &t.Description, &t.IconURL,
		&t.DefaultUsername, &t.DefaultPassword, &t.IsActive, &t.CreatedAt,
	)
	return &t, err
}

// ListTemplatesForUser returns templates accessible to a user based on their role.
func (q *Queries) ListTemplatesForUser(ctx context.Context, userID uuid.UUID, role string) ([]models.Template, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT DISTINCT t.id, t.name, t.vcenter_template, t.os_type, t.default_vcpus,
		       t.default_ram_mb, t.default_disk_gb, t.min_vcpus, t.min_ram_mb,
		       COALESCE(t.description, ''), COALESCE(t.icon_url, ''),
		       t.default_username, t.default_password, t.is_active, t.created_at
		FROM templates t
		LEFT JOIN template_access ta ON t.id = ta.template_id
		WHERE t.is_active = true
		  AND (ta.role = $1 OR ta.user_id = $2 OR ta.role = 'admin'
		       OR NOT EXISTS (SELECT 1 FROM template_access WHERE template_id = t.id))
		ORDER BY t.name
	`, role, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []models.Template
	for rows.Next() {
		var t models.Template
		if err := rows.Scan(
			&t.ID, &t.Name, &t.VCenterTemplate, &t.OSType, &t.DefaultVCPUs,
			&t.DefaultRAMMB, &t.DefaultDiskGB, &t.MinVCPUs, &t.MinRAMMB,
			&t.Description, &t.IconURL, &t.DefaultUsername, &t.DefaultPassword,
			&t.IsActive, &t.CreatedAt,
		); err != nil {
			return nil, err
		}
		templates = append(templates, t)
	}
	return templates, nil
}

// ListAllTemplates returns all templates (admin).
func (q *Queries) ListAllTemplates(ctx context.Context) ([]models.Template, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT id, name, vcenter_template, os_type, default_vcpus, default_ram_mb,
		       default_disk_gb, min_vcpus, min_ram_mb, COALESCE(description, ''), COALESCE(icon_url, ''),
		       default_username, default_password, is_active, created_at
		FROM templates ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var templates []models.Template
	for rows.Next() {
		var t models.Template
		if err := rows.Scan(
			&t.ID, &t.Name, &t.VCenterTemplate, &t.OSType, &t.DefaultVCPUs,
			&t.DefaultRAMMB, &t.DefaultDiskGB, &t.MinVCPUs, &t.MinRAMMB,
			&t.Description, &t.IconURL, &t.DefaultUsername, &t.DefaultPassword,
			&t.IsActive, &t.CreatedAt,
		); err != nil {
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
	var t models.Template
	err := q.pool.QueryRow(ctx, `
		UPDATE templates SET
			name = COALESCE($2, name),
			description = COALESCE($3, description),
			icon_url = COALESCE($4, icon_url),
			default_vcpus = COALESCE($5, default_vcpus),
			default_ram_mb = COALESCE($6, default_ram_mb),
			default_disk_gb = COALESCE($7, default_disk_gb),
			is_active = COALESCE($8, is_active),
			default_username = COALESCE($9, default_username),
			default_password = COALESCE($10, default_password)
		WHERE id = $1
		RETURNING id, name, vcenter_template, os_type, default_vcpus, default_ram_mb,
		          default_disk_gb, min_vcpus, min_ram_mb, COALESCE(description, ''), COALESCE(icon_url, ''),
		          default_username, default_password, is_active, created_at
	`, id, req.Name, req.Description, req.IconURL, req.DefaultVCPUs, req.DefaultRAMMB, req.DefaultDiskGB, req.IsActive,
		req.DefaultUsername, req.DefaultPassword,
	).Scan(
		&t.ID, &t.Name, &t.VCenterTemplate, &t.OSType, &t.DefaultVCPUs, &t.DefaultRAMMB,
		&t.DefaultDiskGB, &t.MinVCPUs, &t.MinRAMMB, &t.Description, &t.IconURL,
		&t.DefaultUsername, &t.DefaultPassword, &t.IsActive, &t.CreatedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &t, err
}

// DeleteTemplate removes a template by ID.
func (q *Queries) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	_, err := q.pool.Exec(ctx, "DELETE FROM templates WHERE id = $1", id)
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
		       error_message, expires_at, created_at, updated_at
		FROM pods WHERE id = $1
	`, id).Scan(
		&p.ID, &p.OwnerID, &p.Name, &p.Salt, &p.VLANID, &p.Subnet, &p.Status,
		&p.ErrorMessage, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt,
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
		       pv.generated_username, pv.generated_password, pv.created_at
		FROM pod_vms pv
		LEFT JOIN templates t ON pv.template_id = t.id
		WHERE pv.pod_id = $1 AND pv.status != 'deleted' ORDER BY pv.created_at
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
			&vm.GeneratedUsername, &vm.GeneratedPassword, &vm.CreatedAt,
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
		       p.error_message, p.expires_at, p.created_at, p.updated_at,
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
			&p.ErrorMessage, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt,
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
		       p.error_message, p.expires_at, p.created_at, p.updated_at,
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
			&p.ErrorMessage, &p.ExpiresAt, &p.CreatedAt, &p.UpdatedAt,
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
		       pv.generated_username, pv.generated_password, pv.created_at
		FROM pod_vms pv
		LEFT JOIN templates t ON pv.template_id = t.id
		WHERE pv.pod_id = $1 AND pv.status != 'deleted' ORDER BY pv.created_at
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
			&vm.GeneratedUsername, &vm.GeneratedPassword, &vm.CreatedAt,
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
			ORDER BY created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, type, payload, status, claimed_by, claimed_at,
		          retry_count, max_retries, rollback_steps, created_at
	`, workerID).Scan(
		&j.ID, &j.Type, &j.Payload, &j.Status, &j.ClaimedBy, &j.ClaimedAt,
		&j.RetryCount, &j.MaxRetries, &j.RollbackSteps, &j.CreatedAt,
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

// --- Pod VMs ---

// ListPodVMs returns all VMs belonging to a pod.
func (q *Queries) ListPodVMs(ctx context.Context, podID uuid.UUID) ([]models.PodVM, error) {
	rows, err := q.pool.Query(ctx, `
		SELECT pv.id, pv.pod_id, pv.template_id, pv.display_name, pv.vcenter_vm_name, pv.vcenter_vm_id,
		       pv.vcpus, pv.ram_mb, pv.disk_gb, pv.ip_address, pv.status,
		       COALESCE(t.default_username, ''), COALESCE(t.default_password, ''),
		       pv.generated_username, pv.generated_password, pv.created_at
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
			&vm.GeneratedUsername, &vm.GeneratedPassword, &vm.CreatedAt)
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
		       pv.generated_username, pv.generated_password, pv.created_at
		FROM pod_vms pv
		LEFT JOIN templates t ON pv.template_id = t.id
		WHERE pv.id = $1
	`, id).Scan(&vm.ID, &vm.PodID, &vm.TemplateID, &vm.DisplayName, &vm.VCenterVMName, &vm.VCenterVMID,
		&vm.VCPUs, &vm.RAMMB, &vm.DiskGB, &vm.IPAddress, &vm.Status,
		&vm.DefaultUsername, &vm.DefaultPassword,
		&vm.GeneratedUsername, &vm.GeneratedPassword, &vm.CreatedAt)
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
	err := q.pool.QueryRow(ctx, `
		SELECT id, name, vcenter_template, os_type, default_vcpus, default_ram_mb,
		       default_disk_gb, min_vcpus, min_ram_mb, COALESCE(description, ''), COALESCE(icon_url, ''),
		       default_username, default_password, is_active, created_at
		FROM templates WHERE id = $1
	`, id).Scan(
		&t.ID, &t.Name, &t.VCenterTemplate, &t.OSType, &t.DefaultVCPUs, &t.DefaultRAMMB,
		&t.DefaultDiskGB, &t.MinVCPUs, &t.MinRAMMB, &t.Description, &t.IconURL,
		&t.DefaultUsername, &t.DefaultPassword, &t.IsActive, &t.CreatedAt,
	)
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

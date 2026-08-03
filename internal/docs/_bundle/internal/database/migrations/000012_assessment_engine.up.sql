-- Migration: 000012_assessment_engine
-- Description: Workflow engine tables for the automated assessment system
-- Depends on: 000010_blueprints (pod_blueprints table), 000001_init (users, pods tables)

-- Workflows: instructor-authored assessment scripts with approval lifecycle
CREATE TABLE workflows (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name                TEXT NOT NULL,
    slug                TEXT NOT NULL UNIQUE,
    description         TEXT NOT NULL DEFAULT '',
    category            TEXT NOT NULL DEFAULT 'general',
    execution_mode      TEXT NOT NULL DEFAULT 'kali_runner'
                            CHECK (execution_mode IN ('kali_runner', 'vmware_tools')),
    script              TEXT NOT NULL DEFAULT '',
    setup_script        TEXT,
    timeout_seconds     INT NOT NULL DEFAULT 300,
    target_os           TEXT,
    target_vm           TEXT,
    guest_interpreter   TEXT,
    required_services   TEXT[],
    metadata            JSONB NOT NULL DEFAULT '{}',
    visible_to_students BOOLEAN NOT NULL DEFAULT false,
    status              TEXT NOT NULL DEFAULT 'draft'
                            CHECK (status IN ('draft', 'pending_review', 'approved', 'active')),
    creation_mode       TEXT NOT NULL DEFAULT 'visual'
                            CHECK (creation_mode IN ('visual', 'script')),
    created_by          UUID NOT NULL REFERENCES users(id),
    approved_by         UUID REFERENCES users(id),
    is_active           BOOLEAN NOT NULL DEFAULT true,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_workflows_slug ON workflows(slug);
CREATE INDEX idx_workflows_category ON workflows(category);
CREATE INDEX idx_workflows_status ON workflows(status);
CREATE INDEX idx_workflows_active ON workflows(is_active) WHERE is_active = true;
CREATE INDEX idx_workflows_mode ON workflows(execution_mode);
CREATE INDEX idx_workflows_created_by ON workflows(created_by);
CREATE INDEX idx_workflows_visible ON workflows(visible_to_students) WHERE visible_to_students = true;

-- Actions: ordered steps within a workflow
CREATE TABLE actions (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_id       UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    description       TEXT NOT NULL DEFAULT '',
    action_type       TEXT NOT NULL DEFAULT 'command',
    params            JSONB NOT NULL DEFAULT '{}',
    execution_order   INT NOT NULL DEFAULT 0,
    timeout_seconds   INT NOT NULL DEFAULT 60,
    student_fail_hint TEXT,
    points            INT,
    penalty           INT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_actions_workflow ON actions(workflow_id, execution_order);

-- Workflow versions: immutable snapshots for run pinning
CREATE TABLE workflow_versions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_id UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version     INT NOT NULL,
    script      TEXT NOT NULL,
    actions     JSONB NOT NULL,
    created_by  UUID REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workflow_id, version)
);

CREATE INDEX idx_workflow_versions_workflow ON workflow_versions(workflow_id, version);

-- Playlists: named collections of workflows
CREATE TABLE playlists (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL UNIQUE,
    description  TEXT NOT NULL DEFAULT '',
    scoring_mode TEXT NOT NULL DEFAULT 'pass_fail'
                     CHECK (scoring_mode IN ('pass_fail', 'points')),
    created_by   UUID NOT NULL REFERENCES users(id),
    is_active    BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_playlists_slug ON playlists(slug);
CREATE INDEX idx_playlists_active ON playlists(is_active) WHERE is_active = true;

-- Playlist-workflow join table
CREATE TABLE playlist_workflows (
    playlist_id     UUID NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    workflow_id     UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    execution_order INT NOT NULL DEFAULT 0,
    PRIMARY KEY (playlist_id, workflow_id)
);

CREATE INDEX idx_playlist_workflows_order ON playlist_workflows(playlist_id, execution_order);

-- Playlist access control
CREATE TABLE playlist_access (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    playlist_id UUID NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    role        TEXT,
    user_id     UUID REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_playlist_access_target CHECK (role IS NOT NULL OR user_id IS NOT NULL)
);

CREATE INDEX idx_playlist_access_playlist ON playlist_access(playlist_id);
CREATE INDEX idx_playlist_access_role ON playlist_access(role) WHERE role IS NOT NULL;
CREATE INDEX idx_playlist_access_user ON playlist_access(user_id) WHERE user_id IS NOT NULL;

-- Blueprint-playlist default assignments
CREATE TABLE blueprint_playlists (
    blueprint_id UUID NOT NULL REFERENCES blueprints(id) ON DELETE CASCADE,
    playlist_id  UUID NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    is_default   BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (blueprint_id, playlist_id)
);

-- Runs: one execution of a playlist against a pod
CREATE TABLE runs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    pod_id            UUID NOT NULL REFERENCES pods(id) ON DELETE CASCADE,
    playlist_id       UUID REFERENCES playlists(id) ON DELETE SET NULL,
    triggered_by      UUID NOT NULL REFERENCES users(id),
    runner_vm_id      TEXT,
    runner_vm_name    TEXT,
    callback_token    TEXT NOT NULL UNIQUE,
    status            TEXT NOT NULL DEFAULT 'pending'
                          CHECK (status IN (
                              'pending', 'provisioning', 'running',
                              'completed', 'failed', 'cancelled', 'timeout'
                          )),
    total_workflows   INT NOT NULL DEFAULT 0,
    passed_workflows  INT NOT NULL DEFAULT 0,
    failed_workflows  INT NOT NULL DEFAULT 0,
    total_points      INT,
    earned_points     INT,
    error_message     TEXT,
    started_at        TIMESTAMPTZ,
    completed_at      TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_runs_pod ON runs(pod_id);
CREATE INDEX idx_runs_playlist ON runs(playlist_id) WHERE playlist_id IS NOT NULL;
CREATE INDEX idx_runs_triggered_by ON runs(triggered_by);
CREATE INDEX idx_runs_status ON runs(status) WHERE status NOT IN ('completed', 'failed', 'cancelled', 'timeout');
CREATE INDEX idx_runs_callback ON runs(callback_token);

-- Workflow results: per-workflow outcome within a run
CREATE TABLE workflow_results (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id              UUID NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    workflow_id         UUID NOT NULL REFERENCES workflows(id),
    workflow_version_id UUID REFERENCES workflow_versions(id),
    execution_order     INT NOT NULL DEFAULT 0,
    execution_mode      TEXT NOT NULL DEFAULT 'kali_runner',
    status              TEXT NOT NULL DEFAULT 'pending'
                            CHECK (status IN (
                                'pending', 'running', 'pass', 'fail',
                                'error', 'timeout', 'skipped', 'cancelled'
                            )),
    student_message     TEXT,
    instructor_output   JSONB NOT NULL DEFAULT '{}'::jsonb,
    action_results      JSONB,
    points_awarded      INT,
    duration_ms         INT,
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_workflow_results_run ON workflow_results(run_id);
CREATE INDEX idx_workflow_results_workflow ON workflow_results(workflow_id);
CREATE INDEX idx_workflow_results_status ON workflow_results(run_id, status);

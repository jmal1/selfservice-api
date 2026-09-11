-- Migration: 000013_playlist_template_binding
-- Description: Replace playlist_access and blueprint_playlists with template-scoped
--              and blueprint-VM-scoped playlist assignment.
--
-- Design change: Playlists are now assigned at two levels:
--   1. template_playlists — defaults for all VMs cloned from a template
--   2. blueprint_vm_playlists — per-VM overrides within a blueprint (replaces template defaults)
-- Students see only playlists assigned to their VM. No global playlist browsing.

-- Drop the old tables
DROP TABLE IF EXISTS blueprint_playlists;
DROP TABLE IF EXISTS playlist_access;

-- Template-level playlist defaults
CREATE TABLE template_playlists (
    template_id     UUID NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
    playlist_id     UUID NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    execution_order INT NOT NULL DEFAULT 0,
    PRIMARY KEY (template_id, playlist_id)
);

CREATE INDEX idx_template_playlists_template ON template_playlists(template_id, execution_order);

-- Blueprint-VM-level playlist overrides (completely replace template defaults when present)
CREATE TABLE blueprint_vm_playlists (
    blueprint_id    UUID NOT NULL REFERENCES blueprints(id) ON DELETE CASCADE,
    vm_slot         INT NOT NULL,
    playlist_id     UUID NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    execution_order INT NOT NULL DEFAULT 0,
    PRIMARY KEY (blueprint_id, vm_slot, playlist_id)
);

CREATE INDEX idx_blueprint_vm_playlists_lookup ON blueprint_vm_playlists(blueprint_id, vm_slot, execution_order);

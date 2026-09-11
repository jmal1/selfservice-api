-- Rollback: 000013_playlist_template_binding

DROP TABLE IF EXISTS blueprint_vm_playlists;
DROP TABLE IF EXISTS template_playlists;

-- Recreate the old tables
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

CREATE TABLE blueprint_playlists (
    blueprint_id UUID NOT NULL REFERENCES blueprints(id) ON DELETE CASCADE,
    playlist_id  UUID NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    is_default   BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (blueprint_id, playlist_id)
);

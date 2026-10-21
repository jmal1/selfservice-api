-- Lab briefing images. Content-addressed: the object lives at
-- crucible/briefings/<sha256> in the image object store bucket.

BEGIN;

CREATE TABLE briefing_assets (
    sha256       TEXT PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    content_type TEXT NOT NULL CHECK (content_type IN ('image/png', 'image/jpeg', 'image/gif', 'image/webp')),
    size_bytes   BIGINT NOT NULL CHECK (size_bytes > 0 AND size_bytes <= 5242880),
    uploaded_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMIT;

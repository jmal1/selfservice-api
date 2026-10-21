-- Lab briefings: versioned Markdown that instructors attach to a template or
-- a blueprint. Each save is an immutable revision; students only ever read the
-- revision that published_revision_id points at.

BEGIN;

CREATE TABLE briefings (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id           UUID REFERENCES templates(id)  ON DELETE CASCADE,
    blueprint_id          UUID REFERENCES blueprints(id) ON DELETE CASCADE,
    published_revision_id UUID,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT briefings_one_owner CHECK ((template_id IS NULL) <> (blueprint_id IS NULL))
);
CREATE UNIQUE INDEX briefings_template_uq  ON briefings (template_id)  WHERE template_id  IS NOT NULL;
CREATE UNIQUE INDEX briefings_blueprint_uq ON briefings (blueprint_id) WHERE blueprint_id IS NOT NULL;

CREATE TABLE briefing_revisions (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    briefing_id   UUID NOT NULL REFERENCES briefings(id) ON DELETE CASCADE,
    revision      INT  NOT NULL CHECK (revision > 0),
    parent_id     UUID REFERENCES briefing_revisions(id) ON DELETE CASCADE,
    body_md       TEXT NOT NULL CHECK (octet_length(body_md) <= 262144),
    body_sha256   TEXT NOT NULL,
    message       TEXT NOT NULL DEFAULT '',
    author_id     UUID REFERENCES users(id) ON DELETE SET NULL,
    restored_from INT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (briefing_id, revision)
);

ALTER TABLE briefings
    ADD CONSTRAINT briefings_published_fk
    FOREIGN KEY (published_revision_id) REFERENCES briefing_revisions(id);

COMMIT;

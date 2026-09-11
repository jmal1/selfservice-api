-- Image uploads: browser -> MinIO (staging) -> vCenter (datastore ISO or
-- imported OVA). Backs the /admin/images surface and the image_import
-- worker job.
--
-- Lifecycle: pending -> uploading -> uploaded -> importing -> imported
-- with `error` as the terminal failure state (retryable: the MinIO object
-- is deliberately NOT deleted on import failure so a retry is cheap).
--
-- On *success* the MinIO object IS deleted, because stagingv01 keeps
-- /data/minio on its 97 GB root filesystem (~85 GB free) while the ISO
-- datastore NAS-BackupsAndISOS has ~6.7 TB. MinIO is a transit buffer,
-- not a library.
CREATE TABLE IF NOT EXISTS image_uploads (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    filename        TEXT NOT NULL,
    kind            TEXT NOT NULL CHECK (kind IN ('iso', 'ova')),
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    checksum_sha256 TEXT NOT NULL DEFAULT '',
    object_key      TEXT NOT NULL,
    upload_id       TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'uploading', 'uploaded',
                                      'importing', 'imported', 'error')),
    -- ISO only, e.g. "[NAS-BackupsAndISOS] ISOs/kali-2024.4.iso".
    datastore_path  TEXT NOT NULL DEFAULT '',
    -- OVA only: moref of the VM created by the OVF import.
    vcenter_vm_id   TEXT NOT NULL DEFAULT '',
    error_message   TEXT NOT NULL DEFAULT '',
    uploaded_by     UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_image_uploads_status ON image_uploads (status);
CREATE UNIQUE INDEX IF NOT EXISTS idx_image_uploads_object_key ON image_uploads (object_key);

-- Unattended-install automation for source_type='iso' templates.
--   manual              - operator installs via the WebMKS console (fallback)
--   cloudinit_cidata    - CIDATA seed ISO (Ubuntu subiquity autoinstall)
--   debian_preseed      - remastered install ISO with /preseed.cfg (Kali/Debian)
--   windows_autounattend- autounattend.xml at the seed ISO root
ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS unattend_mode TEXT NOT NULL DEFAULT 'manual';

ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS unattend_config JSONB NOT NULL DEFAULT '{}'::jsonb;

-- guest_id is the vSphere GuestOS identifier used when creating the blank
-- VM for an ISO install (e.g. "ubuntu64Guest", "debian12_64Guest").
-- Ignored by every non-ISO source type.
ALTER TABLE templates
    ADD COLUMN IF NOT EXISTS guest_id TEXT NOT NULL DEFAULT '';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'templates_unattend_mode_check'
    ) THEN
        ALTER TABLE templates ADD CONSTRAINT templates_unattend_mode_check
            CHECK (unattend_mode IN ('manual', 'cloudinit_cidata',
                                     'debian_preseed', 'windows_autounattend'));
    END IF;
END $$;

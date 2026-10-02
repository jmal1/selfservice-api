-- Standard student quota is 4 vCPU (already the default) and 4 GiB RAM.
-- Only rows still on the previous 8 GiB student default are lowered.

ALTER TABLE users ALTER COLUMN max_ram_mb SET DEFAULT 4096;

UPDATE users
SET max_ram_mb = 4096,
    updated_at = now()
WHERE role = 'student'
  AND max_ram_mb = 8192;

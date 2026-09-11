ALTER TABLE templates
    DROP COLUMN IF EXISTS last_validation_result,
    DROP COLUMN IF EXISTS last_validated_at,
    DROP COLUMN IF EXISTS trust_tier;

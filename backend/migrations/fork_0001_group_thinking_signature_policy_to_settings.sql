-- Fork migration: per-group thinking.signature policy moves from
-- groups.thinking_invalid_signature_policy into settings rows keyed
-- "thinking_invalid_signature_policy:group:<id>".
--
-- Fork migrations use the fork_NNNN_ prefix so they sort after every upstream
-- NNN_ migration and never collide with upstream numbering.
--
-- The column only exists on databases that applied the earlier fork migration
-- 246_group_thinking_invalid_signature_policy.sql. It is left in place (NOT NULL
-- DEFAULT '' keeps inserts working) so an older binary can still run during a
-- rollback; drop it manually once no deployment needs it.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'groups'
          AND column_name = 'thinking_invalid_signature_policy'
    ) THEN
        INSERT INTO settings (key, value, updated_at)
        SELECT 'thinking_invalid_signature_policy:group:' || id,
               thinking_invalid_signature_policy,
               NOW()
        FROM groups
        WHERE thinking_invalid_signature_policy IN ('strip', 'reject')
          AND deleted_at IS NULL
        ON CONFLICT (key) DO NOTHING;
    END IF;
END $$;

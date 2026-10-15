-- Per-group policy for invalid/missing Anthropic thinking.signature blocks.
-- Empty string means follow the global setting (default strip).
-- Valid values: '' | strip | reject
ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS thinking_invalid_signature_policy VARCHAR(20) NOT NULL DEFAULT '';

COMMENT ON COLUMN groups.thinking_invalid_signature_policy IS
    'Anthropic-strict thinking.signature policy: empty=follow global, strip=drop invalid blocks, reject=HTTP 400';

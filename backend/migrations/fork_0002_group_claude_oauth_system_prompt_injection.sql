ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS disable_claude_oauth_system_prompt_injection BOOLEAN NOT NULL DEFAULT false;

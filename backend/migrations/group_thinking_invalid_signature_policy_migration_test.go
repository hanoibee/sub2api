package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGroupThinkingInvalidSignaturePolicyMigration(t *testing.T) {
	content, err := FS.ReadFile("246_group_thinking_invalid_signature_policy.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql,
		"ADD COLUMN IF NOT EXISTS thinking_invalid_signature_policy VARCHAR(20) NOT NULL DEFAULT ''")
	require.Contains(t, sql, "COMMENT ON COLUMN groups.thinking_invalid_signature_policy")
}

package migrations

import (
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Fork migrations must run after every upstream NNN_ migration (the runner
// applies files in sort.Strings order) and must never reuse an upstream number.
func TestForkMigrationsSortAfterUpstream(t *testing.T) {
	files, err := fs.Glob(FS, "*.sql")
	require.NoError(t, err)
	sort.Strings(files)

	seenFork := false
	for _, name := range files {
		isFork := strings.HasPrefix(name, "fork_")
		if seenFork {
			require.Truef(t, isFork, "upstream migration %s sorts after a fork_ migration", name)
		}
		seenFork = seenFork || isFork
	}
	require.True(t, seenFork, "expected at least one fork_ migration")
}

func TestForkGroupThinkingSignaturePolicyToSettingsMigration(t *testing.T) {
	content, err := FS.ReadFile("fork_0001_group_thinking_signature_policy_to_settings.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	// Only databases that applied the old fork column migration carry data.
	require.Contains(t, sql, "column_name = 'thinking_invalid_signature_policy'")
	require.Contains(t, sql, "'thinking_invalid_signature_policy:group:' || id")
	require.Contains(t, sql, "WHERE thinking_invalid_signature_policy IN ('strip', 'reject')")
	require.Contains(t, sql, "ON CONFLICT (key) DO NOTHING")
	// The column is kept so an older binary can still run during rollback.
	require.NotContains(t, strings.ToUpper(sql), "DROP COLUMN")
	_, err = FS.ReadFile("246_group_thinking_invalid_signature_policy.sql")
	require.Error(t, err, "the fork column migration must not occupy upstream number 246")
}

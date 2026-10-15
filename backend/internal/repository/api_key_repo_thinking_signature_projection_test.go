package repository

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGetByKeyForAuthProjectsThinkingInvalidSignaturePolicy guards against
// silent policy fallback: auth-cache snapshots are built from GetByKeyForAuth's
// explicit Group Select. Omitting thinking_invalid_signature_policy makes every
// group look like "" (inherit → global strip), so reject never fires on the
// hot path even when groups.thinking_invalid_signature_policy='reject'.
func TestGetByKeyForAuthProjectsThinkingInvalidSignaturePolicy(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	srcPath := filepath.Join(filepath.Dir(thisFile), "api_key_repo.go")
	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("read api_key_repo.go: %v", err)
	}
	needle := "group.FieldThinkingInvalidSignaturePolicy"
	if !strings.Contains(string(src), needle) {
		t.Fatalf("GetByKeyForAuth WithGroup Select must include %s", needle)
	}
	// Ensure it sits inside the GetByKeyForAuth method body, not only Update/Create.
	idx := strings.Index(string(src), "func (r *apiKeyRepository) GetByKeyForAuth")
	if idx < 0 {
		t.Fatal("GetByKeyForAuth not found")
	}
	rest := string(src)[idx:]
	end := strings.Index(rest, "\nfunc (")
	if end > 0 {
		rest = rest[:end]
	}
	if !strings.Contains(rest, needle) {
		t.Fatalf("GetByKeyForAuth must Select %s (found elsewhere but not in method)", needle)
	}
}

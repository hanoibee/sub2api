package requestarchive

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchivePreservesPrivateDataWhileRedactionIsDisabled(t *testing.T) {
	root := t.TempDir()
	started := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	if err := Initialize(Config{Root: root}, started); err != nil {
		t.Fatal(err)
	}
	request := `{"model":"safe-model","max_tokens":100,"user":"customer-42","password":"open-sesame","messages":[{"role":"user","content":"联系 alice@example.com 或 13800138000，Authorization: Bearer abcdefghijklmnop"}],"image_url":"data:image/png;base64,PRIVATE"}`
	response := `{"id":"msg-safe","content":[{"type":"thinking","thinking":"alice@example.com"},{"type":"text","text":"发送到 alice@example.com"}],"signature":"private-signature","usage":{"input_tokens":10,"output_tokens":2}}`
	_, scope := WithScope(context.Background(), Config{Root: root}, "session-customer-42", started)
	wrapped := scope.Wrap(io.NopCloser(strings.NewReader(response)), []byte(request), Info{Protocol: "ANTHROPIC", Model: "safe-model", Status: 200}, false, int64(len(response)))
	transport, err := io.ReadAll(wrapped)
	if err != nil || string(transport) != response {
		t.Fatalf("transport changed: %v", err)
	}
	_ = wrapped.Close()
	scope.Finish()
	dir := filepath.Join(root, "2026-09-22", "req_20260922_000001")
	requestArchive, err := os.ReadFile(filepath.Join(dir, "biz_request.json"))
	if err != nil {
		t.Fatal(err)
	}
	responseArchive, err := os.ReadFile(filepath.Join(dir, "biz_response.json"))
	if err != nil {
		t.Fatal(err)
	}
	combined := string(requestArchive) + string(responseArchive)
	for _, original := range []string{"customer-42", "open-sesame", "alice@example.com", "13800138000", "abcdefghijklmnop", "PRIVATE", "private-signature"} {
		if !strings.Contains(combined, original) {
			t.Fatalf("原始内容未完整保存: %s", original)
		}
	}
	if strings.Contains(combined, redactedValue) || !strings.Contains(string(responseArchive), `"id":"msg-safe"`) {
		t.Fatalf("归档内容被意外脱敏: %s", combined)
	}
	metaRaw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta Metadata
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.SessionID == nil || *meta.SessionID != "session-customer-42" {
		t.Fatalf("sessionId was unexpectedly changed: %+v", meta.SessionID)
	}
}

func TestArchiveRedactsChineseKeysAndText(t *testing.T) {
	raw := `{"密码":"1234","账户名":"alice-account","银行卡号":"6222 0202 0202 0202","身份证号":"11010519491231002X","messages":[{"content":"我的邮箱是 alice@example.com，收货地址：北京市朝阳区，访问令牌：cn-token-secret，持卡人：张三，银行卡号：6222 0202 0202 0202"}],"max_tokens":128}`
	redacted, err := redactJSON([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	value := string(redacted)
	for _, privateValue := range []string{"1234", "alice-account", "6222 0202 0202 0202", "11010519491231002X", "alice@example.com", "北京市朝阳区", "cn-token-secret", "张三"} {
		if strings.Contains(value, privateValue) {
			t.Fatalf("中文隐私数据泄露: %s", privateValue)
		}
	}
	if !strings.Contains(value, `"max_tokens":128`) || !strings.Contains(value, redactedValue) {
		t.Fatalf("意外的脱敏结果: %s", value)
	}
}

func TestArchiveLeavesSubtreePastDepthLimitUntouched(t *testing.T) {
	value := any("secret-at-depth-65")
	for i := 0; i < 65; i++ {
		value = map[string]any{"nested": value}
	}
	redacted := redactArchiveValue(value, "", 0)
	current := redacted
	for i := 0; i < 65; i++ {
		current = current.(map[string]any)["nested"]
	}
	if current != "secret-at-depth-65" {
		t.Fatalf("deep subtree was changed: %v", current)
	}
}

func TestStreamPreservesEachEventAndDoneWhileRedactionIsDisabled(t *testing.T) {
	root := t.TempDir()
	started := time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC)
	if err := Initialize(Config{Root: root}, started); err != nil {
		t.Fatal(err)
	}
	_, scope := WithScope(context.Background(), Config{Root: root}, "", started)
	stream := "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"alice@example.com\",\"signature\":\"secret-signature\"}}\n\ndata: [DONE]\n\n"
	wrapped := scope.Wrap(io.NopCloser(strings.NewReader(stream)), []byte(`{"stream":true}`), Info{Protocol: "ANTHROPIC", Stream: true, Status: 200}, true, -1)
	transport, err := io.ReadAll(wrapped)
	if err != nil || string(transport) != stream {
		t.Fatalf("transport changed: %v", err)
	}
	_ = wrapped.Close()
	scope.Finish()
	archived, err := os.ReadFile(filepath.Join(root, "2026-09-22", "req_20260922_000001", "biz_stream_response.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(archived), "alice@example.com") || !strings.Contains(string(archived), "secret-signature") {
		t.Fatalf("原始流式内容未完整保存: %s", archived)
	}
	if !strings.HasSuffix(string(archived), "[DONE]\n") {
		t.Fatalf("DONE marker changed: %s", archived)
	}
}

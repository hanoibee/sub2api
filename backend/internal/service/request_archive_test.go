package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestarchive"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
)

type archiveTestUpstream struct{ client *http.Client }

func (u archiveTestUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.client.Do(r)
}
func (u archiveTestUpstream) DoWithTLS(r *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, id, n)
}
func TestRequestArchiveUpstreamIntegration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[stream], func(t *testing.T) {
			request := `{ "model":"actual-model","stream":false,"custom":9007199254740993 }`
			responsePayload := `{ "id":"upstream-response", "usage":{"input_tokens":12,"output_tokens":4}, "output":[],"custom":9007199254740993 }`
			response := responsePayload
			contentType := "application/json"
			wantFile := responsePayload
			filename := "biz_response.json"
			if stream {
				request = strings.Replace(request, "false", "true", 1)
				contentType = "text/event-stream"
				response = "data: " + responsePayload + "\n\ndata: [DONE]\n\n"
				wantFile = responsePayload + "\n[DONE]\n"
				filename = "biz_stream_response.jsonl"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != request {
					t.Error("upstream request modified")
				}
				w.Header().Set("Content-Type", contentType)
				_, _ = io.WriteString(w, response)
			}))
			defer server.Close()
			ctx := context.Background()
			root := t.TempDir()
			ctx, scope := requestarchive.WithScope(ctx, requestarchive.Config{Root: root, ProviderCode: "deployment-a"}, "client-session", time.Date(2026, 9, 21, 16, 0, 0, 0, time.UTC))
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", bytes.NewBufferString(request))
			if err != nil {
				t.Fatal(err)
			}
			account := &Account{ID: 1, Platform: PlatformDeepseek, Type: AccountTypeAPIKey}
			upstream := archiveHTTPUpstream(archiveTestUpstream{server.Client()}, account)
			resp, err := upstream.DoWithTLS(req, "", 1, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(raw) != response {
				t.Fatal("response changed", err)
			}
			scope.Finish()
			dir := filepath.Join(root, "2026-09-22", "req_20260922_000001")
			archived, err := os.ReadFile(filepath.Join(dir, filename))
			if err != nil || string(archived) != wantFile {
				t.Fatal("archive differs", err)
			}
			saved, err := os.ReadFile(filepath.Join(dir, "biz_request.json"))
			if err != nil || string(saved) != request {
				t.Fatal("request archive differs", err)
			}
			metaRaw, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
			if err != nil {
				t.Fatal(err)
			}
			var meta requestarchive.Metadata
			if json.Unmarshal(metaRaw, &meta) != nil {
				t.Fatal("invalid metadata")
			}
			if meta.RequestID != "req_20260922_000001" || *meta.ModelName != "actual-model" || *meta.InputTokens != 12 || *meta.OutputTokens != 4 || meta.Protocol != "OPENAI_RESPONSES" {
				t.Fatalf("metadata: %+v", meta)
			}
		})
	}
}
func TestRequestArchiveUpstreamScopeAndEndpointFiltering(t *testing.T) {
	for _, tt := range []struct {
		name, path, ct, body string
		scope                bool
	}{
		{"no scope", "/v1/responses", "application/json", `{}`, false},
		{"model discovery", "/v1/models", "application/json", `{}`, true},
		{"token count", "/v1/messages/count_tokens", "application/json", `{}`, true},
		{"binary", "/model/claude/invoke-with-response-stream", "application/vnd.amazon.eventstream", `{}`, true},
		{"background", "/v1/responses", "application/json", `{"background":true}`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			ctx := context.Background()
			if tt.scope {
				var scope *requestarchive.Scope
				ctx, scope = requestarchive.WithScope(ctx, requestarchive.Config{Root: root}, "", time.Now())
				defer scope.Finish()
			}
			req, _ := http.NewRequestWithContext(ctx, "POST", "https://example.test"+tt.path, strings.NewReader(tt.body))
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {tt.ct}}, Body: io.NopCloser(strings.NewReader(`{}`)), ContentLength: 2}
			original := resp.Body
			captureArchiveResponse(req, resp, &Account{Platform: PlatformOpenAI})
			if original != resp.Body {
				t.Fatal("out-of-scope response wrapped")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatal("out-of-scope archive created")
			}
		})
	}
}
func TestRequestArchivePlatformAndURLModels(t *testing.T) {
	tests := []struct{ raw, platform, kind, wantProtocol, wantPlatform, wantModel string }{
		{"https://api.deepseek.com/v1/chat/completions", PlatformOpenAI, AccountTypeAPIKey, "OPENAI", "deepseek", ""},
		{"https://host.openai.azure.com/openai/deployments/gpt-deploy/chat/completions", PlatformOpenAI, AccountTypeAPIKey, "OPENAI", "azure", "gpt-deploy"},
		{"https://aiplatform.googleapis.com/v1/projects/p/locations/global/publishers/anthropic/models/claude@20260921:streamRawPredict", PlatformAnthropic, AccountTypeServiceAccount, "ANTHROPIC", "vertex", "claude@20260921"},
		{"https://generativelanguage.googleapis.com/v1beta/models/gemini-flash:generateContent", PlatformGemini, AccountTypeAPIKey, "VERTEX", "ai_studio", "gemini-flash"},
		{"https://bedrock-runtime.us-east-1.amazonaws.com/model/anthropic.claude-v1:0/invoke", PlatformAnthropic, AccountTypeBedrock, "ANTHROPIC", "anthropic_on_aws", "anthropic.claude-v1:0"},
		{"https://api.example.test/v1/messages", PlatformZhipu, AccountTypeAPIKey, "ANTHROPIC", "glm", ""},
	}
	for _, tt := range tests {
		u, _ := url.Parse(tt.raw)
		a := &Account{Platform: tt.platform, Type: tt.kind}
		if p := archiveProtocol(u.Path); p != tt.wantProtocol {
			t.Errorf("protocol %s: %s", tt.raw, p)
		}
		if p := archivePlatform(a, u); p != tt.wantPlatform {
			t.Errorf("platform %s: %s", tt.raw, p)
		}
		if m := archiveURLModel(u); m != tt.wantModel {
			t.Errorf("model %s: %s", tt.raw, m)
		}
	}
}

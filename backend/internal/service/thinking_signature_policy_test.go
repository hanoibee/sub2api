package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

type thinkingPolicySettingRepoStub struct {
	value string
	err   error
	calls atomic.Int32
}

func (s *thinkingPolicySettingRepoStub) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get")
}
func (s *thinkingPolicySettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	s.calls.Add(1)
	if key != SettingKeyThinkingInvalidSignaturePolicy {
		panic("unexpected key: " + key)
	}
	if s.err != nil {
		return "", s.err
	}
	return s.value, nil
}
func (s *thinkingPolicySettingRepoStub) Set(context.Context, string, string) error {
	panic("unexpected Set")
}
func (s *thinkingPolicySettingRepoStub) GetMultiple(context.Context, []string) (map[string]string, error) {
	panic("unexpected GetMultiple")
}
func (s *thinkingPolicySettingRepoStub) SetMultiple(context.Context, map[string]string) error {
	panic("unexpected SetMultiple")
}
func (s *thinkingPolicySettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll")
}
func (s *thinkingPolicySettingRepoStub) Delete(context.Context, string) error {
	panic("unexpected Delete")
}

func resetThinkingInvalidSignaturePolicyCache(t *testing.T) {
	t.Helper()
	thinkingInvalidSignaturePolicySF.Forget(thinkingInvalidSignaturePolicySFKey)
	thinkingInvalidSignaturePolicyCache.Store((*cachedThinkingInvalidSignaturePolicy)(nil))
	t.Cleanup(func() {
		thinkingInvalidSignaturePolicySF.Forget(thinkingInvalidSignaturePolicySFKey)
		thinkingInvalidSignaturePolicyCache.Store((*cachedThinkingInvalidSignaturePolicy)(nil))
	})
}

func TestNormalizeThinkingInvalidSignaturePolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"  ", ""},
		{"strip", ThinkingInvalidSignaturePolicyStrip},
		{"STRIP", ThinkingInvalidSignaturePolicyStrip},
		{"reject", ThinkingInvalidSignaturePolicyReject},
		{"Reject", ThinkingInvalidSignaturePolicyReject},
		{"drop", ""},
		{"inherit", ""},
	}
	for _, tc := range cases {
		if got := NormalizeThinkingInvalidSignaturePolicy(tc.in); got != tc.want {
			t.Errorf("Normalize(%q)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseThinkingInvalidSignaturePolicyInput(t *testing.T) {
	t.Parallel()
	got, err := ParseThinkingInvalidSignaturePolicyInput("")
	if err != nil || got != "" {
		t.Fatalf("empty: got (%q, %v)", got, err)
	}
	got, err = ParseThinkingInvalidSignaturePolicyInput("reject")
	if err != nil || got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("reject: got (%q, %v)", got, err)
	}
	if _, err = ParseThinkingInvalidSignaturePolicyInput("nope"); err == nil {
		t.Fatal("expected error for invalid policy")
	}
}

func TestResolveThinkingInvalidSignaturePolicy(t *testing.T) {
	t.Parallel()
	if got := ResolveThinkingInvalidSignaturePolicy(nil, ""); got != ThinkingInvalidSignaturePolicyStrip {
		t.Fatalf("default=%q", got)
	}
	if got := ResolveThinkingInvalidSignaturePolicy(nil, "reject"); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("global reject=%q", got)
	}
	g := &Group{ThinkingInvalidSignaturePolicy: "strip"}
	if got := ResolveThinkingInvalidSignaturePolicy(g, "reject"); got != ThinkingInvalidSignaturePolicyStrip {
		t.Fatalf("group override strip=%q", got)
	}
	g.ThinkingInvalidSignaturePolicy = "reject"
	if got := ResolveThinkingInvalidSignaturePolicy(g, "strip"); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("group override reject=%q", got)
	}
	g.ThinkingInvalidSignaturePolicy = ""
	if got := ResolveThinkingInvalidSignaturePolicy(g, "reject"); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("inherit global=%q", got)
	}
}

func TestApplyThinkingInvalidSignaturePolicy_Matrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	invalidBody := []byte(`{
		"thinking":{"type":"adaptive"},
		"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x"},{"type":"text","text":"hi"}]}]
	}`)
	validBody := []byte(`{
		"thinking":{"type":"adaptive"},
		"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"ok","signature":"sig_real_123"},{"type":"text","text":"hi"}]}]
	}`)
	model := "claude-sonnet-4-5"

	s := &GatewayService{}

	t.Run("strip_invalid_strips", func(t *testing.T) {
		group := &Group{ThinkingInvalidSignaturePolicy: ThinkingInvalidSignaturePolicyStrip}
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, group, invalidBody, model)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(out, invalidBody) {
			t.Fatal("expected body to be stripped")
		}
		if !bytes.Contains(out, []byte(`"text"`)) {
			t.Fatalf("expected text preserved: %s", out)
		}
		if bytes.Contains(out, []byte(`"type":"thinking"`)) {
			t.Fatalf("expected thinking block removed: %s", out)
		}
	})

	t.Run("reject_invalid_400", func(t *testing.T) {
		group := &Group{ThinkingInvalidSignaturePolicy: ThinkingInvalidSignaturePolicyReject}
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), c, group, invalidBody, model)
		if !errors.Is(err, ErrThinkingInvalidSignature) {
			t.Fatalf("err=%v out=%s", err, out)
		}
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if !bytes.Contains(w.Body.Bytes(), []byte("invalid_request_error")) {
			t.Fatalf("body=%s", w.Body.String())
		}
	})

	t.Run("reject_valid_passthrough", func(t *testing.T) {
		group := &Group{ThinkingInvalidSignaturePolicy: ThinkingInvalidSignaturePolicyReject}
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, group, validBody, model)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, validBody) {
			t.Fatalf("expected unchanged body")
		}
	})

	t.Run("inherit_global_reject", func(t *testing.T) {
		// Without settingService, global defaults to strip; group inherit stays strip.
		group := &Group{ThinkingInvalidSignaturePolicy: ""}
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, group, invalidBody, model)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(out, invalidBody) {
			t.Fatal("default strip should modify body")
		}
	})

	t.Run("non_anthropic_strict_skips", func(t *testing.T) {
		group := &Group{ThinkingInvalidSignaturePolicy: ThinkingInvalidSignaturePolicyReject}
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, group, invalidBody, "deepseek-v4-pro")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, invalidBody) {
			t.Fatal("passback-required models must not filter")
		}
	})

	tamperedBody := []byte(`{
		"max_tokens": 4096,
		"messages": [
			{"content": "u1", "role": "user"},
			{"content": [{"signature": "CAISiQQKhwEIEBgCKkC0NRQg123@@", "thinking": "x", "type": "thinking"}], "role": "assistant"},
			{"content": "continue", "role": "user"}
		],
		"stream": false
	}`)

	t.Run("reject_tampered_signature_on_55_models", func(t *testing.T) {
		group := &Group{ThinkingInvalidSignaturePolicy: ThinkingInvalidSignaturePolicyReject}
		for _, model := range []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5-5", "claude-haiku-5"} {
			out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, group, tamperedBody, model)
			if !errors.Is(err, ErrThinkingInvalidSignature) {
				t.Fatalf("model=%s err=%v out=%s", model, err, out)
			}
		}
	})

	t.Run("strip_keeps_tampered_nonempty_on_55", func(t *testing.T) {
		group := &Group{ThinkingInvalidSignaturePolicy: ThinkingInvalidSignaturePolicyStrip}
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, group, tamperedBody, "claude-sonnet-5-5")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, tamperedBody) {
			t.Fatalf("strip must not rewrite 5.5 history with a non-empty signature: %s", out)
		}
	})
}

func TestGetThinkingInvalidSignaturePolicy_UsesProcessCache(t *testing.T) {
	resetThinkingInvalidSignaturePolicyCache(t)

	repo := &thinkingPolicySettingRepoStub{value: ThinkingInvalidSignaturePolicyReject}
	svc := NewSettingService(repo, &config.Config{})

	if got := svc.GetThinkingInvalidSignaturePolicy(context.Background()); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("first read: got %q", got)
	}
	if got := svc.GetThinkingInvalidSignaturePolicy(context.Background()); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("cached read: got %q", got)
	}
	if calls := repo.calls.Load(); calls != 1 {
		t.Fatalf("expected 1 DB call, got %d", calls)
	}

	storeThinkingInvalidSignaturePolicyCache(ThinkingInvalidSignaturePolicyStrip)
	if got := svc.GetThinkingInvalidSignaturePolicy(context.Background()); got != ThinkingInvalidSignaturePolicyStrip {
		t.Fatalf("after store: got %q", got)
	}
	if calls := repo.calls.Load(); calls != 1 {
		t.Fatalf("store should not hit DB, calls=%d", calls)
	}
}

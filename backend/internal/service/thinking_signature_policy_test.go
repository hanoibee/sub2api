package service

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

// thinkingPolicySettingRepoStub is an in-memory settings table. calls counts
// GetValue round-trips so tests can assert the process caches.
type thinkingPolicySettingRepoStub struct {
	mu     sync.Mutex
	values map[string]string
	err    error
	calls  atomic.Int32
}

func newThinkingPolicySettingRepoStub(values map[string]string) *thinkingPolicySettingRepoStub {
	if values == nil {
		values = map[string]string{}
	}
	return &thinkingPolicySettingRepoStub{values: values}
}

func (s *thinkingPolicySettingRepoStub) Get(context.Context, string) (*Setting, error) {
	panic("unexpected Get")
}
func (s *thinkingPolicySettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	s.calls.Add(1)
	if s.err != nil {
		return "", s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}
func (s *thinkingPolicySettingRepoStub) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = value
	return nil
}
func (s *thinkingPolicySettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := s.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}
func (s *thinkingPolicySettingRepoStub) SetMultiple(context.Context, map[string]string) error {
	panic("unexpected SetMultiple")
}
func (s *thinkingPolicySettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected GetAll")
}
func (s *thinkingPolicySettingRepoStub) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, key)
	return nil
}

func resetThinkingInvalidSignaturePolicyCache(t *testing.T) {
	t.Helper()
	reset := func() {
		thinkingInvalidSignaturePolicySF.Forget(thinkingInvalidSignaturePolicySFKey)
		thinkingInvalidSignaturePolicyCache.Store((*cachedThinkingInvalidSignaturePolicy)(nil))
		groupThinkingInvalidSignaturePolicyCache.Range(func(k, _ any) bool {
			groupThinkingInvalidSignaturePolicyCache.Delete(k)
			return true
		})
	}
	reset()
	t.Cleanup(reset)
}

// newThinkingPolicyGatewayService wires a GatewayService whose settings hold the
// given global policy ("" = unset) and per-group overrides.
func newThinkingPolicyGatewayService(t *testing.T, global string, groups map[int64]string) (*GatewayService, *thinkingPolicySettingRepoStub) {
	t.Helper()
	resetThinkingInvalidSignaturePolicyCache(t)
	values := map[string]string{}
	if global != "" {
		values[SettingKeyThinkingInvalidSignaturePolicy] = global
	}
	for id, p := range groups {
		values[thinkingInvalidSignaturePolicyGroupKey(id)] = p
	}
	repo := newThinkingPolicySettingRepoStub(values)
	return &GatewayService{settingService: NewSettingService(repo, &config.Config{})}, repo
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
	if got := ResolveThinkingInvalidSignaturePolicy("", ""); got != ThinkingInvalidSignaturePolicyStrip {
		t.Fatalf("default=%q", got)
	}
	if got := ResolveThinkingInvalidSignaturePolicy("", "reject"); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("global reject=%q", got)
	}
	if got := ResolveThinkingInvalidSignaturePolicy("strip", "reject"); got != ThinkingInvalidSignaturePolicyStrip {
		t.Fatalf("group override strip=%q", got)
	}
	if got := ResolveThinkingInvalidSignaturePolicy("reject", "strip"); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("group override reject=%q", got)
	}
	if got := ResolveThinkingInvalidSignaturePolicy("bogus", "reject"); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("invalid group value must inherit global=%q", got)
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

	const (
		stripGroup   int64 = 11
		rejectGroup  int64 = 12
		inheritGroup int64 = 13
	)
	overrides := map[int64]string{
		stripGroup:  ThinkingInvalidSignaturePolicyStrip,
		rejectGroup: ThinkingInvalidSignaturePolicyReject,
	}

	t.Run("strip_invalid_strips", func(t *testing.T) {
		s, _ := newThinkingPolicyGatewayService(t, "", overrides)
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, stripGroup, invalidBody, model)
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
		s, _ := newThinkingPolicyGatewayService(t, "", overrides)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), c, rejectGroup, invalidBody, model)
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
		s, _ := newThinkingPolicyGatewayService(t, "", overrides)
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, rejectGroup, validBody, model)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, validBody) {
			t.Fatalf("expected unchanged body")
		}
	})

	t.Run("no_setting_service_defaults_strip", func(t *testing.T) {
		resetThinkingInvalidSignaturePolicyCache(t)
		s := &GatewayService{}
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, rejectGroup, invalidBody, model)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(out, invalidBody) {
			t.Fatal("default strip should modify body")
		}
	})

	t.Run("inherit_global_reject", func(t *testing.T) {
		s, _ := newThinkingPolicyGatewayService(t, ThinkingInvalidSignaturePolicyReject, overrides)
		if _, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, inheritGroup, invalidBody, model); !errors.Is(err, ErrThinkingInvalidSignature) {
			t.Fatalf("group without override must follow global reject, err=%v", err)
		}
		if _, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, stripGroup, invalidBody, model); err != nil {
			t.Fatalf("group strip override must win over global reject, err=%v", err)
		}
	})

	t.Run("non_anthropic_strict_skips", func(t *testing.T) {
		s, repo := newThinkingPolicyGatewayService(t, "", overrides)
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, rejectGroup, invalidBody, "deepseek-v4-pro")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, invalidBody) {
			t.Fatal("passback-required models must not filter")
		}
		if calls := repo.calls.Load(); calls != 0 {
			t.Fatalf("non anthropic-strict models must not resolve policy, calls=%d", calls)
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
		s, _ := newThinkingPolicyGatewayService(t, "", overrides)
		for _, model := range []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5-5", "claude-haiku-5"} {
			out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, rejectGroup, tamperedBody, model)
			if !errors.Is(err, ErrThinkingInvalidSignature) {
				t.Fatalf("model=%s err=%v out=%s", model, err, out)
			}
		}
	})

	t.Run("strip_keeps_tampered_nonempty_on_55", func(t *testing.T) {
		s, _ := newThinkingPolicyGatewayService(t, "", overrides)
		out, err := s.applyThinkingInvalidSignaturePolicy(context.Background(), nil, stripGroup, tamperedBody, "claude-sonnet-5-5")
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, tamperedBody) {
			t.Fatalf("strip must not rewrite 5.5 history with a non-empty signature: %s", out)
		}
	})
}

func TestHasMalformedThinkingSignature(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"no_thinking", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"valid_base64", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"EqQBCkYI+/_-="}]}]}`, false},
		{"tampered_chars", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"abc123@@"}]}]}`, true},
		{"missing_signature", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x"}]}]}`, true},
		{"dummy_signature", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":"skip_thought_signature_validator"}]}]}`, true},
		{"numeric_signature", `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"x","signature":123}]}]}`, true},
		{"redacted_without_signature", `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]}]}`, false},
		{"redacted_null_signature", `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque","signature":null}]}]}`, true},
		{"redacted_tampered_signature", `{"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque","signature":"x@y"}]}]}`, true},
		{"string_content_mentions_thinking", `{"messages":[{"role":"user","content":"thinking about it"}]}`, false},
		{"later_message_tampered", `{"messages":[{"role":"assistant","content":[{"type":"thinking","signature":"ok"}]},{"role":"assistant","content":[{"type":"text","text":"t"},{"type":"thinking","signature":"bad!"}]}]}`, true},
		{"invalid_json", `{"messages":[{"type":"thinking"`, false},
	}
	for _, tc := range cases {
		if got := hasMalformedThinkingSignature([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestThinkingSignaturePolicyGroupID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set("api_key", &APIKey{Group: &Group{ID: 7}})

	if got := thinkingSignaturePolicyGroupID(c, nil); got != 7 {
		t.Fatalf("api key group fallback: got %d", got)
	}
	parsed := int64(9)
	if got := thinkingSignaturePolicyGroupID(c, &parsed); got != 9 {
		t.Fatalf("parsed group wins: got %d", got)
	}
	if got := thinkingSignaturePolicyGroupID(nil, nil); got != 0 {
		t.Fatalf("no group: got %d", got)
	}
}

func TestGroupThinkingInvalidSignaturePolicy_StoreAndCache(t *testing.T) {
	resetThinkingInvalidSignaturePolicyCache(t)
	repo := newThinkingPolicySettingRepoStub(map[string]string{
		thinkingInvalidSignaturePolicyGroupKey(1): ThinkingInvalidSignaturePolicyReject,
	})
	svc := NewSettingService(repo, &config.Config{})
	ctx := context.Background()

	if got := svc.GetGroupThinkingInvalidSignaturePolicy(ctx, 1); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("first read: %q", got)
	}
	if got := svc.GetGroupThinkingInvalidSignaturePolicy(ctx, 1); got != ThinkingInvalidSignaturePolicyReject {
		t.Fatalf("cached read: %q", got)
	}
	if got := svc.GetGroupThinkingInvalidSignaturePolicy(ctx, 2); got != "" {
		t.Fatalf("group without override must inherit, got %q", got)
	}
	if calls := repo.calls.Load(); calls != 2 {
		t.Fatalf("expected 2 DB reads (one per group), got %d", calls)
	}

	if err := svc.SetGroupThinkingInvalidSignaturePolicy(ctx, 2, "STRIP"); err != nil {
		t.Fatal(err)
	}
	if got := svc.GetGroupThinkingInvalidSignaturePolicy(ctx, 2); got != ThinkingInvalidSignaturePolicyStrip {
		t.Fatalf("after set: %q", got)
	}
	if err := svc.SetGroupThinkingInvalidSignaturePolicy(ctx, 1, ""); err != nil {
		t.Fatal(err)
	}
	if got := svc.GetGroupThinkingInvalidSignaturePolicy(ctx, 1); got != "" {
		t.Fatalf("after clear: %q", got)
	}
	if _, ok := repo.values[thinkingInvalidSignaturePolicyGroupKey(1)]; ok {
		t.Fatal("clearing an override must delete the settings row")
	}
	if calls := repo.calls.Load(); calls != 2 {
		t.Fatalf("set must refresh the cache without a DB read, calls=%d", calls)
	}
	if err := svc.SetGroupThinkingInvalidSignaturePolicy(ctx, 3, "drop"); err == nil {
		t.Fatal("expected error for invalid policy")
	}

	got, err := svc.GetGroupThinkingInvalidSignaturePolicies(ctx, []int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[2] != ThinkingInvalidSignaturePolicyStrip {
		t.Fatalf("batch read: %v", got)
	}
}

func TestGroupThinkingInvalidSignaturePolicy_FailOpen(t *testing.T) {
	resetThinkingInvalidSignaturePolicyCache(t)
	repo := newThinkingPolicySettingRepoStub(nil)
	repo.err = errors.New("db down")
	svc := NewSettingService(repo, &config.Config{})

	if got := svc.GetGroupThinkingInvalidSignaturePolicy(context.Background(), 5); got != "" {
		t.Fatalf("read error must fall back to inherit, got %q", got)
	}
}

func TestGetThinkingInvalidSignaturePolicy_UsesProcessCache(t *testing.T) {
	resetThinkingInvalidSignaturePolicyCache(t)

	repo := newThinkingPolicySettingRepoStub(map[string]string{
		SettingKeyThinkingInvalidSignaturePolicy: ThinkingInvalidSignaturePolicyReject,
	})
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

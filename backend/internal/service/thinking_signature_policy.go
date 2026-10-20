package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Thinking invalid-signature prefilter policies for Anthropic-strict models.
const (
	ThinkingInvalidSignaturePolicyStrip  = "strip"
	ThinkingInvalidSignaturePolicyReject = "reject"
)

// SettingKeyThinkingInvalidSignaturePolicy stores the global default policy
// (strip|reject). Empty / missing falls back to strip.
const SettingKeyThinkingInvalidSignaturePolicy = "thinking_invalid_signature_policy"

// Per-group overrides live in the settings table as one row per group
// ("thinking_invalid_signature_policy:group:<id>" = strip|reject), not as a
// groups column, so this fork feature stays out of ent / group_repo / the
// API-key auth snapshot that upstream changes frequently. A missing row means
// the group follows the global setting.
const settingKeyThinkingInvalidSignaturePolicyGroupPrefix = "thinking_invalid_signature_policy:group:"

func thinkingInvalidSignaturePolicyGroupKey(groupID int64) string {
	return settingKeyThinkingInvalidSignaturePolicyGroupPrefix + strconv.FormatInt(groupID, 10)
}

// cachedGroupThinkingInvalidSignaturePolicy caches one group's override
// (process-local, same TTLs as the global policy). policy "" = inherit.
type cachedGroupThinkingInvalidSignaturePolicy struct {
	policy    string
	expiresAt int64 // unix nano
}

var groupThinkingInvalidSignaturePolicyCache sync.Map // int64 -> *cachedGroupThinkingInvalidSignaturePolicy

// ErrThinkingInvalidSignature is returned when policy=reject and the body
// contains thinking blocks that would be stripped for missing/invalid signatures.
var ErrThinkingInvalidSignature = errors.New("thinking.signature: Field required")

// NormalizeThinkingInvalidSignaturePolicy validates and normalizes a policy value.
// Empty input returns "" (inherit). Invalid input returns "" as well so callers
// can fall through to the next layer safely.
func NormalizeThinkingInvalidSignaturePolicy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ThinkingInvalidSignaturePolicyStrip:
		return ThinkingInvalidSignaturePolicyStrip
	case ThinkingInvalidSignaturePolicyReject:
		return ThinkingInvalidSignaturePolicyReject
	default:
		return ""
	}
}

// ParseThinkingInvalidSignaturePolicyInput parses admin write input.
// Empty string means inherit (store ""). Invalid non-empty values error.
func ParseThinkingInvalidSignaturePolicyInput(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	if p := NormalizeThinkingInvalidSignaturePolicy(trimmed); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("thinking_invalid_signature_policy must be empty, strip, or reject")
}

// ResolveThinkingInvalidSignaturePolicy returns the effective policy.
// Priority: group override > global setting > strip.
func ResolveThinkingInvalidSignaturePolicy(groupPolicy, global string) string {
	if p := NormalizeThinkingInvalidSignaturePolicy(groupPolicy); p != "" {
		return p
	}
	if p := NormalizeThinkingInvalidSignaturePolicy(global); p != "" {
		return p
	}
	return ThinkingInvalidSignaturePolicyStrip
}

// GetThinkingInvalidSignaturePolicy returns the global default policy (strip|reject).
// Hot-path safe: process-local atomic cache (60s TTL) + singleflight; fail-open to strip.
func (s *SettingService) GetThinkingInvalidSignaturePolicy(ctx context.Context) string {
	if cached, ok := thinkingInvalidSignaturePolicyCache.Load().(*cachedThinkingInvalidSignaturePolicy); ok && cached != nil {
		if time.Now().UnixNano() < cached.expiresAt {
			return normalizeCachedThinkingInvalidSignaturePolicy(cached.policy)
		}
	}
	result, err, _ := thinkingInvalidSignaturePolicySF.Do(thinkingInvalidSignaturePolicySFKey, func() (any, error) {
		if cached, ok := thinkingInvalidSignaturePolicyCache.Load().(*cachedThinkingInvalidSignaturePolicy); ok && cached != nil {
			if time.Now().UnixNano() < cached.expiresAt {
				return normalizeCachedThinkingInvalidSignaturePolicy(cached.policy), nil
			}
		}
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), thinkingInvalidSignaturePolicyDBTimeout)
		defer cancel()
		value, err := s.settingRepo.GetValue(dbCtx, SettingKeyThinkingInvalidSignaturePolicy)
		if err != nil {
			if !errors.Is(err, ErrSettingNotFound) {
				slog.Warn("failed to get thinking_invalid_signature_policy, defaulting to strip", "error", err)
				thinkingInvalidSignaturePolicyCache.Store(&cachedThinkingInvalidSignaturePolicy{
					policy:    ThinkingInvalidSignaturePolicyStrip,
					expiresAt: time.Now().Add(thinkingInvalidSignaturePolicyErrorTTL).UnixNano(),
				})
				return ThinkingInvalidSignaturePolicyStrip, nil
			}
			value = ""
		}
		policy := normalizeCachedThinkingInvalidSignaturePolicy(value)
		thinkingInvalidSignaturePolicyCache.Store(&cachedThinkingInvalidSignaturePolicy{
			policy:    policy,
			expiresAt: time.Now().Add(thinkingInvalidSignaturePolicyCacheTTL).UnixNano(),
		})
		return policy, nil
	})
	if err != nil {
		return ThinkingInvalidSignaturePolicyStrip
	}
	if p, ok := result.(string); ok {
		return normalizeCachedThinkingInvalidSignaturePolicy(p)
	}
	return ThinkingInvalidSignaturePolicyStrip
}

func normalizeCachedThinkingInvalidSignaturePolicy(raw string) string {
	if p := NormalizeThinkingInvalidSignaturePolicy(raw); p != "" {
		return p
	}
	return ThinkingInvalidSignaturePolicyStrip
}

func storeThinkingInvalidSignaturePolicyCache(policy string) {
	thinkingInvalidSignaturePolicySF.Forget(thinkingInvalidSignaturePolicySFKey)
	thinkingInvalidSignaturePolicyCache.Store(&cachedThinkingInvalidSignaturePolicy{
		policy:    normalizeCachedThinkingInvalidSignaturePolicy(policy),
		expiresAt: time.Now().Add(thinkingInvalidSignaturePolicyCacheTTL).UnixNano(),
	})
}

// SetThinkingInvalidSignaturePolicy persists the global default policy.
func (s *SettingService) SetThinkingInvalidSignaturePolicy(ctx context.Context, policy string) error {
	p := NormalizeThinkingInvalidSignaturePolicy(policy)
	if p == "" {
		p = ThinkingInvalidSignaturePolicyStrip
	}
	if err := s.settingRepo.Set(ctx, SettingKeyThinkingInvalidSignaturePolicy, p); err != nil {
		return err
	}
	storeThinkingInvalidSignaturePolicyCache(p)
	return nil
}

// GetGroupThinkingInvalidSignaturePolicy returns a group's override
// (strip|reject), or "" when the group follows the global setting.
// Hot-path safe: per-group process cache (60s TTL) + singleflight; fail-open to "".
func (s *SettingService) GetGroupThinkingInvalidSignaturePolicy(ctx context.Context, groupID int64) string {
	if s == nil || s.settingRepo == nil || groupID <= 0 {
		return ""
	}
	if v, ok := groupThinkingInvalidSignaturePolicyCache.Load(groupID); ok {
		if cached := v.(*cachedGroupThinkingInvalidSignaturePolicy); time.Now().UnixNano() < cached.expiresAt {
			return cached.policy
		}
	}
	key := thinkingInvalidSignaturePolicyGroupKey(groupID)
	result, _, _ := thinkingInvalidSignaturePolicySF.Do(key, func() (any, error) {
		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), thinkingInvalidSignaturePolicyDBTimeout)
		defer cancel()
		value, err := s.settingRepo.GetValue(dbCtx, key)
		ttl := thinkingInvalidSignaturePolicyCacheTTL
		if err != nil {
			value = ""
			if !errors.Is(err, ErrSettingNotFound) {
				slog.Warn("failed to get group thinking_invalid_signature_policy, following global", "group_id", groupID, "error", err)
				ttl = thinkingInvalidSignaturePolicyErrorTTL
			}
		}
		policy := NormalizeThinkingInvalidSignaturePolicy(value)
		groupThinkingInvalidSignaturePolicyCache.Store(groupID, &cachedGroupThinkingInvalidSignaturePolicy{
			policy:    policy,
			expiresAt: time.Now().Add(ttl).UnixNano(),
		})
		return policy, nil
	})
	p, _ := result.(string)
	return p
}

// GetGroupThinkingInvalidSignaturePolicies batch-loads overrides for admin
// views (uncached). Groups without an override are absent from the map.
func (s *SettingService) GetGroupThinkingInvalidSignaturePolicies(ctx context.Context, groupIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(groupIDs))
	if s == nil || s.settingRepo == nil || len(groupIDs) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(groupIDs))
	byKey := make(map[string]int64, len(groupIDs))
	for _, id := range groupIDs {
		if id <= 0 {
			continue
		}
		key := thinkingInvalidSignaturePolicyGroupKey(id)
		keys = append(keys, key)
		byKey[key] = id
	}
	values, err := s.settingRepo.GetMultiple(ctx, keys)
	if err != nil {
		return nil, err
	}
	for key, value := range values {
		if p := NormalizeThinkingInvalidSignaturePolicy(value); p != "" {
			out[byKey[key]] = p
		}
	}
	return out, nil
}

// SetGroupThinkingInvalidSignaturePolicy persists a group's override.
// "" removes the override so the group follows the global setting.
// Other instances pick the change up within the cache TTL.
func (s *SettingService) SetGroupThinkingInvalidSignaturePolicy(ctx context.Context, groupID int64, policy string) error {
	if s == nil || s.settingRepo == nil || groupID <= 0 {
		return nil
	}
	p, err := ParseThinkingInvalidSignaturePolicyInput(policy)
	if err != nil {
		return err
	}
	key := thinkingInvalidSignaturePolicyGroupKey(groupID)
	if p == "" {
		err = s.settingRepo.Delete(ctx, key)
	} else {
		err = s.settingRepo.Set(ctx, key, p)
	}
	if err != nil {
		return err
	}
	thinkingInvalidSignaturePolicySF.Forget(key)
	groupThinkingInvalidSignaturePolicyCache.Store(groupID, &cachedGroupThinkingInvalidSignaturePolicy{
		policy:    p,
		expiresAt: time.Now().Add(thinkingInvalidSignaturePolicyCacheTTL).UnixNano(),
	})
	return nil
}

// ResolveThinkingInvalidSignaturePolicyForRequest resolves policy for a gateway
// request from the group override (when groupID > 0) and the global setting.
func (s *GatewayService) ResolveThinkingInvalidSignaturePolicyForRequest(ctx context.Context, groupID int64) string {
	if s == nil || s.settingService == nil {
		return ThinkingInvalidSignaturePolicyStrip
	}
	if p := s.settingService.GetGroupThinkingInvalidSignaturePolicy(ctx, groupID); p != "" {
		return p
	}
	return ResolveThinkingInvalidSignaturePolicy("", s.settingService.GetThinkingInvalidSignaturePolicy(ctx))
}

// thinkingSignaturePolicyGroupID picks the group whose override applies:
// the parsed request group when set, else the authenticated API key's group.
func thinkingSignaturePolicyGroupID(c *gin.Context, groupID *int64) int64 {
	if groupID != nil && *groupID > 0 {
		return *groupID
	}
	if c != nil {
		if v, ok := c.Get("api_key"); ok {
			if apiKey, ok := v.(*APIKey); ok && apiKey != nil && apiKey.Group != nil {
				return apiKey.Group.ID
			}
		}
	}
	return 0
}

// applyThinkingInvalidSignaturePolicy applies strip|reject for Anthropic-strict
// models. On reject with invalid signatures it writes an Anthropic-style 400 JSON
// body (when c is non-nil) and returns ErrThinkingInvalidSignature.
//
// strip deliberately keeps non-empty signatures that merely look malformed:
// strip means "make the request go through", and on 5.5 any stripped block
// drops the whole signed thinking history. Groups that must not forward
// tampered signatures should use reject.
func (s *GatewayService) applyThinkingInvalidSignaturePolicy(
	ctx context.Context,
	c *gin.Context,
	groupID int64,
	body []byte,
	mappedModel string,
) ([]byte, error) {
	if !ShouldPreFilterThinkingBlocks(mappedModel) {
		return body, nil
	}
	policy := s.ResolveThinkingInvalidSignaturePolicyForRequest(ctx, groupID)
	if policy == ThinkingInvalidSignaturePolicyReject {
		if ShouldRejectInvalidThinkingSignature(body, mappedModel) {
			if c != nil {
				c.JSON(http.StatusBadRequest, gin.H{
					"type": "error",
					"error": gin.H{
						"type":    "invalid_request_error",
						"message": ErrThinkingInvalidSignature.Error(),
					},
				})
			}
			return nil, ErrThinkingInvalidSignature
		}
		return body, nil
	}
	return FilterThinkingBlocks(body, mappedModel), nil
}

// ShouldRejectInvalidThinkingSignature reports whether reject policy should
// locally 400. This is broader than WouldFilterThinkingBlocks:
//
//   - Missing / empty / dummy signatures still reject (same as strip-would-modify).
//   - Claude 5.5 defaults to keeping any non-empty signature so that a continuous
//     thinking history can round-trip. Probe cases tamper the payload with
//     characters Anthropic never emits (e.g. trailing "123@@"). Those survive
//     FilterThinkingBlocks and must be rejected here, not forwarded.
//
// Limitation: signatures are opaque and can only be verified by the upstream,
// so this is a format check, not an integrity check. A signature tampered with
// only base64 characters (e.g. appended "AAAA") passes and is forwarded.
func ShouldRejectInvalidThinkingSignature(body []byte, mappedModel string) bool {
	if !ShouldPreFilterThinkingBlocks(mappedModel) {
		return false
	}
	if WouldFilterThinkingBlocks(body, mappedModel) {
		return true
	}
	return hasMalformedThinkingSignature(body)
}

func hasMalformedThinkingSignature(body []byte) bool {
	if !bytes.Contains(body, []byte("thinking")) {
		return false
	}
	malformed := false
	gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
		msg.Get("content").ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "thinking":
				malformed = !isPlausibleThinkingSignatureValue(block.Get("signature"))
			case "redacted_thinking":
				// 5.5 keeps redacted blocks with non-empty data and no signature.
				// Only reject when an explicit signature is present and malformed.
				if sig := block.Get("signature"); sig.Exists() {
					malformed = !isPlausibleThinkingSignatureValue(sig)
				}
			}
			return !malformed
		})
		return !malformed
	})
	return malformed
}

// isPlausibleThinkingSignatureValue treats non-string signatures (null,
// numbers, objects) as malformed.
func isPlausibleThinkingSignatureValue(sig gjson.Result) bool {
	return sig.Type == gjson.String && isPlausibleAnthropicThinkingSignature(sig.Str)
}

// isPlausibleAnthropicThinkingSignature only checks the alphabet (standard or
// URL-safe base64). There is deliberately no length or structure check:
// third-party Anthropic-compatible upstreams and existing fixtures use short
// opaque signatures, and a false positive here turns into a client 400.
func isPlausibleAnthropicThinkingSignature(signature string) bool {
	if signature == "" || signature == antigravity.DummyThoughtSignature {
		return false
	}
	for i := 0; i < len(signature); i++ {
		c := signature[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '+', c == '/', c == '=', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

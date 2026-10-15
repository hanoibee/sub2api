package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Thinking invalid-signature prefilter policies for Anthropic-strict models.
const (
	ThinkingInvalidSignaturePolicyStrip  = "strip"
	ThinkingInvalidSignaturePolicyReject = "reject"
)

// SettingKeyThinkingInvalidSignaturePolicy stores the global default policy
// (strip|reject). Empty / missing falls back to strip.
const SettingKeyThinkingInvalidSignaturePolicy = "thinking_invalid_signature_policy"

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
func ResolveThinkingInvalidSignaturePolicy(group *Group, global string) string {
	if group != nil {
		if p := NormalizeThinkingInvalidSignaturePolicy(group.ThinkingInvalidSignaturePolicy); p != "" {
			return p
		}
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

// ResolveThinkingInvalidSignaturePolicyForRequest resolves policy for a gateway
// request using the group from context (when present) and the global setting.
func (s *GatewayService) ResolveThinkingInvalidSignaturePolicyForRequest(ctx context.Context, group *Group) string {
	global := ThinkingInvalidSignaturePolicyStrip
	if s != nil && s.settingService != nil {
		if v := s.settingService.GetThinkingInvalidSignaturePolicy(ctx); v != "" {
			global = v
		}
	}
	return ResolveThinkingInvalidSignaturePolicy(group, global)
}

// groupForThinkingSignaturePolicy resolves the request group for policy lookup.
// Prefers the authenticated API key group (auth-cache hot path); falls back to
// groupRepo by parsed.GroupID when the cached group is missing or mismatched.
func (s *GatewayService) groupForThinkingSignaturePolicy(ctx context.Context, c *gin.Context, groupID *int64) *Group {
	if c != nil {
		if v, ok := c.Get("api_key"); ok {
			if apiKey, ok := v.(*APIKey); ok && apiKey != nil && apiKey.Group != nil {
				if groupID == nil || *groupID == 0 || apiKey.Group.ID == *groupID {
					return apiKey.Group
				}
			}
		}
	}
	if groupID != nil && *groupID > 0 && s != nil && s.groupRepo != nil {
		if g, err := s.groupRepo.GetByIDLite(ctx, *groupID); err == nil {
			return g
		}
	}
	return nil
}

// applyThinkingInvalidSignaturePolicy applies strip|reject for Anthropic-strict
// models. On reject with invalid signatures it writes an Anthropic-style 400 JSON
// body (when c is non-nil) and returns ErrThinkingInvalidSignature.
func (s *GatewayService) applyThinkingInvalidSignaturePolicy(
	ctx context.Context,
	c *gin.Context,
	group *Group,
	body []byte,
	mappedModel string,
) ([]byte, error) {
	policy := s.ResolveThinkingInvalidSignaturePolicyForRequest(ctx, group)
	if policy == ThinkingInvalidSignaturePolicyReject {
		if WouldFilterThinkingBlocks(body, mappedModel) {
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

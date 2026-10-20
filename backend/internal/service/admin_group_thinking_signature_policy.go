package service

import (
	"context"
	"log/slog"
)

// [fork] Admin-side plumbing for per-group thinking.signature overrides.
// Overrides are stored in settings (see GetGroupThinkingInvalidSignaturePolicy),
// so Group.ThinkingInvalidSignaturePolicy is only filled in here for admin views
// and persisted here after the group row itself is written.

// overlayGroupThinkingSignaturePolicies fills Group.ThinkingInvalidSignaturePolicy
// from settings. A read failure leaves the field empty (shown as "follow global").
func (s *adminServiceImpl) overlayGroupThinkingSignaturePolicies(ctx context.Context, groups ...*Group) {
	if s == nil || s.settingService == nil || len(groups) == 0 {
		return
	}
	ids := make([]int64, 0, len(groups))
	for _, g := range groups {
		if g != nil {
			ids = append(ids, g.ID)
		}
	}
	policies, err := s.settingService.GetGroupThinkingInvalidSignaturePolicies(ctx, ids)
	if err != nil {
		slog.Warn("failed to load group thinking_invalid_signature_policy overrides", "error", err)
		return
	}
	for _, g := range groups {
		if g != nil {
			g.ThinkingInvalidSignaturePolicy = policies[g.ID]
		}
	}
}

func (s *adminServiceImpl) overlayGroupSliceThinkingSignaturePolicies(ctx context.Context, groups []Group) {
	ptrs := make([]*Group, len(groups))
	for i := range groups {
		ptrs[i] = &groups[i]
	}
	s.overlayGroupThinkingSignaturePolicies(ctx, ptrs...)
}

// saveGroupThinkingSignaturePolicy persists group.ThinkingInvalidSignaturePolicy
// after the group row has been written.
func (s *adminServiceImpl) saveGroupThinkingSignaturePolicy(ctx context.Context, group *Group) error {
	if s == nil || s.settingService == nil || group == nil {
		return nil
	}
	return s.settingService.SetGroupThinkingInvalidSignaturePolicy(ctx, group.ID, group.ThinkingInvalidSignaturePolicy)
}

// saveCreatedGroupThinkingSignaturePolicy persists the override of a freshly
// created group. The group already exists, so a failure must not be reported as
// a create failure (the admin would retry and create a duplicate); it is logged
// and the response shows the real state (follow global).
func (s *adminServiceImpl) saveCreatedGroupThinkingSignaturePolicy(ctx context.Context, group *Group) {
	if err := s.saveGroupThinkingSignaturePolicy(ctx, group); err != nil {
		slog.Error("failed to save group thinking_invalid_signature_policy", "group_id", group.ID, "error", err)
		group.ThinkingInvalidSignaturePolicy = ""
	}
}

// copyGroupThinkingSignaturePolicy carries the source group's override over to
// a duplicate. Same failure handling as saveCreatedGroupThinkingSignaturePolicy.
func (s *adminServiceImpl) copyGroupThinkingSignaturePolicy(ctx context.Context, sourceID int64, duplicate *Group) {
	if s == nil || s.settingService == nil || duplicate == nil {
		return
	}
	// Uncached read: the hot-path cache may lag another instance's edit.
	policies, err := s.settingService.GetGroupThinkingInvalidSignaturePolicies(ctx, []int64{sourceID})
	if err != nil {
		slog.Error("failed to load source group thinking_invalid_signature_policy", "group_id", sourceID, "error", err)
		return
	}
	duplicate.ThinkingInvalidSignaturePolicy = policies[sourceID]
	if duplicate.ThinkingInvalidSignaturePolicy != "" {
		s.saveCreatedGroupThinkingSignaturePolicy(ctx, duplicate)
	}
}

// deleteGroupThinkingSignaturePolicy drops a deleted group's override. Best
// effort: a leftover row is harmless because group IDs are never reused.
func (s *adminServiceImpl) deleteGroupThinkingSignaturePolicy(ctx context.Context, groupID int64) {
	if s == nil || s.settingService == nil {
		return
	}
	if err := s.settingService.SetGroupThinkingInvalidSignaturePolicy(ctx, groupID, ""); err != nil {
		slog.Warn("failed to delete group thinking_invalid_signature_policy override", "group_id", groupID, "error", err)
	}
}

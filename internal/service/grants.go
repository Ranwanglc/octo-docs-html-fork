package service

import (
	"context"
	"log/slog"
	"maps"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// grantRoleReader is the only role a per-uid grant may carry today. Editing
// (writer) needs a new Capability tier + a full pass over the resolution chain,
// so grants are read+comment only for now.
const grantRoleReader = "reader"

// ListGrants returns the uid→role map of explicit grants for a slug (empty when
// none). A missing doc is NotFound so callers can hide non-existent docs.
func (s *AuthService) ListGrants(ctx context.Context, slug string) (map[string]string, error) {
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, apperr.NotFound("no such doc: " + slug)
	}
	out := map[string]string{}
	grants, ok := meta.Extra[storage.GrantsExtraKey].(map[string]any)
	if !ok {
		return out, nil
	}
	for uid, v := range grants {
		if entry, ok := v.(map[string]any); ok {
			if role, ok := entry["role"].(string); ok {
				out[uid] = role
			}
		}
	}
	return out, nil
}

// AddGrant grants uid a role on slug (upsert). grantedBy records who authorized
// it. Only the reader role is accepted for now.
//
// TODO: verify uid is a real octo user (anti ghost-member) once octo-server
// exposes a uid-existence lookup the doc can call; today any uid is accepted.
func (s *AuthService) AddGrant(ctx context.Context, slug, uid, role, grantedBy string) error {
	if uid == "" {
		return apperr.Validation("uid required", "invalid_grant")
	}
	if role != grantRoleReader {
		return apperr.Validation("role must be reader", "invalid_grant")
	}
	if err := s.lock.With(ctx, slug, func() error {
		meta, gerr := s.meta.GetMeta(ctx, slug)
		if gerr != nil {
			return gerr
		}
		if meta == nil {
			return apperr.NotFound("no such doc: " + slug)
		}
		extra := map[string]any{}
		maps.Copy(extra, meta.Extra)
		grants := map[string]any{}
		if existing, ok := extra[storage.GrantsExtraKey].(map[string]any); ok {
			maps.Copy(grants, existing)
		}
		grants[uid] = map[string]any{
			"role":       role,
			"granted_by": grantedBy,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		extra[storage.GrantsExtraKey] = grants
		return s.meta.PutMeta(ctx, slug, storage.DocMeta{
			Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra,
		})
	}); err != nil {
		return err
	}
	s.mirrorGrantUpsert(ctx, slug, uid, grantedBy)
	return nil
}

// RemoveGrant revokes uid's grant on slug. Removing an absent uid is a no-op
// (idempotent). The grants key itself is dropped once empty.
func (s *AuthService) RemoveGrant(ctx context.Context, slug, uid string) error {
	deletedGrant := false
	if err := s.lock.With(ctx, slug, func() error {
		meta, gerr := s.meta.GetMeta(ctx, slug)
		if gerr != nil {
			return gerr
		}
		if meta == nil {
			return apperr.NotFound("no such doc: " + slug)
		}
		existing, ok := meta.Extra[storage.GrantsExtraKey].(map[string]any)
		if !ok {
			return nil
		}
		if _, has := existing[uid]; !has {
			return nil
		}
		deletedGrant = true
		extra := map[string]any{}
		maps.Copy(extra, meta.Extra)
		grants := map[string]any{}
		for k, v := range existing {
			if k != uid {
				grants[k] = v
			}
		}
		if len(grants) == 0 {
			delete(extra, storage.GrantsExtraKey)
		} else {
			extra[storage.GrantsExtraKey] = grants
		}
		return s.meta.PutMeta(ctx, slug, storage.DocMeta{
			Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra,
		})
	}); err != nil {
		return err
	}
	// Mirror only on a real grant removal — avoid an empty permission_epoch bump.
	if deletedGrant {
		s.mirrorGrantDelete(ctx, slug, uid)
	}
	return nil
}

func (s *AuthService) mirrorGrantUpsert(ctx context.Context, slug, uid, grantedBy string) {
	if s.docMembers == nil {
		return
	}
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug)
	if err != nil {
		slog.Default().Debug("doc_member mirror resolve failed", "slug", slug, "uid", uid, "err", err.Error())
		return
	}
	if !ok {
		slog.Default().Debug("doc_member mirror skipped: doc_meta missing", "slug", slug, "uid", uid)
		return
	}
	if err := s.docMembers.UpsertDirectGrant(ctx, docID, uid, docMemberRoleReader, grantedBy); err != nil {
		slog.Default().Debug("doc_member mirror upsert failed", "slug", slug, "uid", uid, "err", err.Error())
	}
}

func (s *AuthService) mirrorGrantDelete(ctx context.Context, slug, uid string) {
	if s.docMembers == nil {
		return
	}
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug)
	if err != nil {
		slog.Default().Debug("doc_member mirror resolve failed", "slug", slug, "uid", uid, "err", err.Error())
		return
	}
	if !ok {
		slog.Default().Debug("doc_member mirror skipped: doc_meta missing", "slug", slug, "uid", uid)
		return
	}
	if err := s.docMembers.DeleteGrant(ctx, docID, uid); err != nil {
		slog.Default().Debug("doc_member mirror delete failed", "slug", slug, "uid", uid, "err", err.Error())
	}
}

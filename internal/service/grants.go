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

// ErrGrantProtected is returned by AddGrant / RemoveGrant when the target uid
// is the doc's creator or a doc_member admin — those rows must never be
// revoked or downgraded through the grants API (that path is reader-scoped).
//
// yujiawei P2-A: this is an *apperr.Error so writeErr surfaces a 409 instead
// of collapsing to 500 through the errors.As(*apperr.Error) fallthrough.
// Callers still use errors.Is(err, ErrGrantProtected); pointer identity is
// preserved because the sentinel is a single package-level *apperr.Error.
var ErrGrantProtected = apperr.Conflict("grant protected: creator or admin cannot be revoked", "grant_protected")

// ListGrants returns the uid→role map of explicit grants for a slug (empty when
// none). A missing doc is NotFound so callers can hide non-existent docs.
//
// Plan③ A6: when a doc_member mirror is wired, the authoritative source is
// doc_member — every direct grant (reader) and every admin (creator/owner
// backfill via M1) lives there, and meta.grants is now write-frozen (see
// AddGrant). The creator row is always surfaced from meta.creator_uid so the
// UI's "created by" row survives even when doc_member has no explicit admin
// row for the creator yet (belt-and-suspenders vs. M1 gaps); if doc_member
// already carries an admin row for the same uid, that row wins — we do not
// duplicate.
//
// When no mirror is wired (single-node deploys, in-memory tests) ListGrants
// falls back to reading meta.grants, matching the pre-plan③ behaviour those
// environments still rely on.
func (s *AuthService) ListGrants(ctx context.Context, slug string) (map[string]string, error) {
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, apperr.NotFound("no such doc: " + slug)
	}
	if s.docMembers == nil {
		return legacyListGrantsFromMeta(meta), nil
	}
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No rich-doc row yet; fall back to meta so ListGrants stays useful
		// during the moment between publish and mirror registration.
		return legacyListGrantsFromMeta(meta), nil
	}
	members, err := s.docMembers.ListMembers(ctx, docID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, m := range members {
		out[m.UID] = roleCodeToLabel(m.Role)
	}
	// Surface creator_uid as admin so callers still see the author even when
	// M1 has not landed for this doc. Skip when doc_member already has a row
	// for the same uid — that row is authoritative.
	if creator := meta.CreatorUID(); creator != "" {
		if _, exists := out[creator]; !exists {
			out[creator] = roleCodeToLabel(DocMemberRoleAdmin)
		}
	}
	return out, nil
}

// legacyListGrantsFromMeta reads the pre-plan③ meta.grants map. Used only in
// the mirror-unwired fallback path so single-node deploys keep working.
func legacyListGrantsFromMeta(meta *storage.DocMeta) map[string]string {
	out := map[string]string{}
	grants, ok := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
	if !ok {
		return out
	}
	for uid, v := range grants {
		if entry, ok := v.(map[string]any); ok {
			if role, ok := entry["role"].(string); ok {
				out[uid] = role
			}
		}
	}
	return out
}

// roleCodeToLabel translates rich-doc doc_member.role integers to the string
// labels this API has always returned. Only reader is used today; admin is
// added so the creator row from ListGrants renders correctly.
func roleCodeToLabel(role int) string {
	switch role {
	case DocMemberRoleAdmin:
		return "admin"
	case DocMemberRoleReader:
		return grantRoleReader
	default:
		return grantRoleReader
	}
}

// AddGrant grants uid a role on slug (upsert). grantedBy records who authorized
// it. Only the reader role is accepted for now.
//
// Plan③ A6: doc_member is authoritative — the upsert goes straight through
// UpsertDirectGrant (source=1 direct grant, encoded inside the mirror impl).
// meta.grants is no longer written. When no mirror is wired we still write
// meta.grants so single-node deploys keep working.
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
	if s.docMembers != nil {
		return s.addGrantToDocMember(ctx, slug, uid, grantedBy)
	}
	return s.addGrantToMeta(ctx, slug, uid, role, grantedBy)
}

// addGrantToDocMember is the plan③ A6 primary path. UpsertDirectGrant is
// idempotent — repeated calls for the same (docID,uid,role) update
// updated_at only, no duplicate row.
//
// yujiawei P1-B: probe RoleByDocUID first and refuse reader-grants that would
// silently downgrade an admin (role=3) or the creator uid. UpsertDirectGrant
// runs ON DUPLICATE KEY UPDATE role=VALUES(role), so a naive reader upsert on
// an existing admin row would clobber it — and once A1 flips creator_uid to
// the bot, the owner's author is nothing but their doc_member admin row.
// One reader grant would demote them. Idempotent reader→reader is a no-op
// (no permission_epoch bump).
func (s *AuthService) addGrantToDocMember(ctx context.Context, slug, uid, grantedBy string) error {
	// Existence check via meta so we still 404 on a bogus slug (rich-doc
	// mirror only knows registered docs).
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return err
	}
	if meta == nil {
		return apperr.NotFound("no such doc: " + slug)
	}
	if creator := meta.CreatorUID(); creator != "" && creator == uid {
		return ErrGrantProtected
	}
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug)
	if err != nil {
		return err
	}
	if !ok {
		return apperr.NotFound("doc has no rich-doc registration yet: " + slug)
	}
	role, ok, err := s.docMembers.RoleByDocUID(ctx, docID, uid)
	if err != nil {
		return err
	}
	if ok {
		if role == DocMemberRoleAdmin {
			return ErrGrantProtected
		}
		if role >= DocMemberRoleReader {
			return nil // already reader (or higher-that-is-not-admin); no-op, no epoch bump
		}
	}
	return s.docMembers.UpsertDirectGrant(ctx, docID, uid, DocMemberRoleReader, grantedBy)
}

// addGrantToMeta preserves the pre-plan③ meta.grants write path for the
// mirror-unwired fallback (single-node deploys, in-memory tests). This is the
// only place we still author meta.grants; production reads never see it once
// A4 lands (bestCred consults doc_member first).
func (s *AuthService) addGrantToMeta(ctx context.Context, slug, uid, role, grantedBy string) error {
	return s.lock.With(ctx, slug, func() error {
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
		if existing, ok := extra[storage.GrantsExtraKey].(map[string]any); ok { //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
			maps.Copy(grants, existing)
		}
		grants[uid] = map[string]any{
			"role":       role,
			"granted_by": grantedBy,
			"created_at": time.Now().UTC().Format(time.RFC3339),
		}
		extra[storage.GrantsExtraKey] = grants //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
		return s.meta.PutMeta(ctx, slug, storage.DocMeta{
			Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra,
		})
	})
}

// RemoveGrant revokes uid's grant on slug. Removing an absent uid is a no-op
// (idempotent).
//
// Plan③ A6 protection: refuses to revoke the doc's creator_uid or any
// doc_member admin row — those are the author identities and the grants API
// (reader-only) has no authority over them. Callers see ErrGrantProtected
// and must go through the identity/admin path instead.
func (s *AuthService) RemoveGrant(ctx context.Context, slug, uid string) error {
	if uid == "" {
		return apperr.Validation("uid required", "invalid_grant")
	}
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return err
	}
	if meta == nil {
		return apperr.NotFound("no such doc: " + slug)
	}
	if creator := meta.CreatorUID(); creator != "" && creator == uid {
		return ErrGrantProtected
	}
	if s.docMembers != nil {
		return s.removeGrantFromDocMember(ctx, slug, uid)
	}
	return s.removeGrantFromMeta(ctx, slug, uid)
}

func (s *AuthService) removeGrantFromDocMember(ctx context.Context, slug, uid string) error {
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug)
	if err != nil {
		return err
	}
	if !ok {
		return nil // no rich-doc row yet, nothing to revoke
	}
	// Probe first so an absent uid is a true no-op (no wasted DELETE, no
	// permission_epoch bump) and admin rows are refused before DELETE runs.
	role, ok, err := s.docMembers.RoleByDocUID(ctx, docID, uid)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if role == DocMemberRoleAdmin {
		return ErrGrantProtected
	}
	return s.docMembers.DeleteGrant(ctx, docID, uid)
}

func (s *AuthService) removeGrantFromMeta(ctx context.Context, slug, uid string) error {
	return s.lock.With(ctx, slug, func() error {
		meta, gerr := s.meta.GetMeta(ctx, slug)
		if gerr != nil {
			return gerr
		}
		if meta == nil {
			return apperr.NotFound("no such doc: " + slug)
		}
		existing, ok := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
		if !ok {
			return nil
		}
		if _, has := existing[uid]; !has {
			return nil
		}
		extra := map[string]any{}
		maps.Copy(extra, meta.Extra)
		grants := map[string]any{}
		for k, v := range existing {
			if k != uid {
				grants[k] = v
			}
		}
		if len(grants) == 0 {
			delete(extra, storage.GrantsExtraKey) //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
		} else {
			extra[storage.GrantsExtraKey] = grants //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
		}
		return s.meta.PutMeta(ctx, slug, storage.DocMeta{
			Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra,
		})
	})
}

// mirrorGrantUpsert / mirrorGrantDelete: Deprecated after plan③ A6 —
// AddGrant/RemoveGrant now talk to doc_member directly. Kept as thin wrappers
// so any external caller still compiles; both now just log at debug when the
// mirror is nil and behave as no-ops. Marked for removal once callers are
// gone (A7 cleanup pass).
//
// Deprecated: use AddGrant/RemoveGrant which handle doc_member natively.
//
//nolint:unused // Retained per plan③ scope: A7 cleanup pass removes these.
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
	if err := s.docMembers.UpsertDirectGrant(ctx, docID, uid, DocMemberRoleReader, grantedBy); err != nil {
		slog.Default().Debug("doc_member mirror upsert failed", "slug", slug, "uid", uid, "err", err.Error())
	}
}

// Deprecated: use RemoveGrant which handles doc_member natively.
//
//nolint:unused // Retained per plan③ scope: A7 cleanup pass removes these.
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

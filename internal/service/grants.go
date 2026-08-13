package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// grant role labels accepted/emitted by the HTML /grants API. They map 1:1 to
// the doc_member.role encoding via roleLabelToCode / roleCodeToLabel so the HTML
// grants path and the rich-doc doc_member table share one four-role vocabulary
// (no second permission fact).
const (
	grantRoleReader    = "reader"
	grantRoleCommenter = "commenter"
	grantRoleWriter    = "writer"
	grantRoleAdmin     = "admin"
)

// ErrGrantProtected is returned by AddGrant / RemoveGrant when the target uid
// is the doc's creator or a doc_member admin — those rows must never be
// revoked or downgraded through the grants API (that path is reader-scoped).
//
// This is an *apperr.Error so writeErr surfaces a 409 instead
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
		return legacyListGrantsFromMeta(meta, meta.CreatorUID()), nil
	}
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug, spaceIDForDoc(ctx, meta))
	if err != nil {
		return nil, err
	}
	if !ok {
		// No rich-doc row yet; fall back to meta so ListGrants stays useful
		// during the moment between publish and mirror registration.
		return legacyListGrantsFromMeta(meta, meta.CreatorUID()), nil
	}
	members, err := s.docMembers.ListMembers(ctx, docID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	creator := meta.CreatorUID()
	for _, m := range members {
		if creator != "" && m.UID == creator {
			continue // handler synthesises the creator row (P2-B)
		}
		label := roleCodeToLabel(m.Role)
		if label == "" {
			continue // unknown/corrupt stored role: do not expose a synthetic grant
		}
		out[m.UID] = label
	}
	return out, nil
}

// legacyListGrantsFromMeta reads the pre-plan③ meta.grants map. Used only in
// the mirror-unwired fallback path so single-node deploys keep working.
//
// Skip the creator uid so the caller (handler
// synthesises the "author"/"owner" row) does not receive a duplicate row on
// the unwired path — mirrors the wired-side dedup in ListGrants above.
func legacyListGrantsFromMeta(meta *storage.DocMeta, creator string) map[string]string {
	out := map[string]string{}
	grants, ok := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
	if !ok {
		return out
	}
	for uid, v := range grants {
		if creator != "" && uid == creator {
			continue
		}
		if entry, ok := v.(map[string]any); ok {
			if role, ok := entry["role"].(string); ok {
				out[uid] = role
			}
		}
	}
	return out
}

// roleCodeToLabel translates rich-doc doc_member.role integers to string labels.
// Unknown codes return the empty label so callers do not misrepresent corrupt
// data as a valid reader grant.
func roleCodeToLabel(role int) string {
	switch role {
	case DocMemberRoleAdmin:
		return grantRoleAdmin
	case DocMemberRoleWriter:
		return grantRoleWriter
	case DocMemberRoleCommenter:
		return grantRoleCommenter
	case DocMemberRoleReader:
		return grantRoleReader
	default:
		return ""
	}
}

// roleLabelToCode maps a grant role label to its doc_member.role encoding.
// ok=false for any unknown label so callers fail closed (never default reader).
func roleLabelToCode(role string) (int, bool) {
	switch role {
	case grantRoleReader:
		return DocMemberRoleReader, true
	case grantRoleCommenter:
		return DocMemberRoleCommenter, true
	case grantRoleWriter:
		return DocMemberRoleWriter, true
	case grantRoleAdmin:
		return DocMemberRoleAdmin, true
	default:
		return 0, false
	}
}

// CapabilityForGrantRole maps a legacy meta.grants role label to a Capability
// for the unwired/single-node fallback (reader→Read, commenter→Comment,
// writer→Edit). admin and any unknown label fail closed to CapNone: the
// unwired fallback must never grant Manage, so a legacy/corrupt admin entry
// cannot escalate through this path.
func CapabilityForGrantRole(role string) Capability {
	code, ok := roleLabelToCode(role)
	if !ok || code == DocMemberRoleAdmin {
		return CapNone
	}
	return CapabilityForDocRole(code)
}

// AddGrant grants uid a role on slug (upsert). grantedBy records who authorized
// it. Accepts reader/commenter/writer; admin is refused here — admin identity is
// owned by creator_uid + the M1 backfill, never mintable through the grants API
// (RemoveGrant/AddGrant both refuse to touch an admin row).
//
// TODO: verify uid is a real octo user (anti ghost-member) once octo-server
// exposes a uid-existence lookup the doc can call; today any uid is accepted.
func (s *AuthService) AddGrant(ctx context.Context, slug, uid, role, grantedBy string) error {
	if uid == "" {
		return apperr.Validation("uid required", "invalid_grant")
	}
	code, ok := roleLabelToCode(role)
	if !ok || code == DocMemberRoleAdmin {
		return apperr.Validation("role must be reader|commenter|writer", "invalid_grant")
	}
	if s.docMembers != nil {
		return s.addGrantToDocMember(ctx, slug, uid, code, grantedBy)
	}
	return s.addGrantToMeta(ctx, slug, uid, role, grantedBy)
}

// addGrantToDocMember is the plan③ A6 primary path. UpsertDirectGrant is
// idempotent for the same (docID,uid,role). It refuses grants that would touch
// the creator uid or an admin (role=admin, encoded 3) row.
// An identical existing role skips the doc_member write (no permission_epoch
// bump); a different non-admin role is a legitimate change (e.g. reader→commenter,
// commenter→writer, or a downgrade) and is written through. Either way, on a
// registered doc the (optional) write and a sweep of any stale legacy
// meta.grants[uid] run under one slug lock (P1) so a later unregistered fallback
// cannot revive the pre-change role — including the same-role case, where a
// stale HIGHER meta.grants role must still be cleared even though doc_member is
// unchanged.
//
// P1 (round-7): the RoleByDocUID probe and the resulting skip-upsert decision
// BOTH run inside the slug lock. An earlier version probed before locking, so a
// concurrent RemoveGrant that deleted the row after the probe (but before we
// locked) left a stale unchanged=true: AddGrant then acquired the lock, skipped
// the upsert as "unchanged", swept meta, and returned success — silently
// dropping the requested grant (the row was gone). Re-probing under the lock
// closes that TOCTOU: whatever RemoveGrant/backfill did is now committed before
// we read, so the skip only fires on a genuinely-current identical role.
func (s *AuthService) addGrantToDocMember(ctx context.Context, slug, uid string, roleCode int, grantedBy string) error {
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
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug, spaceIDForDoc(ctx, meta))
	if err != nil {
		return err
	}
	if !ok {
		// Doc not yet registered in doc_member (post-commit registration gap, or
		// a non-mounted / failed registration). Fall back to the legacy
		// meta.grants writer so the four operations stay aligned.
		return s.addGrantToMeta(ctx, slug, uid, roleCodeToLabel(roleCode), grantedBy)
	}
	// P1 (round-6): serialize the doc_member write and the legacy meta.grants
	// sweep under one slug lock (same lock reconcile + RemoveGrant take). On a
	// REGISTERED doc, doc_member is authoritative, so any leftover
	// meta.grants[uid] from an earlier unregistered/fallback write is stale. Left
	// behind, a later unmount/soft-delete flipping DocIDBySlug back to ok=false
	// would let the A4 legacy fallback revive that stale (possibly higher) role —
	// reverting a downgrade written here, OR reviving a higher role over an
	// unchanged one. Sweeping on every registered path (change AND no-change)
	// makes AddGrant symmetric with RemoveGrant, which already sweeps unconditionally.
	return s.lock.With(ctx, slug, func() error {
		// Re-probe the CURRENT registered role inside the lock (P1 round-7). A
		// pre-lock probe could go stale if a concurrent RemoveGrant deleted the
		// row between probe and lock — skipping the upsert then would drop the
		// requested grant. Deciding here, after the lock serialises us behind
		// any concurrent revoke/backfill, keeps the skip honest.
		role, ok, err := s.docMembers.RoleByDocUID(ctx, docID, uid)
		if err != nil {
			return err
		}
		if ok && role == DocMemberRoleAdmin {
			// Creator/admin rows are never mintable/mutable through this API.
			return ErrGrantProtected
		}
		// Skip the doc_member write only when a row genuinely still exists with
		// the identical role (no permission_epoch bump). If the row is now
		// absent (ok=false, e.g. a concurrent revoke landed first) or holds a
		// different role, we MUST write it — the grant was requested and must be
		// (re)applied.
		if !ok || role != roleCode {
			if err := s.docMembers.UpsertDirectGrant(ctx, docID, uid, roleCode, grantedBy); err != nil {
				return err
			}
		}
		// Absent entry ⇒ nil (idempotent); never fail the grant on a clean sweep.
		return s.removeGrantFromMetaLocked(ctx, slug, uid)
	})
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
		// Hold the slug lock across both the
		// doc_member DELETE and the meta sweep. Without this, reconcile
		// could snapshot meta.grants[uid], then RemoveGrant deletes the
		// doc_member row and sweeps meta.grants, and finally reconcile
		// re-inserts the doc_member row from its stale snapshot =
		// resurrected revoked grant. Round-4's per-op locks left this
		// TOCTOU open; wrapping both here (and reconcile takes the same
		// lock) serialises the pair.
		return s.lock.With(ctx, slug, func() error {
			if err := s.removeGrantFromDocMember(ctx, slug, uid, spaceIDForDoc(ctx, meta)); err != nil {
				return err
			}
			// Purge any legacy meta.grants[uid]
			// on every remove path so a later unmount / soft-delete flipping
			// DocIDBySlug back to ok=false cannot resurrect via the A4
			// fallback. Absent entry ⇒ nil (idempotent).
			return s.removeGrantFromMetaLocked(ctx, slug, uid)
		})
	}
	return s.removeGrantFromMeta(ctx, slug, uid)
}

// removeGrantFromDocMember resolves the doc with the caller-provided spaceID
// (already derived via spaceIDForDoc by RemoveGrant from the fetched meta),
// so a user-publish doc with no ctx space is still scoped to its own space —
// never the unfiltered slug fallback that could hit another space's row.
func (s *AuthService) removeGrantFromDocMember(ctx context.Context, slug, uid, spaceID string) error {
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug, spaceID)
	if err != nil {
		return err
	}
	if !ok {
		// Doc not registered in rich-doc yet (post-commit registration gap, or a
		// non-mounted / failed registration). Nothing to delete from
		// doc_member — the caller (RemoveGrant) still sweeps meta.grants
		// unconditionally.
		return nil
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
	// P2 race guard: DeleteGrant returns ErrDocMemberAdminGuard if a concurrent
	// backfill promoted the row to admin between our probe and the DELETE.
	// Translate that to the domain-level protected error so RemoveGrant callers
	// see one sentinel regardless of where the guard triggered.
	if err := s.docMembers.DeleteGrant(ctx, docID, uid); err != nil {
		if errors.Is(err, ErrDocMemberAdminGuard) {
			return ErrGrantProtected
		}
		return err
	}
	return nil
}

// removeGrantFromMeta takes the slug lock and calls the locked helper. Used
// by the mirror-unwired branch of RemoveGrant so single-node deploys still get
// serialized meta writes.
func (s *AuthService) removeGrantFromMeta(ctx context.Context, slug, uid string) error {
	return s.lock.With(ctx, slug, func() error {
		return s.removeGrantFromMetaLocked(ctx, slug, uid)
	})
}

// removeGrantFromMetaLocked is the body of removeGrantFromMeta assuming the
// caller already holds s.lock for slug. RemoveGrant's
// wired branch takes one slug lock across both the doc_member DELETE and the
// meta sweep, so this helper serialises with reconcile (which also holds the
// same lock) — reconcile can no longer resurrect a revoked reader from a
// stale meta.grants snapshot.
func (s *AuthService) removeGrantFromMetaLocked(ctx context.Context, slug, uid string) error {
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
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug, spaceIDForDoc(ctx, nil))
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
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug, spaceIDForDoc(ctx, nil))
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

// ReconcileMetaGrantsToDocMember migrates any legacy meta.grants entries into
// doc_member and then clears the consumed entries. Called by
// DocService.afterPublished after confirmed registration so that grants issued
// during the registration gap (AddGrant → meta.grants fallback while
// DocIDBySlug ok=false) do not evaporate once bestCred flips to the strict
// wired gate.
//
// Wired/registered mode: doc_member is the sole authority. reader/commenter/
// writer are inserted via InsertDirectGrantIfAbsent (atomic: never overwrites a
// concurrently-written authoritative row; bumps epoch only on insert). admin,
// unknown, and malformed entries are never restored (fail closed). Once the doc
// is registered every consumed entry is cleared — including invalid/admin ones
// so they cannot resurrect via a later unregistered fallback — but only after a
// successful insert / confirmed existing row; a DB error retains the entry for
// retry. Runs under the slug lock (shared with RemoveGrant's sweep).
func (s *AuthService) ReconcileMetaGrantsToDocMember(ctx context.Context, slug string) error {
	if s.docMembers == nil {
		return nil
	}
	return s.lock.With(ctx, slug, func() error {
		meta, err := s.meta.GetMeta(ctx, slug)
		if err != nil {
			return fmt.Errorf("reconcile meta lookup: %w", err)
		}
		if meta == nil {
			return nil
		}
		docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug, spaceIDForDoc(ctx, meta))
		if err != nil {
			return fmt.Errorf("reconcile slug resolve: %w", err)
		}
		if !ok {
			// Not registered yet: leave meta.grants intact for the unwired
			// fallback; a later publish/mount retriggers.
			return nil
		}
		grants, ok := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
		if !ok || len(grants) == 0 {
			return nil
		}
		creator := meta.CreatorUID()
		logger := slog.Default()
		consumed := make([]string, 0, len(grants))
		for uid, v := range grants {
			if uid == "" || (creator != "" && uid == creator) {
				continue
			}
			entry, ok := v.(map[string]any)
			if !ok {
				// Malformed shape: consume so it cannot resurrect once wired.
				consumed = append(consumed, uid)
				continue
			}
			roleStr, _ := entry["role"].(string)
			code, ok := roleLabelToCode(roleStr)
			if !ok || code == DocMemberRoleAdmin {
				// admin/unknown: never restore; consume so a later unregistered
				// fallback cannot escalate from a legacy/corrupt entry.
				consumed = append(consumed, uid)
				continue
			}
			grantedBy, _ := entry["granted_by"].(string)
			if grantedBy == "" {
				grantedBy = "reconcile_afterpublished"
			}
			// Atomic insert-if-absent: never overwrites a concurrent authoritative
			// row. Consume on insert OR when a row already exists; retain on error.
			if _, err := s.docMembers.InsertDirectGrantIfAbsent(ctx, docID, uid, code, grantedBy); err != nil {
				logger.Error("reconcile insert failed", "slug", slug, "uid", uid, "err", err.Error())
				continue // leave this entry for a later retry; do not clear
			}
			consumed = append(consumed, uid)
		}
		if len(consumed) == 0 {
			return nil
		}
		// Clear consumed entries only after their writes succeeded, re-reading
		// meta under the same lock to avoid clobbering a concurrent meta write.
		return s.clearConsumedMetaGrantsLocked(ctx, slug, consumed)
	})
}

// clearConsumedMetaGrantsLocked removes uids from meta.grants; caller holds
// s.lock for slug. Drops the whole key when the map empties. Idempotent for
// absent uids. Completes the wired migration: once a grant is authoritative in
// doc_member, its stale metadata is removed so no fallback can resurrect it.
func (s *AuthService) clearConsumedMetaGrantsLocked(ctx context.Context, slug string, uids []string) error {
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return fmt.Errorf("reconcile clear lookup: %w", err)
	}
	if meta == nil {
		return nil
	}
	existing, ok := meta.Extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
	if !ok || len(existing) == 0 {
		return nil
	}
	remove := make(map[string]struct{}, len(uids))
	for _, uid := range uids {
		remove[uid] = struct{}{}
	}
	grants := map[string]any{}
	for k, v := range existing {
		if _, drop := remove[k]; drop {
			continue
		}
		grants[k] = v
	}
	if len(grants) == len(existing) {
		return nil // nothing actually removed
	}
	extra := map[string]any{}
	maps.Copy(extra, meta.Extra)
	if len(grants) == 0 {
		delete(extra, storage.GrantsExtraKey) //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
	} else {
		extra[storage.GrantsExtraKey] = grants //nolint:staticcheck // legacy meta.grants fallback until A7 cleanup
	}
	return s.meta.PutMeta(ctx, slug, storage.DocMeta{
		Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra,
	})
}

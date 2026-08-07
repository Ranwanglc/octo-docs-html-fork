package httpx_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/log"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/transport/httpx"
)

// P1-A / P1b regression tests.
//
// P1-A: doc_member rows appear after the HTML commit during the publish request,
// and non-mounted / failed-registration docs never register. A wired mirror can
// therefore return DocIDBySlug ok=false on a live doc. In that state A3② / A4
// must fall back to the meta-based legacy match, otherwise a bot-behind-owner
// would 404 its own freshly published doc and a grant created in the
// registration gap would disappear.
//
// P1b (round 3): the fallback is ONLY safe when the doc is truly unregistered.
// If the doc IS registered but the caller has no doc_member row (e.g. a M2
// migrated reader whose row was just DELETEd), falling back to meta.grants
// would resurrect access = revoke bypass. RoleBySlugUID's docRegistered
// return separates the two states so this file exercises both.

// newServerWithMirrorAndBotAuth is newServerWithMirror + bot auth enabled so
// tests can drive a Bearer bot-token request path (selfUID=bot, ownerUID=owner).
func newServerWithMirrorAndBotAuth(t *testing.T, mirror *stubMirror) (http.Handler, *memory.Store) {
	t.Helper()
	cfg := ownerAuthCfg()
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, cfg.BaseURL, cfg.MaxHTMLBytes)
	assets := service.NewAssetService(store, store, locker, cfg.MaxAssetBytes, cfg.AssetMIMEAllow)
	auth := service.NewAuthService(store, cfg, locker).WithDocMemberMirror(mirror)
	srv := httpx.New(httpx.Deps{
		Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments,
		Assets: assets, Auth: auth, OverlayJS: "/* overlay */",
	})
	return srv.Handler(), store
}

// A3② unregistered doc: DocIDBySlug returns ok=false (doc not in doc_member
// yet) -> docRegistered=false -> meta fallback allowed -> creator_uid==ownerUID
// authors the bot. Mirrors the post-commit registration gap: bot bearer
// would otherwise 404 its own freshly published doc.
func TestA3OwnerFallsBackToMetaWhenDocUnregistered(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	mirror := &stubMirror{} // no slugToDoc, no roles -> DocIDBySlug ok=false
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	key := publishAsBot(t, h, "docW") // creator_uid = owner-1 (bot -> ownerUID stamp)

	// Bot bearer session: selfUID=bot-1, ownerUID=owner-1. A3① misses (bot uid
	// != creator=owner-1); A3② wired branch returns docRegistered=false and
	// falls back to creator_uid==ownerUID via meta -> author.
	rec := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bot bearer share on unregistered doc = %d; want 200 (P1-A meta fallback): %s", rec.Code, rec.Body.String())
	}
}

// A3② P1-a lockout close: doc IS registered but no
// doc_member owner-admin row (M1 has not backfilled yet, or docs-backend
// registered the doc atomically without owner-admin). A3②'s fallback keys on
// creator_uid, which is stamped at publish and never revocable, so falling back
// is safe: it cannot resurrect a revoked grant. An earlier revision gated this
// path on docRegistered and locked the owner out of their own doc; the gate was removed
// on A3② only (A4 keeps its gate, see TestA4RegisteredDocDeletedRowNoFallback).
func TestA3OwnerFallbackAllowedWhenDocRegisteredButRowMissing(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-2", botName: "Bot Two", botSpaceID: "s2", botOwnerUID: "owner-2"})
	mirror := &stubMirror{slugToDoc: map[string]string{service.DeriveDocKey("owner-2", "docReg"): "dReg"}} // registered, no owner row
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	key := publishAsBot(t, h, "docReg") // creator_uid = owner-2

	// A3① misses (bot uid != owner-2); A3② wired returns docRegistered=true,
	// ok=false; without the old gate, fallback proceeds and
	// creator_uid==ownerUID lands owner-2 as CapAuthor.
	rec := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bot bearer share on registered-no-owner-row = %d; want 200 (P1-a fallback restored): %s", rec.Code, rec.Body.String())
	}
}

// A3② P1-a lockout close, with unrelated doc_member rows present. Mirror
// has entries for other uids (a reader grant and a stranger) but no
// owner-admin row. Confirms the fallback keys strictly on
// creator_uid==ownerUID via meta and is not confused by adjacent rows.
func TestA3OwnerNoLockoutWhenDocRegisteredButAdminRowMissing(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-3", botName: "Bot Three", botSpaceID: "s3", botOwnerUID: "owner-3"})
	mirror := &stubMirror{
		slugToDoc: map[string]string{service.DeriveDocKey("owner-3", "docReg3"): "dReg3"},
		roles: map[string]int{
			"dReg3|reader-x":   service.DocMemberRoleReader,
			"dReg3|stranger-y": service.DocMemberRoleReader,
		},
	}
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	key := publishAsBot(t, h, "docReg3") // creator_uid = owner-3

	rec := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bot bearer share w/ unrelated rows = %d; want 200 (creator_uid fallback wins): %s", rec.Code, rec.Body.String())
	}
}

// A3② wired match still wins when a doc_member admin row exists. Same setup
// but with the row backfilled: wired path returns author directly. Guards
// against the P1-A fallback regressing the primary match.
func TestA3OwnerAdminInDocMemberStillWinsAfterFallback(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	mirror := &stubMirror{
		slugToDoc: map[string]string{service.DeriveDocKey("owner-1", "docWM"): "dWM"},
		roles:     map[string]int{"dWM|owner-1": service.DocMemberRoleAdmin},
	}
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	key := publishAsBot(t, h, "docWM")

	rec := do(t, h, http.MethodPost, "/v1/docs/"+key+"/share",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bot bearer share via doc_member admin = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}

// A4 unregistered doc falls back to meta.grants: mirror wired but no
// slug->docID mapping (post-commit registration gap). The
// reader tier's wired probe returns docRegistered=false and legacy
// meta.grants[uid]=reader keeps the caller readable.
func TestA4UnregisteredDocFallsBackToMeta(t *testing.T) {
	h, store := newServerWithMirrorAndBotAuth(t, &stubMirror{}) // no slugToDoc

	rec := do(t, h, http.MethodPost, "/v1/docs",
		map[string]string{octoUIDHeaderName: "owner-42", "Content-Type": "application/json"},
		`{"slug":"docR","version":1,"html":"<html><body><p>hi</p></body></html>","meta":{"title":"T"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)

	// Legacy grant seeded before doc_member registration lands.
	seedLegacyReaderGrant(t, store, key, "reader-9")

	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1",
		map[string]string{octoUIDHeaderName: "reader-9"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unregistered-doc reader read = %d; want 200 via legacy fallback: %s", rec.Code, rec.Body.String())
	}
}

// A4 P1b revoke-bypass close: doc IS registered but reader-9 has no
// doc_member row, yet a stale legacy meta.grants[reader-9]=reader lingers
// (M2 copies, does not delete; or the row was just DELETEd via
// /grants/{uid}). Fallback must be blocked so the read is 404 — otherwise
// the revoke is silently a no-op.
func TestA4RegisteredDocDeletedRowNoFallback(t *testing.T) {
	mirror := &stubMirror{slugToDoc: map[string]string{service.DeriveDocKey("owner-42", "docR"): "dR"}} // registered, no reader row
	h, store := newServerWithMirrorAndBotAuth(t, mirror)

	rec := do(t, h, http.MethodPost, "/v1/docs",
		map[string]string{octoUIDHeaderName: "owner-42", "Content-Type": "application/json"},
		`{"slug":"docR","version":1,"html":"<html><body><p>hi</p></body></html>","meta":{"title":"T"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)

	// Stale legacy grant left behind after M2 migration / a prior revoke.
	seedLegacyReaderGrant(t, store, key, "reader-9")

	// reader-9 reads. A4 wired returns docRegistered=true, ok=false -> fallback
	// blocked -> meta.grants[reader-9] IGNORED -> 404. Revoke bypass closed.
	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1",
		map[string]string{octoUIDHeaderName: "reader-9"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("registered-no-row reader read = %d; want 404 (P1b revoke bypass close): %s", rec.Code, rec.Body.String())
	}
}

// seedLegacyReaderGrant writes DocMeta.Extra["grants"][uid]={role:"reader"}
// directly on the in-memory store -- the exact shape pre-plan③ AddGrant
// used to produce.
func seedLegacyReaderGrant(t *testing.T, store *memory.Store, slug, uid string) {
	t.Helper()
	ctx := context.Background()
	meta, err := store.GetMeta(ctx, slug)
	if err != nil || meta == nil {
		t.Fatalf("seed lookup: meta=%v err=%v", meta, err)
	}
	extra := map[string]any{}
	for k, v := range meta.Extra {
		extra[k] = v
	}
	grants, _ := extra[storage.GrantsExtraKey].(map[string]any) //nolint:staticcheck // legacy meta.grants seed for P1-A fallback test
	if grants == nil {
		grants = map[string]any{}
	}
	grants[uid] = map[string]any{"role": "reader"}
	extra[storage.GrantsExtraKey] = grants //nolint:staticcheck // legacy meta.grants seed for P1-A fallback test
	if err := store.PutMeta(ctx, slug, storage.DocMeta{Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra}); err != nil {
		t.Fatalf("seed write: %v", err)
	}
}

// End-to-end revoke-bypass regression: an M2-migrated reader has both a
// doc_member row AND a stale meta.grants[uid] entry (M2 copies, does not
// delete). A DELETE /grants/{uid} used to remove the doc_member row but
// leave meta.grants alone; the next read fell through the wired probe
// (ok=false, docRegistered=true) to legacy meta.GrantRole and still
// returned CapReader — the revoke was silent no-op = revoke bypass.
//
// With the P1a/P1b fix, docRegistered=true blocks the meta fallback and
// the post-revoke read is 404. This is the "safety property" test.
func TestRevokeClosesReadWithStaleMetaGrant(t *testing.T) {
	mirror := &stubMirror{slugToDoc: map[string]string{service.DeriveDocKey("owner-42", "docM"): "dM"}}
	h, store := newServerWithMirrorAndBotAuth(t, mirror)

	// Owner publishes; creator_uid = owner-42.
	rec := do(t, h, http.MethodPost, "/v1/docs",
		map[string]string{octoUIDHeaderName: "owner-42", "Content-Type": "application/json"},
		`{"slug":"docM","version":1,"html":"<html><body><p>hi</p></body></html>","meta":{"title":"T"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	key := pubKey(t, rec)

	// Owner grants reader-9 (writes doc_member).
	rec = do(t, h, http.MethodPut, "/v1/docs/"+key+"/grants",
		map[string]string{octoUIDHeaderName: "owner-42", "Content-Type": "application/json"},
		`{"uid":"reader-9","role":"reader"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant = %d: %s", rec.Code, rec.Body.String())
	}

	// Simulate M2: the migration copied the same grant into meta.grants and
	// never deleted the source. Plant that stale entry now.
	seedLegacyReaderGrant(t, store, key, "reader-9")

	// Pre-revoke sanity: reader-9 can read.
	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1",
		map[string]string{octoUIDHeaderName: "reader-9"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-revoke read = %d; want 200: %s", rec.Code, rec.Body.String())
	}

	// Owner revokes. doc_member row is deleted; meta.grants[reader-9] lingers.
	rec = do(t, h, http.MethodDelete, "/v1/docs/"+key+"/grants/reader-9",
		map[string]string{octoUIDHeaderName: "owner-42"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke = %d; want 200: %s", rec.Code, rec.Body.String())
	}

	// Post-revoke: A4 wired returns docRegistered=true, ok=false; fallback
	// disabled; meta.grants[reader-9] IGNORED; render 404. Pre-fix this was
	// still 200 and the revoke was a silent no-op.
	rec = do(t, h, http.MethodGet, "/d/"+key+"/v/1",
		map[string]string{octoUIDHeaderName: "reader-9"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("post-revoke read = %d; want 404 (revoke bypass closed): %s", rec.Code, rec.Body.String())
	}
}

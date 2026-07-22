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

// P1-A regression tests (yujiawei review on PR #17).
//
// doc_member rows are registered asynchronously (DocService.afterPublished
// go-routine) and thread-mount / non-mounted docs are never registered at
// all, so a WIRED mirror can legitimately return ok=false on a live doc.
// Without a fallback the tier skips and the bot-behind-owner path 404s on
// its own doc; a legacy reader grant that landed before registration also
// disappears. A3② and A4 both fall through to the meta-based legacy match
// when the wired probe misses.

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

// A3② wired-!ok fallback: creator_uid == ownerUID via meta, doc_member has
// no row -> still CapAuthor. Mirrors yujiawei's repro: mirror wired,
// doc_member empty, creator_uid stamped as owner because A1 has not flipped
// yet, bot bearer would otherwise 404 its own doc.
func TestA3OwnerFallsBackToMetaWhenDocMemberEmpty(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	mirror := &stubMirror{slugToDoc: map[string]string{"docW": "dW"}} // no roles seeded
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	publishAsBot(t, h, "docW") // creator_uid = owner-1 (bot -> ownerUID stamp)

	// Bot bearer session: selfUID=bot-1, ownerUID=owner-1. A3① misses (bot uid
	// != creator=owner-1); A3② wired branch returns ok=false and must fall
	// back to creator_uid==ownerUID via meta -> author.
	rec := do(t, h, http.MethodPost, "/v1/docs/docW/share",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bot bearer share on wired-empty doc = %d; want 200 (P1-A meta fallback): %s", rec.Code, rec.Body.String())
	}
}

// A3② wired match still wins when a doc_member admin row exists. Same setup
// but with the row backfilled: wired path returns author directly. Guards
// against the P1-A fallback regressing the primary match.
func TestA3OwnerAdminInDocMemberStillWinsAfterFallback(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	mirror := &stubMirror{
		slugToDoc: map[string]string{"docWM": "dWM"},
		roles:     map[string]int{"dWM|owner-1": service.DocMemberRoleAdmin},
	}
	h, _ := newServerWithMirrorAndBotAuth(t, mirror)
	publishAsBot(t, h, "docWM")

	rec := do(t, h, http.MethodPost, "/v1/docs/docWM/share",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("bot bearer share via doc_member admin = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}

// A4 wired-!ok fallback: mirror wired but no doc_member row for a uid that
// only appears in legacy meta.grants (pre-plan③ data or a reader grant
// added while doc_member registration was still pending). The reader tier
// must fall back to meta.GrantRole so the caller keeps reader access.
func TestA4WiredMissFallsBackToMetaGrant(t *testing.T) {
	// Wired mirror without a slug -> docID mapping. RoleBySlugUID misses.
	h, store := newServerWithMirrorAndBotAuth(t, &stubMirror{})

	// Publish as the owner so creator_uid == "owner-42".
	rec := do(t, h, http.MethodPost, "/v1/docs",
		map[string]string{octoUIDHeaderName: "owner-42", "Content-Type": "application/json"},
		`{"slug":"docR","version":1,"html":"<html><body><p>hi</p></body></html>","meta":{"title":"T"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}

	// Simulate pre-plan③ / pending-registration state: plant a legacy
	// meta.grants[reader-9]=reader row directly in the store.
	seedLegacyReaderGrant(t, store, "docR", "reader-9")

	// reader-9 reads the doc via trust header. Wired probe misses (no docID),
	// fallback finds meta.grants[reader-9]=reader -> CapReader -> render 200.
	rec = do(t, h, http.MethodGet, "/d/docR/v/1",
		map[string]string{octoUIDHeaderName: "reader-9"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("wired-but-!ok reader read = %d; want 200 via legacy fallback: %s", rec.Code, rec.Body.String())
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

package httpx_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/lml2468/octo-doc/internal/config"
	"github.com/lml2468/octo-doc/internal/platform/log"
	"github.com/lml2468/octo-doc/internal/platform/sluglock"
	"github.com/lml2468/octo-doc/internal/service"
	"github.com/lml2468/octo-doc/internal/storage"
	"github.com/lml2468/octo-doc/internal/storage/memory"
	"github.com/lml2468/octo-doc/internal/transport/httpx"
)

// 方案 i acceptance: author is owned by the USER behind a bot (OwnerUID), so a
// bot and its owner share author, while unrelated bots/users do not — and a bot
// is no longer a blanket superAdmin. These tests exercise the full HTTP path
// (bot verify → publish → author-only ops) against different identities.

func ownerAuthCfg() *config.Config {
	return &config.Config{
		WriteToken:        "test-token",
		MaxHTMLBytes:      5 << 20,
		MaxAssetBytes:     25 << 20,
		AssetMIMEAllow:    []string{"image/png"},
		OctoServerBaseURL: "http://octo.example",
		BotAuthEnabled:    true,
	}
}

func publishAsBot(t *testing.T, h http.Handler, slug string) {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/v1/docs",
		map[string]string{"Authorization": "Bearer bot-token", "Content-Type": "application/json"},
		`{"slug":"`+slug+`","version":1,"html":"<html><body><p>hi</p></body></html>","meta":{"title":"T"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish as bot %s = %d: %s", slug, rec.Code, rec.Body.String())
	}
}

// 验收1: bot 发布 → creator = bot 的 OwnerUID（用户 uid），不是 bot uid。
func TestBotPublishStampsOwnerUIDAsCreator(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	h := newTestServer(t, ownerAuthCfg())
	publishAsBot(t, h, "docO")

	// The owner user (its own trust-header login == creator_uid) can author.
	rec := do(t, h, http.MethodDelete, "/v1/docs/docO",
		map[string]string{octoUIDHeaderName: "owner-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner delete = %d; creator_uid should be owner uid: %s", rec.Code, rec.Body.String())
	}

	// The bot's own uid must NOT be the creator: a user logging in as the bot uid
	// (not the owner) gets no author.
	publishAsBot(t, h, "docO2")
	rec = do(t, h, http.MethodDelete, "/v1/docs/docO2",
		map[string]string{octoUIDHeaderName: "bot-1"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bot-uid delete = %d; want 404 (creator is owner uid, not bot uid): %s", rec.Code, rec.Body.String())
	}
}

// 验收2: 该 owner 用户用自己信任头登录 → 能 author（delete/share）。
func TestOwnerUserAuthorsBotCreatedDoc(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-1", botName: "Bot One", botSpaceID: "s1", botOwnerUID: "owner-1"})
	h := newTestServer(t, ownerAuthCfg())
	publishAsBot(t, h, "docShare")

	rec := do(t, h, http.MethodPost, "/v1/docs/docShare/share",
		map[string]string{octoUIDHeaderName: "owner-1"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner share = %d: %s", rec.Code, rec.Body.String())
	}
}

// 验收3: 同 owner 的 bot 再来 → 能 author；不同 owner 的 bot / 无关 user → 404。
func TestSameOwnerBotAuthorsOthersRejected(t *testing.T) {
	// Bot A creates the doc (owner-1). Same-owner bot re-verifies via the same
	// stub (OwnerUID owner-1) → author. We then swap the stub to a different owner
	// bot and an unrelated user and assert both are hidden (404).
	withStubIdentity(t, stubIdentity{botUID: "bot-A", botName: "Bot A", botSpaceID: "s1", botOwnerUID: "owner-1"})
	h := newTestServer(t, ownerAuthCfg())
	publishAsBot(t, h, "docTeam")

	// Same-owner bot (OwnerUID owner-1) shares → author.
	rec := do(t, h, http.MethodPost, "/v1/docs/docTeam/share",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("same-owner bot share = %d: %s", rec.Code, rec.Body.String())
	}

	// Different-owner bot → 404 (not author).
	withStubIdentity(t, stubIdentity{botUID: "bot-B", botName: "Bot B", botSpaceID: "s1", botOwnerUID: "owner-2"})
	rec = do(t, h, http.MethodDelete, "/v1/docs/docTeam",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("different-owner bot delete = %d; want 404: %s", rec.Code, rec.Body.String())
	}

	// Unrelated user → 404.
	rec = do(t, h, http.MethodDelete, "/v1/docs/docTeam",
		map[string]string{octoUIDHeaderName: "random-user"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unrelated user delete = %d; want 404: %s", rec.Code, rec.Body.String())
	}
}

// 验收5: 一个 bot 对别人 owner 的 doc → 非 author（404）。确认普通 bot 不再是全局
// superAdmin。 A user-created doc (owner-X) is not writable by an unrelated bot.
func TestUnrelatedBotNotSuperAdminOnOthersDoc(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-X", botName: "Bot X", botSpaceID: "s1", botOwnerUID: "owner-X"})
	h := newTestServer(t, ownerAuthCfg())
	// Doc created by a real user (creator_uid = "human-owner"), no bot involved.
	rec := do(t, h, http.MethodPost, "/v1/docs",
		map[string]string{octoUIDHeaderName: "human-owner", "Content-Type": "application/json"},
		`{"slug":"docHuman","version":1,"html":"<html><body><p>hi</p></body></html>","meta":{"title":"T"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("human publish = %d: %s", rec.Code, rec.Body.String())
	}

	// Unrelated bot must NOT be able to delete/author it (would have if bot were
	// a global superAdmin — the retired behavior).
	rec = do(t, h, http.MethodDelete, "/v1/docs/docHuman",
		map[string]string{"Authorization": "Bearer bot-token"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unrelated bot delete = %d; want 404 (bot is not superAdmin): %s", rec.Code, rec.Body.String())
	}
}

// 验收4: 普通用户/bot draft-first 建新 slug → 成功（不再 404），且 creator 落对。
func TestDraftFirstCreateByUser(t *testing.T) {
	h := newTestServer(t, ownerAuthCfg())
	// A brand-new slug with no prior publish. Under the old gate this 404'd
	// (requireDocAuthor found no creator to match). Now a first draft succeeds.
	draft := `{"html":"<html><body><p>draft</p></body></html>","meta":{"title":"D"}}`
	rec := do(t, h, http.MethodPut, "/v1/docs/newSlugU/draft",
		map[string]string{octoUIDHeaderName: "user-42", "Content-Type": "application/json"}, draft)
	if rec.Code != http.StatusOK {
		t.Fatalf("draft-first save = %d; want 200 (no longer 404): %s", rec.Code, rec.Body.String())
	}

	// creator was stamped to the creating user → same user can promote (author).
	rec = do(t, h, http.MethodPost, "/v1/docs/newSlugU/draft/promote",
		map[string]string{octoUIDHeaderName: "user-42"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("promote as creator = %d: %s", rec.Code, rec.Body.String())
	}

	// A different user must not author it after creation.
	rec = do(t, h, http.MethodDelete, "/v1/docs/newSlugU",
		map[string]string{octoUIDHeaderName: "someone-else"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other user delete = %d; want 404: %s", rec.Code, rec.Body.String())
	}
	// The creator can delete it.
	rec = do(t, h, http.MethodDelete, "/v1/docs/newSlugU",
		map[string]string{octoUIDHeaderName: "user-42"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("creator delete = %d: %s", rec.Code, rec.Body.String())
	}
}

// draft-first by a bot stamps the OwnerUID as creator (same owner rule), and the
// owner user can then author the promoted doc.
func TestDraftFirstCreateByBotStampsOwner(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "bot-D", botName: "Bot D", botSpaceID: "s1", botOwnerUID: "owner-D"})
	h := newTestServer(t, ownerAuthCfg())
	draft := `{"html":"<html><body><p>draft</p></body></html>","meta":{"title":"D"}}`
	rec := do(t, h, http.MethodPut, "/v1/docs/newSlugBot/draft",
		map[string]string{"Authorization": "Bearer bot-token", "Content-Type": "application/json"}, draft)
	if rec.Code != http.StatusOK {
		t.Fatalf("bot draft-first save = %d: %s", rec.Code, rec.Body.String())
	}

	// The owner user (owner-D) can author it — creator was stamped to OwnerUID.
	rec = do(t, h, http.MethodPost, "/v1/docs/newSlugBot/draft/promote",
		map[string]string{octoUIDHeaderName: "owner-D"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner promote of bot draft = %d: %s", rec.Code, rec.Body.String())
	}

	// The stamped creator survives promote: confirm a published version exists and
	// the owner can delete.
	rec = do(t, h, http.MethodDelete, "/v1/docs/newSlugBot",
		map[string]string{octoUIDHeaderName: "owner-D"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("owner delete after promote = %d: %s", rec.Code, rec.Body.String())
	}
}

// newTestServerWithStore builds a server on a caller-visible memory store so a
// test can pre-seed legacy/creator-less docs the HTTP path would never produce
// (publish always stamps a creator). Returns the handler and the raw store.
func newTestServerWithStore(t *testing.T, cfg *config.Config) (http.Handler, *memory.Store) {
	t.Helper()
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, cfg.BaseURL, cfg.MaxHTMLBytes)
	assets := service.NewAssetService(store, store, locker, cfg.MaxAssetBytes, cfg.AssetMIMEAllow)
	auth := service.NewAuthService(store, cfg, locker)
	srv := httpx.New(httpx.Deps{
		Config: cfg, Logger: log.New("silent"), Docs: docs, Comments: comments,
		Assets: assets, Auth: auth, OverlayJS: "/* overlay */",
	})
	return srv.Handler(), store
}

// BLOCKER-1: a pre-migration/write-token-era doc can have real versions (or a
// draft) but an empty creator_uid. The draft-first bypass must NOT treat such an
// existing doc as "no creator ⇒ first-create": an unrelated logged-in user must
// not be able to PUT /draft and stamp themselves as author (hijack). Existing
// creator-less content falls to strict author-only (only superAdmin passes).
func TestCreatorlessExistingDocNotHijackableViaDraft(t *testing.T) {
	ctx := context.Background()

	// (a) Existing PUBLISHED doc with a version but no creator_uid.
	t.Run("has_version", func(t *testing.T) {
		h, store := newTestServerWithStore(t, ownerAuthCfg())
		if _, err := store.PutDoc(ctx, "legacyPub", 1, "<html><body><p>old</p></body></html>"); err != nil {
			t.Fatalf("seed doc blob: %v", err)
		}
		if err := store.PutMeta(ctx, "legacyPub", storage.DocMeta{
			Slug: "legacyPub", Title: "Legacy",
			Versions: []storage.VersionRef{{N: 1}},
			Extra:    map[string]any{}, // NO creator_uid
		}); err != nil {
			t.Fatalf("seed meta: %v", err)
		}

		draft := `{"html":"<html><body><p>mine now</p></body></html>","meta":{"title":"H"}}`
		rec := do(t, h, http.MethodPut, "/v1/docs/legacyPub/draft",
			map[string]string{octoUIDHeaderName: "attacker", "Content-Type": "application/json"}, draft)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("attacker draft on creator-less published doc = %d; want 404 (no hijack): %s", rec.Code, rec.Body.String())
		}
		// Creator was NOT stamped: still empty.
		meta, err := store.GetMeta(ctx, "legacyPub")
		if err != nil {
			t.Fatalf("re-read meta: %v", err)
		}
		if meta.CreatorUID() != "" {
			t.Fatalf("creator_uid got stamped by attacker = %q; want empty (no hijack)", meta.CreatorUID())
		}
	})

	// (b) Existing DRAFT (draft marker in Extra) but no creator_uid and no version.
	t.Run("has_draft", func(t *testing.T) {
		h, store := newTestServerWithStore(t, ownerAuthCfg())
		if _, err := store.PutDraft(ctx, "legacyDraft", "<html><body><p>wip</p></body></html>"); err != nil {
			t.Fatalf("seed draft blob: %v", err)
		}
		if err := store.PutMeta(ctx, "legacyDraft", storage.DocMeta{
			Slug: "legacyDraft", Title: "Legacy Draft",
			Versions: []storage.VersionRef{},
			Extra:    map[string]any{storage.DraftExtraKey: map[string]any{"updated_at": "x"}}, // draft, NO creator_uid
		}); err != nil {
			t.Fatalf("seed meta: %v", err)
		}

		draft := `{"html":"<html><body><p>mine now</p></body></html>","meta":{"title":"H"}}`
		rec := do(t, h, http.MethodPut, "/v1/docs/legacyDraft/draft",
			map[string]string{octoUIDHeaderName: "attacker", "Content-Type": "application/json"}, draft)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("attacker draft on creator-less drafted doc = %d; want 404 (no hijack): %s", rec.Code, rec.Body.String())
		}
		meta, err := store.GetMeta(ctx, "legacyDraft")
		if err != nil {
			t.Fatalf("re-read meta: %v", err)
		}
		if meta.CreatorUID() != "" {
			t.Fatalf("creator_uid got stamped by attacker = %q; want empty (no hijack)", meta.CreatorUID())
		}
	})

	// (c) A superAdmin CAN still recover such an orphaned creator-less doc.
	t.Run("superadmin_recovers", func(t *testing.T) {
		h, store := newTestServerWithStore(t, ownerAuthCfg())
		if _, err := store.PutDoc(ctx, "orphan", 1, "<html><body><p>o</p></body></html>"); err != nil {
			t.Fatalf("seed doc blob: %v", err)
		}
		if err := store.PutMeta(ctx, "orphan", storage.DocMeta{
			Slug: "orphan", Title: "Orphan",
			Versions: []storage.VersionRef{{N: 1}},
			Extra:    map[string]any{},
		}); err != nil {
			t.Fatalf("seed meta: %v", err)
		}
		draft := `{"html":"<html><body><p>admin fix</p></body></html>","meta":{"title":"H"}}`
		rec := do(t, h, http.MethodPut, "/v1/docs/orphan/draft",
			map[string]string{octoUIDHeaderName: "admin-uid", octoRoleHeaderName: "superAdmin", "Content-Type": "application/json"}, draft)
		if rec.Code != http.StatusOK {
			t.Fatalf("superAdmin draft on orphan doc = %d; want 200 (superAdmin override): %s", rec.Code, rec.Body.String())
		}
	})
}

// A genuinely brand-new slug (meta==nil) still succeeds via draft-first.
func TestBrandNewSlugDraftFirstStillWorks(t *testing.T) {
	h := newTestServer(t, ownerAuthCfg())
	draft := `{"html":"<html><body><p>new</p></body></html>","meta":{"title":"N"}}`
	rec := do(t, h, http.MethodPut, "/v1/docs/freshSlug/draft",
		map[string]string{octoUIDHeaderName: "user-77", "Content-Type": "application/json"}, draft)
	if rec.Code != http.StatusOK {
		t.Fatalf("brand-new draft-first = %d; want 200: %s", rec.Code, rec.Body.String())
	}
	// The creator was stamped; a different user cannot author it.
	rec = do(t, h, http.MethodDelete, "/v1/docs/freshSlug",
		map[string]string{octoUIDHeaderName: "other"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("other user delete of freshSlug = %d; want 404: %s", rec.Code, rec.Body.String())
	}
}

// BLOCKER-2: wipeComments (DELETE /v1/comments?slug=..&all=1) is author-only.
// creator/superAdmin can clear; non-author is 404; a stale write token no longer
// authorizes (it is not an author credential).
func TestWipeCommentsAuthorOnly(t *testing.T) {
	h := newTestServer(t, ownerAuthCfg())
	// Creator publishes and seeds a comment.
	rec := do(t, h, http.MethodPost, "/v1/docs",
		map[string]string{octoUIDHeaderName: "creator-w", "Content-Type": "application/json"},
		`{"slug":"wdoc","version":1,"html":"<html><body><p>hello world</p></body></html>","meta":{"title":"W"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("publish = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, h, http.MethodPost, "/v1/comments",
		map[string]string{octoUIDHeaderName: "creator-w", "Content-Type": "application/json"},
		`{"slug":"wdoc","text":"note","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed comment = %d: %s", rec.Code, rec.Body.String())
	}

	// Non-author → 404 (hidden), and does NOT wipe.
	rec = do(t, h, http.MethodDelete, "/v1/comments?slug=wdoc&all=1",
		map[string]string{octoUIDHeaderName: "stranger"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-author wipe = %d; want 404: %s", rec.Code, rec.Body.String())
	}

	// Stale write token no longer authorizes the wipe (author is creator/superAdmin).
	rec = do(t, h, http.MethodDelete, "/v1/comments?slug=wdoc&all=1",
		map[string]string{"Authorization": "Bearer test-token"}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("write-token wipe = %d; want 404 (write token no longer authorizes): %s", rec.Code, rec.Body.String())
	}

	// Comment still present after the rejected attempts.
	rec = do(t, h, http.MethodGet, "/v1/comments?slug=wdoc&version=1",
		map[string]string{octoUIDHeaderName: "creator-w"}, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "note") {
		t.Fatalf("comment should survive rejected wipes: %d %s", rec.Code, rec.Body.String())
	}

	// The creator CAN wipe (200).
	rec = do(t, h, http.MethodDelete, "/v1/comments?slug=wdoc&all=1",
		map[string]string{octoUIDHeaderName: "creator-w"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("creator wipe = %d; want 200: %s", rec.Code, rec.Body.String())
	}

	// A superAdmin can also wipe (seed a fresh comment first).
	_ = do(t, h, http.MethodPost, "/v1/comments",
		map[string]string{octoUIDHeaderName: "creator-w", "Content-Type": "application/json"},
		`{"slug":"wdoc","text":"again","version":1,"anchor":{"kind":"text","text":"hello"}}`)
	rec = do(t, h, http.MethodDelete, "/v1/comments?slug=wdoc&all=1",
		map[string]string{octoUIDHeaderName: "admin-uid", octoRoleHeaderName: "superAdmin"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("superAdmin wipe = %d; want 200: %s", rec.Code, rec.Body.String())
	}
}

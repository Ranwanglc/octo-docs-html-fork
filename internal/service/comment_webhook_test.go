package service_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/core"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/eventwebhook"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

// getMetaCounter wraps MetadataStore to count GetMeta calls. Everything else
// delegates via interface embedding so tests still get the memory backend's
// real semantics — only the one method we're asserting on is instrumented.
type getMetaCounter struct {
	storage.MetadataStore
	n atomic.Int64
}

func (g *getMetaCounter) GetMeta(ctx context.Context, slug string) (*storage.DocMeta, error) {
	g.n.Add(1)
	return g.MetadataStore.GetMeta(ctx, slug)
}

func (g *getMetaCounter) count() int64 { return g.n.Load() }

// stubNotifier captures fire calls synchronously so tests don't race on
// goroutines. The mu guards fires under -race.
type stubNotifier struct {
	mu    sync.Mutex
	fires []eventwebhook.Event
}

func (s *stubNotifier) Fire(_ context.Context, ev eventwebhook.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fires = append(s.fires, ev)
}

func (s *stubNotifier) events() []eventwebhook.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]eventwebhook.Event, len(s.fires))
	copy(out, s.fires)
	return out
}

// waitForFires polls until the notifier has recorded at least n events or
// deadline hits. Needed because fireCommentCreated now dispatches on a
// detached goroutine (B4) — synchronous readback right after Create races.
func (s *stubNotifier) waitForFires(t *testing.T, n int) []eventwebhook.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := s.events(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := s.events()
	t.Fatalf("waitForFires: got %d, want ≥ %d", len(got), n)
	return got
}

// seedMeta primes a slug's DocMeta so docTitle lookups return a value.
func seedMeta(t *testing.T, store storage.MetadataStore, slug, title string) {
	t.Helper()
	if err := store.PutMeta(t.Context(), slug, storage.DocMeta{Slug: slug, Title: title}); err != nil {
		t.Fatalf("PutMeta: %v", err)
	}
}

func newCommentsWithNotifier(t *testing.T, n eventwebhook.Notifier) (*service.CommentService, storage.MetadataStore) {
	t.Helper()
	store := memory.New()
	locker := sluglock.NewMemory()
	cs := service.NewCommentService(store, locker).WithEventWebhook(n, "https://docs.example.com", nil)
	return cs, store
}

func TestCommentServiceFiresOnCreateSuccess(t *testing.T) {
	notif := &stubNotifier{}
	cs, store := newCommentsWithNotifier(t, notif)
	seedMeta(t, store, "my-doc", "季度复盘")

	author := &core.Author{Login: "u123", Name: "张三"}
	res, err := cs.Create(t.Context(), "my-doc", author, "第 3 节评论", nil, 1)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Status != 200 {
		t.Fatalf("Status = %d, want 200 (body=%v)", res.Status, res.Body)
	}
	fires := notif.waitForFires(t, 1)
	if len(fires) != 1 {
		t.Fatalf("fire count = %d, want 1", len(fires))
	}
	ev := fires[0]
	if ev.EventType != eventwebhook.EventTypeCommentCreated {
		t.Errorf("event_type = %q", ev.EventType)
	}
	if ev.Slug != "my-doc" {
		t.Errorf("slug = %q", ev.Slug)
	}
	if ev.Actor.UID != "u123" || ev.Actor.Name != "张三" {
		t.Errorf("actor = %#v", ev.Actor)
	}
	if ev.Doc.Title != "季度复盘" {
		t.Errorf("doc.title = %q", ev.Doc.Title)
	}
	// Deep-link is exactly "<baseURL>/d/<slug>#<comment.id>" — the fragment
	// must equal comment.id byte-for-byte so the server-side receiver can
	// slice URL.fragment and match payload.comment.id without any prefix
	// stripping (OCT-137/A anchor contract, B1).
	if want := "https://docs.example.com/d/my-doc#" + ev.Comment.ID; ev.Doc.URL != want {
		t.Errorf("doc.url = %q, want %q (fragment must equal comment.id)", ev.Doc.URL, want)
	}
	if ev.Comment.Text != "第 3 节评论" {
		t.Errorf("comment.text = %q", ev.Comment.Text)
	}
	if ev.Comment.ID == "" || ev.Comment.CreatedAt == "" {
		t.Errorf("comment id/at empty: %#v", ev.Comment)
	}
}

func TestCommentServiceFiresOnReplySuccess(t *testing.T) {
	notif := &stubNotifier{}
	cs, store := newCommentsWithNotifier(t, notif)
	seedMeta(t, store, "my-doc", "季度复盘")

	// Seed a parent comment. Create bumps the notifier by one; drop it so the
	// reply assertion below reads a clean slate.
	author := &core.Author{Login: "u1"}
	res, err := cs.Create(t.Context(), "my-doc", author, "root", nil, 1)
	if err != nil || res.Status != 200 {
		t.Fatalf("seed create: %v status=%d", err, res.Status)
	}
	parentID := notif.waitForFires(t, 1)[0].Comment.ID
	notif.mu.Lock()
	notif.fires = nil
	notif.mu.Unlock()

	res, err = cs.Reply(t.Context(), "my-doc", parentID, &core.Author{Login: "u2", Name: "李四"}, "reply", 1)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if res.Status != 200 {
		t.Fatalf("Status = %d (body=%v)", res.Status, res.Body)
	}
	fires := notif.waitForFires(t, 1)
	if len(fires) != 1 {
		t.Fatalf("reply fire count = %d", len(fires))
	}
	if fires[0].Actor.UID != "u2" {
		t.Errorf("actor uid = %q, want u2", fires[0].Actor.UID)
	}
	if fires[0].Comment.Text != "reply" {
		t.Errorf("comment.text = %q", fires[0].Comment.Text)
	}
}

func TestCommentServiceDoesNotFireWhenApplyOpFails(t *testing.T) {
	notif := &stubNotifier{}
	cs, _ := newCommentsWithNotifier(t, notif)
	// Reply to a nonexistent parent ⇒ ApplyCommentOp returns non-200 and the
	// store is not written; notifier must stay silent so we never leak a
	// "created" event when nothing was actually persisted.
	res, err := cs.Reply(t.Context(), "d", "c-does-not-exist", &core.Author{Login: "u"}, "x", 1)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if res.Status == 200 {
		t.Fatalf("expected non-200 for missing parent, got %d (body=%v)", res.Status, res.Body)
	}
	time.Sleep(50 * time.Millisecond)
	if got := notif.events(); len(got) != 0 {
		t.Errorf("expected no fires on apply-op failure, got %d", len(got))
	}
}

func TestCommentServiceNilNotifierIsSilent(t *testing.T) {
	// The whole point of the nil-notifier branch: no crash, no wire attempt
	// when the deploy left OCTO_WEBHOOK_URL unset. This is the "webhook off"
	// contract the runbook + PR desc call out.
	store := memory.New()
	locker := sluglock.NewMemory()
	cs := service.NewCommentService(store, locker) // no WithEventWebhook
	res, err := cs.Create(t.Context(), "d", &core.Author{Login: "u"}, "hi", nil, 1)
	if err != nil || res.Status != 200 {
		t.Fatalf("Create failed: err=%v status=%d", err, res.Status)
	}
}

func TestCommentServiceAnonymousActorEmpty(t *testing.T) {
	// Anonymous flow (session==nil ⇒ author==nil from authorFromSession).
	// Server contract accepts empty actor.uid; we must not panic on nil.
	notif := &stubNotifier{}
	cs, _ := newCommentsWithNotifier(t, notif)
	res, err := cs.Create(t.Context(), "d", nil, "anon", nil, 1)
	if err != nil || res.Status != 200 {
		t.Fatalf("Create failed: err=%v status=%d", err, res.Status)
	}
	fires := notif.waitForFires(t, 1)
	if len(fires) != 1 {
		t.Fatalf("fire count = %d", len(fires))
	}
	if fires[0].Actor.UID != "" || fires[0].Actor.Name != "" {
		t.Errorf("actor should be empty for nil author, got %#v", fires[0].Actor)
	}
}

// TestNilNotifierViaNewNoDBRead is the watchdog for the nil-interface boxing
// trap (B3). Wire the way production does — eventwebhook.New with an empty
// URL returns a nil *Client, and the cmd/ layer must NOT box that into the
// Notifier interface; if it did, s.notify == nil would be false and every
// comment would incur a docTitle → GetMeta round-trip on the request path.
// This test proves the disabled-webhook path stays cold: zero GetMeta calls
// after a successful Create.
func TestNilNotifierViaNewNoDBRead(t *testing.T) {
	// Simulate the production wiring pattern from cmd/octo-doc/commands.go:
	// if url is unset, leave the interface as an untyped nil.
	var notifier eventwebhook.Notifier
	if url := ""; url != "" { // deliberately unset — mirrors OCTO_WEBHOOK_URL="" prod deploy
		notifier = eventwebhook.New(url, "", nil)
	}

	spy := &getMetaCounter{MetadataStore: memory.New()}
	locker := sluglock.NewMemory()
	cs := service.NewCommentService(spy, locker).WithEventWebhook(notifier, "https://x", nil)

	res, err := cs.Create(t.Context(), "d", &core.Author{Login: "u"}, "hi", nil, 1)
	if err != nil || res.Status != 200 {
		t.Fatalf("Create failed: err=%v status=%d", err, res.Status)
	}
	// Zero: no docTitle lookup should fire when webhook is disabled.
	if got := spy.count(); got != 0 {
		t.Fatalf("GetMeta calls = %d, want 0 (nil-interface guard defeated — B3 regression)", got)
	}
}

// TestFireCommentCreatedDoesNotBlockRequestPath is B4's watchdog: with a real
// notifier attached, the request-side Create must return WITHOUT paying the
// docTitle → GetMeta round-trip. The lookup happens on the detached goroutine.
// We assert twice: right after Create (must be 0) and after a short wait
// (allowed to reach 1) so a regression that puts the DB call back on the
// request path is caught in the first assertion.
func TestFireCommentCreatedDoesNotBlockRequestPath(t *testing.T) {
	notif := &stubNotifier{}
	spy := &getMetaCounter{MetadataStore: memory.New()}
	locker := sluglock.NewMemory()
	cs := service.NewCommentService(spy, locker).WithEventWebhook(notif, "https://x", nil)
	if err := spy.PutMeta(t.Context(), "d", storage.DocMeta{Slug: "d", Title: "T"}); err != nil {
		t.Fatalf("seed PutMeta: %v", err)
	}
	before := spy.count()

	res, err := cs.Create(t.Context(), "d", &core.Author{Login: "u"}, "hi", nil, 1)
	if err != nil || res.Status != 200 {
		t.Fatalf("Create failed: err=%v status=%d", err, res.Status)
	}
	// Immediately: goroutine may not have scheduled yet, so GetMeta count MUST
	// still equal the pre-Create baseline. A regression that inlines docTitle
	// bumps this by one and trips here.
	if got := spy.count(); got != before {
		t.Fatalf("GetMeta count after Create = %d, want %d (docTitle leaked onto request path — B4 regression)", got, before)
	}
	// Then wait for the goroutine to fire, up to 2s. Fire happening at all
	// proves the goroutine ran; we don't strictly need the count for that, but
	// asserting it reached 1 confirms docTitle really did run in the background
	// rather than being silently skipped.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(notif.events()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if fires := notif.events(); len(fires) != 1 {
		t.Fatalf("expected 1 fire after wait, got %d", len(fires))
	}
	if got := spy.count(); got != before+1 {
		t.Fatalf("GetMeta count after fire = %d, want %d (docTitle should run in goroutine)", got, before+1)
	}
	if fires := notif.events(); fires[0].Doc.Title != "T" {
		t.Errorf("doc.title = %q, want %q", fires[0].Doc.Title, "T")
	}
}

// TestCommentServiceDoesNotFireOnReplyToReplyID complements the missing-parent
// case with a subtler apply-fail: the parent id actually exists in the tree,
// but as a *reply seed*, not a top-level comment. opReply's findByID walks
// only top-level, so it returns 404 and the store is not written; notifier
// must stay silent. Same guarantee, different apply-fail branch — pins the
// "no fire unless status==200" invariant against future refactors of opReply
// (N1).
func TestCommentServiceDoesNotFireOnReplyToReplyID(t *testing.T) {
	notif := &stubNotifier{}
	cs, _ := newCommentsWithNotifier(t, notif)

	// Seed: parent comment + one reply. Grab the reply id and use it as the
	// parent id for a second Reply — should 404 apply-side.
	res, err := cs.Create(t.Context(), "d", &core.Author{Login: "u"}, "root", nil, 1)
	if err != nil || res.Status != 200 {
		t.Fatalf("seed Create: err=%v status=%d", err, res.Status)
	}
	parentID := notif.waitForFires(t, 1)[0].Comment.ID

	res, err = cs.Reply(t.Context(), "d", parentID, &core.Author{Login: "u"}, "r1", 1)
	if err != nil || res.Status != 200 {
		t.Fatalf("seed Reply: err=%v status=%d", err, res.Status)
	}
	replyID := notif.waitForFires(t, 2)[1].Comment.ID

	// Snapshot the count with proper locking so the -race detector is happy
	// even if a straggler goroutine writes after we start reading.
	notif.mu.Lock()
	baseline := len(notif.fires)
	notif.mu.Unlock()

	// Now try to reply *to the reply* — opReply.findByID walks top-level only,
	// so parent lookup misses and we get 404.
	res, err = cs.Reply(t.Context(), "d", replyID, &core.Author{Login: "u"}, "should-fail", 1)
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if res.Status == 200 {
		t.Fatalf("expected non-200 for reply-to-reply, got %d (body=%v)", res.Status, res.Body)
	}
	time.Sleep(50 * time.Millisecond)
	notif.mu.Lock()
	after := len(notif.fires)
	notif.mu.Unlock()
	if after != baseline {
		t.Errorf("fire count changed after apply-fail: baseline=%d after=%d", baseline, after)
	}
}

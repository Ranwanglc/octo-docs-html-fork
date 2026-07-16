package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/lml2468/octo-doc/internal/core"
	"github.com/lml2468/octo-doc/internal/platform/sluglock"
	"github.com/lml2468/octo-doc/internal/service/eventwebhook"
	"github.com/lml2468/octo-doc/internal/storage"
)

// CommentService is the serialized owner of per-slug comment mutations. All
// writes for a slug run under a per-slug lock, making read→apply→write atomic.
// Reads fold the stored log.
type CommentService struct {
	meta    storage.MetadataStore
	lock    sluglock.Locker
	notify  eventwebhook.Notifier
	baseURL string
	logger  *slog.Logger
	// fireHook is a seam: tests replace it with a synchronous shim to make
	// barrier / DB-read-count assertions deterministic. Production leaves it
	// pointing at fireAsync (installed by WithEventWebhook).
	fireHook func(slug, id string, author *core.Author, text, at string)
}

// NewCommentService constructs a CommentService.
func NewCommentService(meta storage.MetadataStore, lock sluglock.Locker) *CommentService {
	return &CommentService{meta: meta, lock: lock}
}

// WithEventWebhook attaches the doc-side comment-event notifier + the doc base
// URL used to build actor-facing links in the payload. Both are optional;
// nil notifier ⇒ webhook disabled ⇒ no goroutine ever fires. Returned to keep
// wiring in cmd/ a one-liner.
func (s *CommentService) WithEventWebhook(n eventwebhook.Notifier, baseURL string, logger *slog.Logger) *CommentService {
	if s == nil {
		return nil
	}
	s.notify = n
	s.baseURL = strings.TrimRight(baseURL, "/")
	s.logger = logger
	s.fireHook = s.fireAsync
	return s
}

// MutationResult is the HTTP-shaped result of a serialized comment mutation.
type MutationResult struct {
	Status int
	Body   any
}

// isoLayout is the millisecond-precision UTC timestamp layout used for all
// service-written timestamps (comment events, asset Created). Shared so writers
// (nowISO) and parsers (gc.withinGrace) can never drift apart.
const isoLayout = "2006-01-02T15:04:05.000Z"

func nowISO() string {
	return time.Now().UTC().Format(isoLayout)
}

// List folds a slug's comments to a version snapshot, or the full history when
// version is core.VersionLatest.
func (s *CommentService) List(ctx context.Context, slug string, version int) ([]core.CommentSnapshot, error) {
	list, err := s.read(ctx, slug)
	if err != nil {
		return nil, err
	}
	if version == core.VersionLatest {
		return core.HistoryList(list), nil
	}
	return core.SnapshotList(list, version), nil
}

// Read returns the migrated raw comment list for a slug (callers fold it).
func (s *CommentService) Read(ctx context.Context, slug string) ([]core.Comment, error) {
	return s.read(ctx, slug)
}

func (s *CommentService) read(ctx context.Context, slug string) ([]core.Comment, error) {
	var list []core.Comment
	err := s.lock.With(ctx, slug, func() error {
		l, lerr := s.meta.GetComments(ctx, slug)
		if lerr != nil {
			return lerr
		}
		core.EnsureMigrated(l)
		list = l
		return nil
	})
	return list, err
}

// Create adds a top-level comment.
func (s *CommentService) Create(ctx context.Context, slug string, author *core.Author, text string, anchor *core.Anchor, version int) (MutationResult, error) {
	now := nowISO()
	id := newCommentID(now)
	res, err := s.mutate(ctx, slug, core.CommentOp{
		Kind: "create", ID: id, Author: author, Text: text, Anchor: anchor, Version: version, At: now,
	})
	// Only fire on the success path (200); ApplyCommentOp signals validation /
	// conflict failures via non-200 Status without an err. Post-persist so a
	// notified event is guaranteed to reflect what's readable in the store.
	if err == nil && res.Status == 200 {
		s.fireCommentCreated(ctx, slug, id, author, text, now)
	}
	return res, err
}

// Reply adds a reply to a parent comment.
func (s *CommentService) Reply(ctx context.Context, slug, parentID string, author *core.Author, text string, version int) (MutationResult, error) {
	now := nowISO()
	replyID := newReplyID(now)
	res, err := s.mutate(ctx, slug, core.CommentOp{
		Kind: "reply", ParentID: parentID, ReplyID: replyID, Author: author, Text: text, Version: version, At: now,
	})
	if err == nil && res.Status == 200 {
		// Same event_type for top-level + reply: server side treats a comment
		// creation uniformly and does not currently distinguish reply threads.
		s.fireCommentCreated(ctx, slug, replyID, author, text, now)
	}
	return res, err
}

// React toggles an emoji reaction on a comment or reply.
func (s *CommentService) React(ctx context.Context, slug, commentID, emoji, by string, version int) (MutationResult, error) {
	return s.mutate(ctx, slug, core.CommentOp{
		Kind: "react", CommentID: commentID, Emoji: emoji, By: by, Version: version, At: nowISO(),
	})
}

// Reanchor re-anchors a comment (resetting its agent verdict).
func (s *CommentService) Reanchor(ctx context.Context, slug, id string, anchor *core.Anchor, version int, actor string) (MutationResult, error) {
	return s.mutate(ctx, slug, core.CommentOp{
		Kind: "patch_anchor", ID: id, Anchor: anchor, ResetStatus: true, Version: version, Actor: actor, At: nowISO(),
	})
}

// Remove soft-deletes a comment or reply at a version.
func (s *CommentService) Remove(ctx context.Context, slug, id string, version int, actor string) (MutationResult, error) {
	return s.mutate(ctx, slug, core.CommentOp{
		Kind: "delete", ID: id, Version: version, Actor: actor, At: nowISO(),
	})
}

// AppendRaw appends pre-built events to a comment (agent reply path).
func (s *CommentService) AppendRaw(ctx context.Context, slug, id string, events []core.CommentEvent, responseBody any) (MutationResult, error) {
	return s.mutate(ctx, slug, core.CommentOp{
		Kind: "raw_events", ID: id, Events: events, ResponseBody: responseBody, At: nowISO(),
	})
}

// Wipe removes all comments for a slug.
func (s *CommentService) Wipe(ctx context.Context, slug string) (MutationResult, error) {
	return s.mutate(ctx, slug, wipeOp())
}

// WipeLocked is Wipe for a caller that ALREADY holds the per-slug lock (e.g.
// DocService.Remove, which serializes the whole delete under one lock). It must
// not re-acquire the lock — sluglock.Memory is not reentrant.
func (s *CommentService) WipeLocked(ctx context.Context, slug string) (MutationResult, error) {
	return s.applyOp(ctx, slug, wipeOp())
}

func wipeOp() core.CommentOp { return core.CommentOp{Kind: "wipe", At: nowISO()} }

// PublishMergeLocked performs the publish-time non-destructive merge + anchor
// reconcile for a caller that ALREADY holds the per-slug lock (DocService.Publish
// serializes the whole publish sequence under one lock). It must not re-acquire
// the lock — sluglock.Memory is not reentrant, so doing so would self-deadlock.
func (s *CommentService) PublishMergeLocked(ctx context.Context, slug string, local []core.Comment, aids []core.StampedArtifact, version int) (MutationResult, error) {
	return s.applyOp(ctx, slug, core.CommentOp{
		Kind: "publish_merge", LocalComments: local, AIDs: aids, Version: version, At: nowISO(),
	})
}

// mutate runs a comment op under the per-slug lock, persisting on success.
func (s *CommentService) mutate(ctx context.Context, slug string, op core.CommentOp) (MutationResult, error) {
	var res MutationResult
	err := s.lock.With(ctx, slug, func() error {
		r, aerr := s.applyOp(ctx, slug, op)
		res = r
		return aerr
	})
	return res, err
}

// applyOp performs one comment op's read→apply→write WITHOUT taking the lock. The
// caller must hold the per-slug lock (via mutate or an outer DocService lock).
func (s *CommentService) applyOp(ctx context.Context, slug string, op core.CommentOp) (MutationResult, error) {
	list, lerr := s.meta.GetComments(ctx, slug)
	if lerr != nil {
		return MutationResult{}, lerr
	}
	newList, opRes := core.ApplyCommentOp(list, op)
	if opRes.Status == 200 {
		if opRes.Wipe {
			if derr := s.meta.DeleteComments(ctx, slug); derr != nil {
				return MutationResult{}, derr
			}
		} else if perr := s.meta.PutComments(ctx, slug, newList); perr != nil {
			return MutationResult{}, perr
		}
	}
	return MutationResult{Status: opRes.Status, Body: opRes.Body}, nil
}

// fireCommentCreated schedules the doc-side comment.created event on a
// detached goroutine. Best-effort: notifier==nil short-circuits (webhook
// disabled) BEFORE the goroutine spawns; doc.Title lookup runs in the
// background and its failure is not fatal (title stays empty); Fire itself is
// also fire-and-forget so a slow notifier never bleeds into the request path.
// Uses context.Background() (with the eventwebhook timeout) — the request ctx
// is about to be cancelled once mutate returns.
func (s *CommentService) fireCommentCreated(_ context.Context, slug, id string, author *core.Author, text, at string) {
	if s == nil || s.notify == nil {
		return
	}
	s.fireHook(slug, id, author, text, at)
}

// fireAsync is the default fireHook: title lookup + Fire on a detached
// goroutine so the request path pays neither the DB round-trip nor the HTTP
// dispatch. Tests may override fireHook to run inline for deterministic
// assertions.
func (s *CommentService) fireAsync(slug, id string, author *core.Author, text, at string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		title := s.docTitle(ctx, slug)
		s.notify.Fire(ctx, eventwebhook.Event{
			EventType: eventwebhook.EventTypeCommentCreated,
			Slug:      slug,
			Actor:     actorFromAuthor(author),
			Doc: eventwebhook.Doc{
				Title: title,
				URL:   s.docURL(slug, id),
			},
			Comment: eventwebhook.Comment{ID: id, Text: text, CreatedAt: at},
		})
	}()
}

// docTitle pulls the current title for a slug; empty on absence or error. Not
// a fatal lookup — the payload's title is a nicety, not a required field.
func (s *CommentService) docTitle(ctx context.Context, slug string) string {
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil || meta == nil {
		return ""
	}
	return meta.Title
}

// docURL returns a deep link to the comment. Uses the configured BaseURL when
// set (public URL of the doc host) and falls back to a bare "/d/{slug}"
// relative path so a misconfigured deploy still gives the server something
// human-readable rather than a naked slug. Fragment is exactly comment.id —
// no prefix — so server-side receiver can slice the URL fragment and match
// payload.comment.id byte-for-byte (OCT-137/A anchor contract).
func (s *CommentService) docURL(slug, id string) string {
	path := "/d/" + slug + "#" + id
	if s.baseURL == "" {
		return path
	}
	return s.baseURL + path
}

// actorFromAuthor collapses the doc-side Author record onto the webhook Actor
// shape. Anonymous comments (nil author) emit an empty uid; the server-side
// contract accepts empty and renders "anonymous".
func actorFromAuthor(a *core.Author) eventwebhook.Actor {
	if a == nil {
		return eventwebhook.Actor{}
	}
	return eventwebhook.Actor{UID: a.Login, Name: a.Name}
}

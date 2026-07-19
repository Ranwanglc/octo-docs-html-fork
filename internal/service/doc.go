package service

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/core"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// DocService handles publish, render-data, version listing, and deletion of
// documents. Publishing is the critical path: stamp artifacts (byte-equivalent
// to upstream), write the immutable blob, bump the monotonic version list, and
// reconcile/merge comments.
type DocService struct {
	blobs    storage.BlobStore
	meta     storage.MetadataStore
	comments *CommentService
	lock     sluglock.Locker
	baseURL  string
	maxBytes int64

	register DocRegistrar
	logger   *slog.Logger
}

// DocRegistrar is the docs-backend side-effect sink. Implementations must be
// nil-safe and best-effort: registration failure must never fail doc writes.
// The token argument is the publishing bot's own bearer token (empty ⇒ the
// implementation falls back to its process-configured token).
type DocRegistrar interface {
	Register(ctx context.Context, reg docsbackend.Registration, token string)
	Rename(ctx context.Context, slug, title, token string)
	Delete(ctx context.Context, slug, token string)
}

// NewDocService constructs a DocService. The locker MUST be the same instance the
// CommentService uses, so that a publish (which holds the slug lock across the
// whole resolve→put→meta→merge sequence) is serialized against comment mutations
// for the same slug.
func NewDocService(blobs storage.BlobStore, meta storage.MetadataStore, comments *CommentService, lock sluglock.Locker, baseURL string, maxBytes int64) *DocService {
	return &DocService{blobs: blobs, meta: meta, comments: comments, lock: lock, baseURL: baseURL, maxBytes: maxBytes}
}

// WithDocsBackendRegistration attaches the optional docs-backend registrar.
// Mount info for each registration comes from the publish request (the bot
// tells us where it is publishing), so no doc_binding lookup is performed here.
func (s *DocService) WithDocsBackendRegistration(r DocRegistrar, logger *slog.Logger) *DocService {
	if s == nil {
		return nil
	}
	if isNilInterface(r) {
		r = nil
	}
	s.register = r
	if logger == nil {
		logger = slog.Default()
	}
	s.logger = logger
	return s
}

func isNilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

// PublishInput is the input to Publish.
type PublishInput struct {
	Slug          string
	HTML          string
	Version       int // 0 = auto-increment
	Title         string
	LocalComments []core.Comment

	// Mount info supplied by the publishing bot, forwarded verbatim to
	// docs-backend registration. Replaces the old GET doc_binding lookup: the
	// caller (bot) knows where it is publishing, so no user-token binding query
	// is needed. Empty MountType ⇒ registration is skipped (non-mounted doc).
	MountType string // "group" | "space" | "thread" (thread ⇒ skipped)
	GroupNo   string
	ThreadID  string

	// CreatorUID is the publishing bot's uid, stamped into DocMeta on first
	// create only (a republish never reassigns ownership). Empty ⇒ no creator
	// recorded (nobody gets author-by-creator for this doc).
	CreatorUID string

	// PublisherToken is the publishing bot's own bearer token, forwarded to the
	// docs-backend registration so the doc is attributed to whoever published it.
	// Empty ⇒ the registrar falls back to its process-configured token.
	PublisherToken string
}

// PublishResult is the result of a successful publish.
type PublishResult struct {
	Slug           string `json:"slug"`
	Version        int    `json:"version"`
	URL            string `json:"url"`
	Size           int64  `json:"size"`
	AIDs           int    `json:"aids"`
	MergedComments int    `json:"merged_comments"`

	title        string
	hadMeta      bool
	titleChanged bool

	// Mount info carried from PublishInput into afterPublished for registration.
	mountType string
	groupNo   string
	threadID  string

	// publisherToken carries the publishing bot's own token from PublishInput
	// into afterPublished so the async registration authenticates as the publisher.
	publisherToken string
}

// RenderData is the render payload for a document version.
type RenderData struct {
	HTML     string
	Versions []storage.VersionRef
}

const docsBackendSideEffectTimeout = 5 * time.Second

// Publish publishes a new (or explicitly-versioned) document.
func (s *DocService) Publish(ctx context.Context, in PublishInput) (*PublishResult, error) {
	if in.HTML == "" {
		return nil, apperr.Validation("html (file) required", "html_required")
	}
	if int64(len(in.HTML)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
	}

	stamped := core.StampAids(in.HTML)

	// Hold the per-slug lock across the whole critical section: version resolution,
	// the immutable blob write, the version-list bump, and the comment merge must be
	// atomic, or two concurrent publishes of the same slug can resolve to the same
	// version and clobber each other (and drift meta vs blobs).
	var result *PublishResult
	err := s.lock.With(ctx, in.Slug, func() error {
		r, perr := s.publishLocked(ctx, in, stamped)
		result = r
		return perr
	})
	if err != nil {
		return nil, err
	}
	s.afterPublished(result)
	return result, nil
}

// publishLocked runs the publish critical section. The caller MUST hold the
// per-slug lock (Publish does); it therefore uses PublishMergeLocked and never
// re-acquires the lock.
func (s *DocService) publishLocked(ctx context.Context, in PublishInput, stamped core.StampResult) (*PublishResult, error) {
	version, err := s.resolveVersion(ctx, in.Slug, in.Version)
	if err != nil {
		return nil, err
	}

	size, err := s.blobs.PutDoc(ctx, in.Slug, version, stamped.HTML)
	if err != nil {
		return nil, apperr.Upstream("blob write failed", "blob_write_failed", err)
	}
	if _, ok, herr := s.blobs.HeadDoc(ctx, in.Slug, version); herr != nil {
		return nil, apperr.Upstream("blob head failed", "blob_head_failed", herr)
	} else if !ok {
		return nil, apperr.Upstream("blob write did not persist", "blob_write_lost", nil)
	}

	metaResult, err := s.upsertMeta(ctx, in, version)
	if err != nil {
		return nil, err
	}

	merge, err := s.comments.PublishMergeLocked(ctx, in.Slug, in.LocalComments, stamped.AIDs, version)
	if err != nil {
		return nil, err
	}
	merged := 0
	if body, ok := merge.Body.(map[string]any); ok {
		if m, ok := body["mergedComments"].(int); ok {
			merged = m
		}
	}

	return &PublishResult{
		Slug:           in.Slug,
		Version:        version,
		URL:            fmt.Sprintf("%s/d/%s/v/%d", s.baseURL, in.Slug, version),
		Size:           size,
		AIDs:           len(stamped.AIDs),
		MergedComments: merged,
		title:          metaResult.title,
		hadMeta:        metaResult.hadMeta,
		titleChanged:   metaResult.titleChanged,
		mountType:      in.MountType,
		groupNo:        in.GroupNo,
		threadID:       in.ThreadID,
		publisherToken: in.PublisherToken,
	}, nil
}

// ElementView is the outer HTML of a single artifact located by aid.
type ElementView struct {
	AID  string `json:"aid"`
	Tag  string `json:"tag"`
	HTML string `json:"html"`
}

// GetElement renders the requested version (0 = latest) and returns the outer
// HTML of the artifact stamped with aid. NotFound if the version or the aid is
// absent. Reuses core parse logic (ElementByAID) rather than re-parsing here.
func (s *DocService) GetElement(ctx context.Context, slug string, version int, aid string) (*ElementView, error) {
	if aid == "" {
		return nil, apperr.Validation("aid required", "aid_required")
	}
	v, err := s.resolveReadVersion(ctx, slug, version)
	if err != nil {
		return nil, err
	}
	rd, err := s.Render(ctx, slug, v)
	if err != nil {
		return nil, err
	}
	if rd == nil {
		return nil, apperr.NotFound("document version not found")
	}
	outer, tag, ok := core.ElementByAID(rd.HTML, aid)
	if !ok {
		return nil, apperr.NotFound("aid not found in this version")
	}
	return &ElementView{AID: aid, Tag: tag, HTML: outer}, nil
}

// ReplaceElement swaps the outer HTML of the artifact identified by aid in the
// base version (0 = latest) with newHTML, then republishes the whole document as
// a new version. The entire resolve→render→replace→stamp→publish sequence runs
// under a SINGLE per-slug lock so it cannot lose an update: without the lock, a
// concurrent publish between our read and our write would be silently clobbered
// (we'd base on v1, mint v3, and drop the intervening v2). We call publishLocked
// (not Publish) inside the lock to avoid re-entering lock.With (deadlock), and we
// stamp here (StampAids) since publishLocked takes an already-stamped result.
// newHTML must be a single top-level element (no multiple elements, no
// script/style, no inline event handlers, no javascript: URLs, no data-odoc-*).
func (s *DocService) ReplaceElement(ctx context.Context, slug string, baseVersion int, aid, newHTML string) (*PublishResult, error) {
	if aid == "" {
		return nil, apperr.Validation("aid required", "aid_required")
	}
	if newHTML == "" {
		return nil, apperr.Validation("new_html required", "new_html_required")
	}
	if int64(len(newHTML)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("new_html exceeds %d bytes", s.maxBytes), "new_html_too_large")
	}
	// Guard against boundary escape and injection: the replacement must be exactly
	// one element (open+close or a void tag), not a raw-text/script fragment, and
	// carry no event handlers / javascript: URLs.
	if _, ok := core.SafeReplacementFragment(newHTML); !ok {
		return nil, apperr.Validation("new_html must be a single safe element fragment", "new_html_not_single_element")
	}
	// Reject stamper-owned attributes in hand-written HTML: Publish re-stamps only
	// stampable open tags, so a leftover data-odoc-* would create an ambiguous DOM
	// selector. Rejecting outright is the safest contract.
	if core.HasDataOdocAttr(newHTML) {
		return nil, apperr.Validation("new_html must not carry data-odoc-* attributes", "new_html_has_odoc_attr")
	}

	var result *PublishResult
	err := s.lock.With(ctx, slug, func() error {
		// Resolve→render→replace all inside the lock so the base we edit is the same
		// latest publishLocked will increment from (baseVersion=0 ⇒ no race window;
		// baseVersion>0 ⇒ edit that explicit base, publish as latest+1).
		v, verr := s.resolveReadVersion(ctx, slug, baseVersion)
		if verr != nil {
			return verr
		}
		rd, rerr := s.Render(ctx, slug, v)
		if rerr != nil {
			return rerr
		}
		if rd == nil {
			return apperr.NotFound("document version not found")
		}
		replaced, ok := core.ReplaceElementByAID(rd.HTML, aid, newHTML)
		if !ok {
			return apperr.NotFound("aid not found in this version")
		}
		if int64(len(replaced)) > s.maxBytes {
			return apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
		}
		// Stamp here (publishLocked expects a stamped result) and publish without
		// re-acquiring the lock — Publish would deadlock via a nested lock.With.
		stamped := core.StampAids(replaced)
		r, perr := s.publishLocked(ctx, PublishInput{Slug: slug, HTML: replaced}, stamped)
		if perr != nil {
			return perr
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.afterPublished(result)
	return result, nil
}

// resolveReadVersion turns an explicit version (0 = latest) into a concrete
// version number for reads, using the same monotonic-max rule as publish.
func (s *DocService) resolveReadVersion(ctx context.Context, slug string, explicit int) (int, error) {
	if explicit > 0 {
		return explicit, nil
	}
	existing, err := s.blobs.ListVersions(ctx, slug)
	if err != nil {
		return 0, err
	}
	maxV := 0
	for _, n := range existing {
		if n > maxV {
			maxV = n
		}
	}
	if maxV == 0 {
		return 0, apperr.NotFound("no published version for " + slug)
	}
	return maxV, nil
}

// Render fetches stored HTML + the version list for rendering, or nil if absent.
func (s *DocService) Render(ctx context.Context, slug string, version int) (*RenderData, error) {
	html, ok, err := s.blobs.GetDoc(ctx, slug, version)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return nil, err
	}
	var versions []storage.VersionRef
	if meta != nil {
		versions = meta.Versions
	}
	return &RenderData{HTML: html, Versions: versions}, nil
}

// VersionList is the response of ListVersions.
type VersionList struct {
	Slug     string               `json:"slug"`
	Title    string               `json:"title"`
	Versions []storage.VersionRef `json:"versions"`
}

// DraftResult is the result of saving a draft.
type DraftResult struct {
	Slug string `json:"slug"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
	AIDs int    `json:"aids"`
}

// SaveDraft stamps and writes the mutable draft slot for a slug, creating the
// meta record if the slug is new (draft-only docs have an empty Versions list).
// The draft never enters the immutable version numbering until Promote.
//
// creatorUID is stamped into meta on first create only (draft-first ownership),
// exactly like Publish; a later save by a different caller never reassigns it,
// and the stamped creator carries through to the promoted version.
func (s *DocService) SaveDraft(ctx context.Context, slug, html, title, creatorUID string) (*DraftResult, error) {
	if html == "" {
		return nil, apperr.Validation("html required", "html_required")
	}
	if int64(len(html)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
	}
	stamped := core.StampAids(html)
	var result *DraftResult
	err := s.lock.With(ctx, slug, func() error {
		size, perr := s.blobs.PutDraft(ctx, slug, stamped.HTML)
		if perr != nil {
			return apperr.Upstream("draft write failed", "draft_write_failed", perr)
		}
		if merr := s.setDraftMeta(ctx, slug, title, creatorUID); merr != nil {
			return merr
		}
		result = &DraftResult{
			Slug: slug,
			URL:  fmt.Sprintf("%s/d/%s/draft", s.baseURL, slug),
			Size: size,
			AIDs: len(stamped.AIDs),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetDraft fetches the draft HTML + version list for rendering, or nil if absent.
func (s *DocService) GetDraft(ctx context.Context, slug string) (*RenderData, error) {
	html, ok, err := s.blobs.GetDraft(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return nil, err
	}
	var versions []storage.VersionRef
	if meta != nil {
		versions = meta.Versions
	}
	return &RenderData{HTML: html, Versions: versions}, nil
}

// Promote turns the current draft into a new immutable version via the normal
// publish path (monotonic maxV+1), then clears the draft blob + meta marker. It
// holds the per-slug lock across the whole sequence so it can't race a publish.
//
// publishLocked is the point of no return: once it succeeds the version is durably
// committed and cannot be rolled back. Clearing the draft afterwards is best-effort
// cleanup — if it fails we log and still return success, because reporting a failure
// would invite a retry that re-runs publishLocked and mints a duplicate version. A
// leftover draft blob is harmless: it's invisible to ListVersions and is overwritten
// by the next SaveDraft.
func (s *DocService) Promote(ctx context.Context, slug, title string) (*PublishResult, error) {
	var result *PublishResult
	err := s.lock.With(ctx, slug, func() error {
		html, ok, gerr := s.blobs.GetDraft(ctx, slug)
		if gerr != nil {
			return gerr
		}
		if !ok {
			return apperr.NotFound("no draft to publish for " + slug)
		}
		stamped := core.StampAids(html)
		r, perr := s.publishLocked(ctx, PublishInput{Slug: slug, HTML: html, Title: title}, stamped)
		if perr != nil {
			return perr
		}
		result = r
		// Best-effort cleanup past the commit point — never fail the promote here.
		if derr := s.blobs.DeleteDraft(ctx, slug); derr != nil {
			slog.Default().Warn("promote: draft blob clear failed (harmless, will be overwritten)",
				"slug", slug, "version", r.Version, "err", derr)
		}
		if merr := s.clearDraftMeta(ctx, slug); merr != nil {
			slog.Default().Warn("promote: draft meta clear failed (harmless)",
				"slug", slug, "version", r.Version, "err", merr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.afterPublished(result)
	return result, nil
}

// setDraftMeta records a draft marker in the meta Extra catch-all, creating the
// meta record if the slug is new. It leaves Versions untouched. creatorUID is
// stamped on first create only (same rule as upsertMeta), never reassigning an
// existing creator.
func (s *DocService) setDraftMeta(ctx context.Context, slug, title, creatorUID string) error {
	prev, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return err
	}
	if prev == nil {
		prev = &storage.DocMeta{Slug: slug, Title: slug, Versions: []storage.VersionRef{}}
	}
	metaTitle := prev.Title
	if title != "" {
		metaTitle = title
	}
	if metaTitle == "" {
		metaTitle = slug
	}
	extra := map[string]any{}
	maps.Copy(extra, prev.Extra)
	extra[storage.DraftExtraKey] = map[string]any{"updated_at": time.Now().UTC().Format(time.RFC3339)}
	if creatorUID != "" && prev.CreatorUID() == "" {
		extra[storage.CreatorUIDExtraKey] = creatorUID
	}
	return s.meta.PutMeta(ctx, slug, storage.DocMeta{
		Slug:     slug,
		Title:    metaTitle,
		Versions: prev.Versions,
		Extra:    extra,
	})
}

// clearDraftMeta removes the draft marker from meta (no-op if none / no meta).
func (s *DocService) clearDraftMeta(ctx context.Context, slug string) error {
	prev, err := s.meta.GetMeta(ctx, slug)
	if err != nil || prev == nil {
		return err
	}
	if _, has := prev.Extra[storage.DraftExtraKey]; !has {
		return nil
	}
	extra := map[string]any{}
	for k, v := range prev.Extra {
		if k != storage.DraftExtraKey {
			extra[k] = v
		}
	}
	if len(extra) == 0 {
		extra = nil
	}
	return s.meta.PutMeta(ctx, slug, storage.DocMeta{
		Slug:     prev.Slug,
		Title:    prev.Title,
		Versions: prev.Versions,
		Extra:    extra,
	})
}

// ListVersions lists versions for a slug (meta-derived, falling back to blobs).
func (s *DocService) ListVersions(ctx context.Context, slug string) (*VersionList, error) {
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return nil, err
	}
	blobVersions, err := s.blobs.ListVersions(ctx, slug)
	if err != nil {
		return nil, err
	}
	if meta == nil && len(blobVersions) == 0 {
		return nil, nil
	}
	title := slug
	var versions []storage.VersionRef
	if meta != nil && len(meta.Versions) > 0 {
		versions = meta.Versions
		if meta.Title != "" {
			title = meta.Title
		}
	} else {
		for _, n := range blobVersions {
			versions = append(versions, storage.VersionRef{N: n})
		}
	}
	return &VersionList{Slug: slug, Title: title, Versions: versions}, nil
}

// Remove deletes all versions, metadata, and comments for a slug. It holds the
// per-slug lock across all three deletes so it is serialized against a concurrent
// Publish of the same slug (which holds the same lock); otherwise a delete could
// interleave with a publish and leave orphaned blobs or meta pointing at a
// missing blob.
func (s *DocService) Remove(ctx context.Context, slug string) error {
	err := s.lock.With(ctx, slug, func() error {
		if err := s.blobs.DeleteDoc(ctx, slug); err != nil {
			return err
		}
		// blobs.DeleteDoc purges asset bytes (they share the doc's key prefix), but
		// the asset metadata rows are a separate store — purge them too, or they'd
		// orphan and resurface if the slug is later reused.
		assets, err := s.meta.ListAssetMeta(ctx, slug)
		if err != nil {
			return err
		}
		for _, a := range assets {
			if derr := s.meta.DeleteAssetMeta(ctx, slug, a.SHA256); derr != nil {
				return derr
			}
		}
		if err := s.meta.DeleteMeta(ctx, slug); err != nil {
			return err
		}
		_, err = s.comments.WipeLocked(ctx, slug)
		return err
	})
	if err != nil {
		return err
	}
	s.afterRemoved(slug)
	return nil
}

func (s *DocService) afterPublished(result *PublishResult) {
	if result == nil || s.register == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), docsBackendSideEffectTimeout)
		defer cancel()
		reg, ok := s.registrationForMount(result.Slug, result.title, result.mountType)
		if !ok {
			return
		}
		s.register.Register(ctx, reg, result.publisherToken)
		if result.hadMeta && result.titleChanged {
			s.register.Rename(ctx, result.Slug, reg.Title, result.publisherToken)
		}
	}()
}

func (s *DocService) afterRemoved(slug string) {
	if s.register == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), docsBackendSideEffectTimeout)
		defer cancel()
		// Delete is by slug and idempotent: docs-backend 404s harmlessly if the
		// slug was never registered. No mount info is needed to unregister, so we
		// call it unconditionally rather than re-deriving a registration. No
		// publisher token is available on the remove path, so "" falls back to the
		// process-configured token.
		s.register.Delete(ctx, slug, "")
	}()
}

// registrationForMount builds a docs-backend registration from mount info the
// publishing bot supplied on the publish request. This replaces the former GET
// /v1/docs/bindings/<slug> lookup (which required a login-user token and 401'd
// on a bot token). SpaceID and Owner are intentionally omitted: docs-backend
// reverse-resolves both from the bot's own token via verify-bot, so the caller
// must not (and need not) supply them.
func (s *DocService) registrationForMount(slug, title, mountType string) (docsbackend.Registration, bool) {
	if s.register == nil {
		return docsbackend.Registration{}, false
	}
	mountType = strings.ToLower(strings.TrimSpace(mountType))
	switch mountType {
	case "":
		s.log().Debug("docs_backend_register skipped: no mount_type", "slug", slug)
		return docsbackend.Registration{}, false
	case "thread":
		s.log().Debug("docs_backend_register skipped: thread mount", "slug", slug)
		return docsbackend.Registration{}, false
	case "group", "space":
	default:
		s.log().Debug("docs_backend_register skipped: unsupported mount_type", "slug", slug, "mount_type", mountType)
		return docsbackend.Registration{}, false
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = slug
	}
	return docsbackend.Registration{
		DocType:     "html",
		OctoDocSlug: slug,
		MountType:   mountType,
		Title:       title,
	}, true
}

func (s *DocService) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// OwnerDoc is one row in the owner catalog. LatestCreated mirrors the newest
// VersionRef.Created (already in store); nil when the doc has no versions.
// *string so JSON callers can omit the field cleanly.
type OwnerDoc struct {
	Slug          string
	Title         string
	Latest        int
	LatestCreated *string
}

// ListAllForOwner lists all docs with a reachable latest version.
func (s *DocService) ListAllForOwner(ctx context.Context) ([]OwnerDoc, error) {
	all, err := s.meta.ListMeta(ctx)
	if err != nil {
		return nil, err
	}
	var out []OwnerDoc
	for _, e := range all {
		latest := 1
		var created *string
		if n := len(e.Meta.Versions); n > 0 {
			latest = e.Meta.Versions[n-1].N
			created = e.Meta.Versions[n-1].Created
		}
		_, ok, herr := s.blobs.HeadDoc(ctx, e.Slug, latest)
		if herr != nil || !ok {
			continue
		}
		title := e.Meta.Title
		if title == "" {
			title = e.Slug
		}
		out = append(out, OwnerDoc{Slug: e.Slug, Title: title, Latest: latest, LatestCreated: created})
	}
	return out, nil
}

func (s *DocService) resolveVersion(ctx context.Context, slug string, explicit int) (int, error) {
	if explicit > 0 {
		return explicit, nil
	}
	existing, err := s.blobs.ListVersions(ctx, slug)
	if err != nil {
		return 0, err
	}
	maxV := 0
	for _, n := range existing {
		if n > maxV {
			maxV = n
		}
	}
	return maxV + 1, nil
}

type publishMetaResult struct {
	title        string
	hadMeta      bool
	titleChanged bool
}

func (s *DocService) upsertMeta(ctx context.Context, in PublishInput, version int) (publishMetaResult, error) {
	prev, err := s.meta.GetMeta(ctx, in.Slug)
	if err != nil {
		return publishMetaResult{}, err
	}
	hadMeta := prev != nil
	if prev == nil {
		prev = &storage.DocMeta{Slug: in.Slug, Title: in.Slug, Versions: []storage.VersionRef{}}
	}
	versions := append([]storage.VersionRef{}, prev.Versions...)
	found := false
	for _, v := range versions {
		if v.N == version {
			found = true
			break
		}
	}
	if !found {
		created := time.Now().UTC().Format(time.RFC3339)
		versions = append(versions, storage.VersionRef{N: version, Created: &created})
	}
	sortVersions(versions)

	title := prev.Title
	if in.Title != "" {
		title = in.Title
	}
	if title == "" {
		title = in.Slug
	}
	// Stamp creator_uid on first create only: ownership is set once and a later
	// republish (possibly by a different caller) must never reassign it.
	extra := prev.Extra
	if in.CreatorUID != "" && prev.CreatorUID() == "" {
		extra = map[string]any{}
		maps.Copy(extra, prev.Extra)
		extra[storage.CreatorUIDExtraKey] = in.CreatorUID
	}
	if err := s.meta.PutMeta(ctx, in.Slug, storage.DocMeta{
		Slug:     in.Slug,
		Title:    title,
		Versions: versions,
		Extra:    extra,
	}); err != nil {
		return publishMetaResult{}, err
	}
	return publishMetaResult{
		title:        title,
		hadMeta:      hadMeta,
		titleChanged: strings.TrimSpace(prev.Title) != "" && prev.Title != title,
	}, nil
}

func sortVersions(v []storage.VersionRef) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1].N > v[j].N; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}

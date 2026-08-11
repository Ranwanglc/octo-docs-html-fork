package service

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
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

	register    DocRegistrar
	reconcileFn GrantReconciler
	logger      *slog.Logger
}

// GrantReconciler drains legacy meta.grants entries into doc_member after
// confirmed registration. Injected (not a hard dep on AuthService) so DocService
// stays testable in isolation and single-node deploys without a mirror wire a
// no-op. Without this hook, a grant issued while the doc is unregistered
// evaporates once registration closes the meta fallback.
type GrantReconciler func(ctx context.Context, slug string) error

// DocRegistrar is the docs-backend side-effect sink.
type DocRegistrar interface {
	Register(ctx context.Context, reg docsbackend.Registration, token string) (*docsbackend.RegistrationResult, error)
	Rename(ctx context.Context, slug, title, token string)
	Delete(ctx context.Context, slug, token string) error
}

// DeleteAuth carries verified bot or HTTP-layer human authorization.
type DeleteAuth struct {
	PublisherToken string
	ActorUID       string
	SuperAdmin     bool
}

type publishNotifier interface {
	Published(ctx context.Context, docID, title, token string) error
}

type lockerProvider interface {
	Locker() sluglock.Locker
}

// NewDocService constructs a DocService. The locker MUST be the same instance the
// CommentService uses. Canonical creates use the metadata store's shared locker.
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

// WithGrantReconciler attaches the reconciler DocService calls after each
// confirmed registration to drain legacy meta.grants entries into doc_member.
// Wired only when both a registrar and a doc_member mirror exist. Returns s.
func (s *DocService) WithGrantReconciler(fn GrantReconciler) *DocService {
	if s == nil {
		return nil
	}
	s.reconcileFn = fn
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

	// Mount info supplied by the publishing bot, normalized and forwarded to
	// docs-backend registration. Replaces the old GET doc_binding lookup: the
	// caller (bot) knows where it is publishing, so no user-token binding query
	// is needed. MountTypePresent distinguishes an omitted field from an explicit
	// empty value; neither implicitly unmounts an existing mounted document.
	MountType        string // "group" | "space" | "thread" (all registerable)
	MountTypePresent bool
	GroupNo          string
	ThreadID         string

	// CreatorUID is the publishing bot's uid, stamped into DocMeta on first
	// create only (a republish never reassigns ownership). Empty ⇒ no creator
	// recorded (nobody gets author-by-creator for this doc).
	CreatorUID string

	// PublisherToken is the publishing bot's own bearer token, forwarded to the
	// docs-backend registration so the doc is attributed to whoever published it.
	// Empty is valid for edits, but cannot allocate a new canonical mounted
	// identity or authenticate backend mutations.
	PublisherToken   string
	PublisherUID     string
	PublisherSpaceID string
	// IdempotencyKey explicitly selects canonical create. Canonical create has no
	// Slug; docs-backend allocates the doc ID before any local write.
	IdempotencyKey string

	mountContextKnown bool
	pinnedAID         string
	pinnedTag         string
	anchorMigrations  map[string]string
	identity          *canonicalIdentity
}

type canonicalIdentity struct {
	docID    string
	shareURL string
}

// PublishResult is the result of a successful publish.
type PublishResult struct {
	Slug string `json:"slug"`

	Version        int    `json:"version"`
	URL            string `json:"url"`
	DocID          string `json:"doc_id"`
	ShareURL       string `json:"share_url"`
	Registered     bool   `json:"registered"`
	Status         string `json:"status"`
	Size           int64  `json:"size"`
	AIDs           int    `json:"aids"`
	MergedComments int    `json:"merged_comments"`

	title        string
	hadMeta      bool
	titleChanged bool

	// Mount info carried through the synchronous post-publish registration step.
	mountType         string
	mountContextKnown bool

	// publisherToken authenticates synchronous registration as the publisher.
	publisherToken string
}

// RenderData is the render payload for a document version.
type RenderData struct {
	HTML     string
	Versions []storage.VersionRef
	// Title is the human title from meta, surfaced so the render handler can seed
	// window.__ODOC__ with it (else the overlay top bar degrades to the slug).
	Title string
}

const (
	docsBackendSideEffectTimeout = 5 * time.Second
	docsBackendAttemptTimeout    = time.Second
	docsBackendRegisterAttempts  = 3
	docsBackendRegisterDelay     = 100 * time.Millisecond
	publishStatusPublished       = "published"
	publishStatusUnregistered    = "published_unregistered"
	publishStatusRegisterFailed  = "registration_failed"
)

// Publish publishes a new (or explicitly-versioned) document.
func (s *DocService) Publish(ctx context.Context, in PublishInput) (*PublishResult, error) {
	return s.PublishAuthorized(ctx, in, nil)
}

// PublishAuthorized publishes after authorize checks the slug's current
// existence while the per-slug lock is held.
func (s *DocService) PublishAuthorized(ctx context.Context, in PublishInput, authorize func(slug string, exists bool) error) (*PublishResult, error) {
	if in.HTML == "" {
		return nil, apperr.Validation("html (file) required", "html_required")
	}
	if int64(len(in.HTML)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
	}
	if in.MountType != "" {
		in.MountTypePresent = true
	}
	mountType, err := normalizeMountType(in.MountType)
	if err != nil {
		return nil, err
	}
	in.MountType = mountType
	in.mountContextKnown = in.MountTypePresent
	explicitCreate := strings.TrimSpace(in.IdempotencyKey) != ""
	if explicitCreate && in.Slug != "" {
		return nil, apperr.Validation("canonical create must not include slug", "create_ref_forbidden")
	}
	if in.Slug == "" && !explicitCreate {
		return nil, apperr.Validation("idempotency_key required for canonical create", "idempotency_key_required")
	}
	if explicitCreate && strings.TrimSpace(in.PublisherToken) == "" {
		return nil, apperr.Unauthorized("canonical create requires bot authentication", "publisher_bot_required")
	}
	exists := false
	if !explicitCreate {
		var err error
		exists, err = s.slugExists(ctx, in.Slug)
		if err != nil {
			return nil, err
		}
		if exists {
			meta, metaErr := s.meta.GetMeta(ctx, in.Slug)
			if metaErr != nil {
				return nil, metaErr
			}
			if persistedMount, ok := meta.MountType(); ok && in.MountType == "" {
				in.MountType, err = normalizeMountType(persistedMount)
				if err != nil {
					return nil, err
				}
				in.mountContextKnown = true
			}
			if docID, shareURL, ok := meta.CanonicalIdentity(); ok {
				in.identity = &canonicalIdentity{docID: docID, shareURL: shareURL}
			}
		} else if authorize != nil {
			return nil, apperr.NotFound("document not found")
		}
	}
	var registration *docsbackend.RegistrationResult
	if explicitCreate {
		var err error
		registration, err = s.preRegister(ctx, in)
		if err != nil {
			return nil, err
		}
		if config.SafeSlug(registration.DocID) == "" || registration.OctoDocSlug != registration.DocID {
			return nil, apperr.Upstream("docs-backend returned invalid document identity", "registration_invalid", nil)
		}
		if (in.PublisherUID != "" || in.PublisherSpaceID != "") && (in.PublisherUID == "" || in.PublisherSpaceID == "" || registration.PublisherUID != in.PublisherUID || registration.SpaceID != in.PublisherSpaceID) {
			return nil, apperr.Forbidden("registration publisher identity mismatch", "registration_identity_mismatch")
		}
		in.Slug = registration.DocID
		in.identity = &canonicalIdentity{docID: registration.DocID, shareURL: registration.ShareURL}
	}

	stamped := core.StampAids(in.HTML)

	// Hold the per-slug lock across the whole critical section: version resolution,
	// the immutable blob write, the version-list bump, and the comment merge must be
	// atomic, or two concurrent publishes of the same slug can resolve to the same
	// version and clobber each other (and drift meta vs blobs).
	var result *PublishResult
	err = s.withCanonicalGuard(ctx, in.Slug, explicitCreate, func() error {
		// Registration allocates the lock key, so it necessarily precedes this
		// critical section. Retry inspection and initialization must both happen
		// here: a Created=false caller waits for the creator and then observes v1.
		if registration != nil {
			existing, xerr := s.existingPublishResult(ctx, in, registration.ShareURL, authorize)
			if xerr != nil {
				return xerr
			}
			if existing != nil {
				result = existing
				return nil
			}
		}
		if authorize != nil {
			exists, xerr := s.slugExists(ctx, in.Slug)
			if xerr != nil {
				return xerr
			}
			if aerr := authorize(in.Slug, exists); aerr != nil {
				return aerr
			}
		}
		return s.publishIntoLockedResult(ctx, in, stamped, &result)
	})
	if err != nil {
		return nil, err
	}
	s.applyIdentity(result, in.identity)
	s.afterPublished(ctx, result, registration != nil)
	return result, nil
}

// publishIntoLockedResult calls the non-locking helper for callers that already
// own the document lock, avoiding lock re-entry.
func (s *DocService) publishIntoLockedResult(ctx context.Context, in PublishInput, stamped core.StampResult, result **PublishResult) error {
	r, err := s.publishLocked(ctx, in, stamped)
	*result = r
	return err
}

func (s *DocService) withCanonicalGuard(ctx context.Context, slug string, canonical bool, fn func() error) error {
	if !canonical {
		return s.lock.With(ctx, slug, fn)
	}
	provider, ok := s.meta.(lockerProvider)
	if !ok || provider.Locker() == nil {
		return apperr.Upstream("shared canonical initialization guard unavailable", "canonical_guard_unavailable", nil)
	}
	return provider.Locker().With(ctx, slug, fn)
}

func (s *DocService) applyIdentity(result *PublishResult, identity *canonicalIdentity) {
	if result == nil || identity == nil {
		return
	}
	result.Slug = identity.docID

	result.DocID = identity.docID
	result.URL = identity.shareURL
	result.ShareURL = identity.shareURL
	result.Registered = true
}

func (s *DocService) existingPublishResult(ctx context.Context, in PublishInput, shareURL string, authorize func(string, bool) error) (*PublishResult, error) {
	versions, err := s.blobs.ListVersions(ctx, in.Slug)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, nil
	}
	latest := versions[0]
	for _, version := range versions[1:] {
		if version > latest {
			latest = version
		}
	}
	meta, err := s.meta.GetMeta(ctx, in.Slug)
	if err != nil {
		return nil, err
	}
	if authorize != nil {
		trustedRecovery := meta == nil && in.PublisherUID != "" && in.PublisherSpaceID != "" && in.identity != nil
		if !trustedRecovery {
			if err := authorize(in.Slug, true); err != nil {
				return nil, err
			}
		}
	}
	// Registration and PutDoc(v1) may be durable before PutMeta fails. Repair
	// that exact half-state under the shared canonical guard without rewriting the
	// immutable blob or minting a replay version.
	_, _, canonical := meta.CanonicalIdentity()
	hasVersion := false
	if meta != nil {
		for _, ref := range meta.Versions {
			if ref.N == latest {
				hasVersion = true
				break
			}
		}
	}
	if meta == nil || !canonical || !hasVersion {
		if _, err = s.upsertMeta(ctx, in, latest); err != nil {
			return nil, err
		}
		meta, err = s.meta.GetMeta(ctx, in.Slug)
		if err != nil {
			return nil, err
		}
		if meta == nil {
			return nil, apperr.Upstream("canonical metadata recovery failed", "metadata_recovery_failed", nil)
		}
	}
	if !publishCommentsMerged(meta, latest) {
		html, ok, getErr := s.blobs.GetDoc(ctx, in.Slug, latest)
		if getErr != nil {
			return nil, getErr
		}
		if !ok {
			return nil, apperr.Upstream("canonical publish blob recovery failed", "blob_recovery_failed", nil)
		}
		persisted := core.StampAids(html)
		if _, mergeErr := s.comments.PublishMergeWithMigrationsLocked(ctx, in.Slug, in.LocalComments, persisted.AIDs, latest, "", "", nil); mergeErr != nil {
			return nil, mergeErr
		}
		if err = s.markPublishCommentsMerged(ctx, in.Slug, latest); err != nil {
			return nil, err
		}
		meta, err = s.meta.GetMeta(ctx, in.Slug)
		if err != nil {
			return nil, err
		}
	}
	if _, persistedURL, ok := meta.CanonicalIdentity(); ok && persistedURL != "" {
		shareURL = persistedURL
	}
	return &PublishResult{
		Slug: in.Slug, Version: latest, DocID: in.Slug, URL: shareURL, ShareURL: shareURL,
		Registered: true, Status: publishStatusPublished, title: meta.Title, publisherToken: in.PublisherToken,
	}, nil
}

func (s *DocService) slugExists(ctx context.Context, slug string) (bool, error) {
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return false, err
	}
	if meta != nil {
		return true, nil
	}
	versions, err := s.blobs.ListVersions(ctx, slug)
	if err != nil {
		return false, err
	}
	if len(versions) > 0 {
		return true, nil
	}
	_, hasDraft, err := s.blobs.GetDraft(ctx, slug)
	return hasDraft, err
}

// publishLocked runs the publish critical section. The caller MUST hold the
// per-slug lock (Publish does); it therefore uses PublishMergeLocked and never
// re-acquires the lock.
func (s *DocService) publishLocked(ctx context.Context, in PublishInput, stamped core.StampResult) (*PublishResult, error) {
	if err := s.restoreMountContext(ctx, &in); err != nil {
		return nil, err
	}
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

	merge, err := s.comments.PublishMergeWithMigrationsLocked(ctx, in.Slug, in.LocalComments, stamped.AIDs, version, in.pinnedAID, in.pinnedTag, in.anchorMigrations)
	if err != nil {
		return nil, err
	}
	if in.identity != nil {
		if err := s.markPublishCommentsMerged(ctx, in.Slug, version); err != nil {
			return nil, err
		}
	}
	merged := 0
	if body, ok := merge.Body.(map[string]any); ok {
		if m, ok := body["mergedComments"].(int); ok {
			merged = m
		}
	}

	result := &PublishResult{
		Slug:              in.Slug,
		Version:           version,
		Status:            publishStatusPublished,
		Size:              size,
		AIDs:              len(stamped.AIDs),
		MergedComments:    merged,
		title:             metaResult.title,
		hadMeta:           metaResult.hadMeta,
		titleChanged:      metaResult.titleChanged,
		mountType:         in.MountType,
		mountContextKnown: in.mountContextKnown,
		publisherToken:    in.PublisherToken,
	}
	s.applyIdentity(result, in.identity)
	return result, nil
}

func (s *DocService) restoreMountContext(ctx context.Context, in *PublishInput) error {
	if in.mountContextKnown && in.MountType != "" {
		return nil
	}
	meta, err := s.meta.GetMeta(ctx, in.Slug)
	if err != nil {
		return err
	}
	if mountType, ok := meta.MountType(); ok {
		normalized, normalizeErr := normalizeMountType(mountType)
		if normalizeErr != nil {
			return normalizeErr
		}
		if normalized != "" || !in.mountContextKnown {
			in.MountType = normalized
			in.mountContextKnown = true
		}
		return nil
	}
	if meta == nil {
		in.mountContextKnown = true
	}
	return nil
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
// After validation the backend injects the target's existing aid onto the
// replacement root for this publish only. Reconciliation validates that the aid
// is unique and refreshes the anchor fingerprint atomically when the tag changes;
// later plain publishes compute normal content-derived identity again.
func (s *DocService) ReplaceElement(ctx context.Context, slug string, baseVersion int, aid, newHTML string) (*PublishResult, error) {
	return s.ReplaceElementAuthorized(ctx, slug, baseVersion, aid, newHTML, "")
}

// ReplaceElementAuthorized replaces an element and carries the request bot token
// through persistence to the bounded best-effort published notification.
func (s *DocService) ReplaceElementAuthorized(ctx context.Context, slug string, baseVersion int, aid, newHTML, publisherToken string) (*PublishResult, error) {
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
	// Root must remain addressable in the replacement version. The pin is only an
	// immediate publish exception; it is not a durable identity promise.
	if !core.IsHarvestableReplacementRoot(newHTML) {
		return nil, apperr.Validation(
			"new_html root must be a stampable element or carry class \"odoc-artifact\"",
			"new_html_root_not_addressable")
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
		injected, localRoot := core.InjectRootAIDAt(newHTML, aid)
		replaced, boundary, ok := core.ReplaceElementByAIDAt(rd.HTML, aid, injected)
		if !ok {
			return apperr.NotFound("aid not found in this version")
		}
		if int64(len(replaced)) > s.maxBytes {
			return apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
		}
		// Emit the replacement root's canonical content identity immediately. Pinning
		// that canonical AID at the exact replacement offset salts any other collision
		// away while every unrelated artifact is stamped normally.
		rootCanonical := core.StampAids(newHTML)
		if len(rootCanonical.AIDs) == 0 {
			return apperr.Validation("new_html root has no canonical aid", "new_html_root_not_addressable")
		}
		canonicalAID := rootCanonical.AIDs[0].AID
		canonical := core.StampAids(replaced)
		matches := 0
		for _, artifact := range canonical.AIDs {
			if artifact.AID == canonicalAID {
				matches++
			}
		}
		if matches != 1 {
			return apperr.Validation("replacement canonical aid is ambiguous", "new_html_canonical_aid_ambiguous")
		}
		stamped := core.StampAidsPinned(replaced, canonicalAID, boundary+localRoot)
		in, ierr := s.existingPublishInput(ctx, slug, replaced, "")
		if ierr != nil {
			return ierr
		}
		in.PublisherToken = publisherToken
		in.pinnedAID = aid
		in.pinnedTag, _ = core.SingleTopLevelTag(newHTML)
		in.anchorMigrations = map[string]string{aid: canonicalAID}
		r, perr := s.publishLocked(ctx, in, stamped)
		if perr != nil {
			return perr
		}
		result = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.afterPublished(ctx, result, false)
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
	var title string
	if meta != nil {
		versions = meta.Versions
		title = meta.Title
	}
	return &RenderData{HTML: html, Versions: versions, Title: title}, nil
}

// VersionList is the response of ListVersions.
type VersionList struct {
	Slug     string               `json:"slug"`
	Title    string               `json:"title"`
	Versions []storage.VersionRef `json:"versions"`
}

// DraftResult is the result of saving a draft.
type DraftResult struct {
	Slug  string `json:"slug"`
	DocID string `json:"doc_id,omitempty"`
	URL   string `json:"url"`
	Size  int64  `json:"size"`
	AIDs  int    `json:"aids"`
}

// SaveDraft stamps and writes the mutable draft slot for a slug, creating the
// meta record if the slug is new (draft-only docs have an empty Versions list).
// The draft never enters the immutable version numbering until Promote.
//
// creatorUID is stamped into meta on first create only (draft-first ownership),
// exactly like Publish; a later save by a different caller never reassigns it,
// and the stamped creator carries through to the promoted version.
func (s *DocService) SaveDraft(ctx context.Context, slug, html, title, creatorUID string) (*DraftResult, error) {
	return s.SaveDraftMounted(ctx, PublishInput{Slug: slug, HTML: html, Title: title, CreatorUID: creatorUID})
}

// SaveDraftMounted saves a draft and pre-registers mounted draft-first docs.
func (s *DocService) SaveDraftMounted(ctx context.Context, in PublishInput) (*DraftResult, error) {
	return s.SaveDraftMountedAuthorized(ctx, in, nil)
}

// SaveDraftMountedAuthorized checks existing identities under their lock.
func (s *DocService) SaveDraftMountedAuthorized(ctx context.Context, in PublishInput, authorize func(string, bool) error) (*DraftResult, error) {
	slug, html, title, creatorUID := in.Slug, in.HTML, in.Title, in.CreatorUID
	if html == "" {
		return nil, apperr.Validation("html required", "html_required")
	}
	if int64(len(html)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
	}
	if in.MountType != "" {
		in.MountTypePresent = true
	}
	mountType, err := normalizeMountType(in.MountType)
	if err != nil {
		return nil, err
	}
	in.MountType = mountType
	explicitCreate := strings.TrimSpace(in.IdempotencyKey) != ""
	if explicitCreate && slug != "" {
		return nil, apperr.Validation("canonical create must not include slug", "create_ref_forbidden")
	}
	if slug == "" && !explicitCreate {
		return nil, apperr.Validation("idempotency_key required for canonical create", "idempotency_key_required")
	}
	if explicitCreate && strings.TrimSpace(in.PublisherToken) == "" {
		return nil, apperr.Unauthorized("canonical create requires bot authentication", "publisher_bot_required")
	}
	exists := false
	if !explicitCreate {
		exists, err = s.slugExists(ctx, slug)
		if err != nil {
			return nil, err
		}
		if exists {
			meta, metaErr := s.meta.GetMeta(ctx, slug)
			if metaErr != nil {
				return nil, metaErr
			}
			if persistedMount, ok := meta.MountType(); ok && in.MountType == "" {
				in.MountType, err = normalizeMountType(persistedMount)
				if err != nil {
					return nil, err
				}
			}
		} else if authorize != nil {
			return nil, apperr.NotFound("document not found")
		}
	}
	var registration *docsbackend.RegistrationResult
	if explicitCreate {
		registration, err = s.preRegister(ctx, in)
		if err != nil {
			return nil, err
		}
		if config.SafeSlug(registration.DocID) == "" || registration.OctoDocSlug != registration.DocID {
			return nil, apperr.Upstream("docs-backend returned invalid document identity", "registration_invalid", nil)
		}
		if (in.PublisherUID != "" || in.PublisherSpaceID != "") && (in.PublisherUID == "" || in.PublisherSpaceID == "" || registration.PublisherUID != in.PublisherUID || registration.SpaceID != in.PublisherSpaceID) {
			return nil, apperr.Forbidden("registration publisher identity mismatch", "registration_identity_mismatch")
		}
		slug = registration.DocID
		in.Slug = slug
		in.identity = &canonicalIdentity{docID: registration.DocID, shareURL: registration.ShareURL}
	}
	stamped := core.StampAids(html)
	var result *DraftResult
	err = s.withCanonicalGuard(ctx, slug, explicitCreate, func() error {
		if registration != nil {
			existing, xerr := s.existingDraftResult(ctx, in, authorize)
			if xerr != nil {
				return xerr
			}
			if existing != nil {
				result = existing
				return nil
			}
		}
		if authorize != nil {
			current, xerr := s.slugExists(ctx, slug)
			if xerr != nil {
				return xerr
			}
			if aerr := authorize(slug, current); aerr != nil {
				return aerr
			}
		}
		return s.saveDraftLocked(ctx, slug, title, creatorUID, stamped, in.identity, &result)
	})
	if err != nil {
		return nil, err
	}
	if in.identity != nil {
		result.DocID = in.identity.docID
	}
	return result, nil
}

func (s *DocService) existingDraftResult(ctx context.Context, in PublishInput, authorize func(string, bool) error) (*DraftResult, error) {
	meta, err := s.meta.GetMeta(ctx, in.Slug)
	if err != nil {
		return nil, err
	}
	if completed, ok := canonicalDraftCreateResult(meta, in.Slug, s.baseURL); ok {
		return completed, nil
	}
	html, ok, err := s.blobs.GetDraft(ctx, in.Slug)
	if err != nil || !ok {
		return nil, err
	}
	if authorize != nil {
		trustedRecovery := meta == nil && in.PublisherUID != "" && in.PublisherSpaceID != "" && in.identity != nil
		if !trustedRecovery {
			if err := authorize(in.Slug, true); err != nil {
				return nil, err
			}
		}
	}
	hasDraftMeta := false
	if meta != nil {
		_, hasDraftMeta = meta.Extra[storage.DraftExtraKey]
	}
	docID, shareURL, canonical := meta.CanonicalIdentity()
	creatorComplete := in.CreatorUID == "" || (meta != nil && meta.CreatorUID() != "")
	identityComplete := canonical && docID == in.Slug && in.identity != nil && shareURL == in.identity.shareURL
	if !hasDraftMeta || !identityComplete || !creatorComplete {
		stamped := core.StampAids(html)
		if err := s.setDraftMeta(ctx, in.Slug, in.Title, in.CreatorUID, in.identity, int64(len(html)), len(stamped.AIDs)); err != nil {
			return nil, err
		}
		meta, err = s.meta.GetMeta(ctx, in.Slug)
		if err != nil {
			return nil, err
		}
		if meta == nil {
			return nil, apperr.Upstream("canonical draft metadata recovery failed", "metadata_recovery_failed", nil)
		}
		docID, shareURL, canonical = meta.CanonicalIdentity()
		_, hasDraftMeta = meta.Extra[storage.DraftExtraKey]
		creatorComplete = in.CreatorUID == "" || meta.CreatorUID() != ""
		if !hasDraftMeta || !canonical || docID != in.Slug || in.identity == nil || shareURL != in.identity.shareURL || !creatorComplete {
			return nil, apperr.Upstream("canonical draft metadata recovery failed", "metadata_recovery_failed", nil)
		}
	}
	return &DraftResult{Slug: in.Slug, DocID: in.Slug, URL: fmt.Sprintf("%s/d/%s/draft", s.baseURL, in.Slug), Size: int64(len(html)), AIDs: len(core.StampAids(html).AIDs)}, nil
}

// saveDraftLocked initializes the draft without acquiring the document lock.
func (s *DocService) saveDraftLocked(ctx context.Context, slug, title, creatorUID string, stamped core.StampResult, identity *canonicalIdentity, result **DraftResult) error {
	size, err := s.blobs.PutDraft(ctx, slug, stamped.HTML)
	if err != nil {
		return apperr.Upstream("draft write failed", "draft_write_failed", err)
	}
	if err := s.setDraftMeta(ctx, slug, title, creatorUID, identity, size, len(stamped.AIDs)); err != nil {
		return err
	}
	*result = &DraftResult{Slug: slug, URL: fmt.Sprintf("%s/d/%s/draft", s.baseURL, slug), Size: size, AIDs: len(stamped.AIDs)}
	return nil
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
	var title string
	if meta != nil {
		versions = meta.Versions
		title = meta.Title
	}
	return &RenderData{HTML: html, Versions: versions, Title: title}, nil
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
	return s.PromoteAuthorized(ctx, slug, title, "")
}

// PromoteAuthorized promotes a draft and forwards the request bot credential to
// the post-commit backend notification.
func (s *DocService) PromoteAuthorized(ctx context.Context, slug, title, publisherToken string) (*PublishResult, error) {
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
		in, ierr := s.existingPublishInput(ctx, slug, html, title)
		if ierr != nil {
			return ierr
		}
		in.PublisherToken = publisherToken
		r, perr := s.publishLocked(ctx, in, stamped)
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
	s.afterPublished(ctx, result, false)
	return result, nil
}

// setDraftMeta records a draft marker in the meta Extra catch-all, creating the
// meta record if the slug is new. It leaves Versions untouched. creatorUID is
// stamped on first create only (same rule as upsertMeta), never reassigning an
// existing creator.
func (s *DocService) setDraftMeta(ctx context.Context, slug, title, creatorUID string, identity *canonicalIdentity, size int64, aids int) error {
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
	if identity != nil {
		extra[storage.CanonicalDocIDExtraKey] = identity.docID
		extra[storage.CanonicalShareURLExtraKey] = identity.shareURL
		extra[storage.CanonicalDraftCreateExtraKey] = map[string]any{"size": size, "aids": aids}
	}
	return s.meta.PutMeta(ctx, slug, storage.DocMeta{
		Slug:     slug,
		Title:    metaTitle,
		Versions: prev.Versions,
		Extra:    extra,
	})
}

func canonicalDraftCreateResult(meta *storage.DocMeta, slug, baseURL string) (*DraftResult, bool) {
	if meta == nil || meta.Extra == nil {
		return nil, false
	}
	marker, ok := meta.Extra[storage.CanonicalDraftCreateExtraKey].(map[string]any)
	if !ok {
		return nil, false
	}
	size, _ := marker["size"].(float64)
	if n, ok := marker["size"].(int64); ok {
		size = float64(n)
	}
	aids, _ := marker["aids"].(float64)
	if n, ok := marker["aids"].(int); ok {
		aids = float64(n)
	}
	return &DraftResult{Slug: slug, DocID: slug, URL: fmt.Sprintf("%s/d/%s/draft", baseURL, slug), Size: int64(size), AIDs: int(aids)}, true
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

func (s *DocService) existingPublishInput(ctx context.Context, slug, html, title string) (PublishInput, error) {
	in := PublishInput{Slug: slug, HTML: html, Title: title}
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return PublishInput{}, err
	}
	if mountType, ok := meta.MountType(); ok {
		normalized, normalizeErr := normalizeMountType(mountType)
		if normalizeErr != nil {
			return PublishInput{}, normalizeErr
		}
		in.MountType = normalized
		in.mountContextKnown = true
	}
	if docID, shareURL, ok := meta.CanonicalIdentity(); ok {
		in.identity = &canonicalIdentity{docID: docID, shareURL: shareURL}
	}

	return in, nil
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
	return s.RemoveAuthorized(ctx, slug, DeleteAuth{})
}

// RemoveAuthorized deletes an existing ref via explicit bot or human auth.
func (s *DocService) RemoveAuthorized(ctx context.Context, slug string, auth DeleteAuth) error {
	return s.lock.With(ctx, slug, func() error {
		exists, err := s.slugExists(ctx, slug)
		if err != nil {
			return err
		}
		if !exists {
			return apperr.NotFound("document not found")
		}
		meta, err := s.meta.GetMeta(ctx, slug)
		if err != nil {
			return err
		}
		docID, _, canonical := meta.CanonicalIdentity()
		if canonical && docID != slug {
			return apperr.Conflict("canonical document identity mismatch", "canonical_identity_mismatch")
		}
		hasBotToken := strings.TrimSpace(auth.PublisherToken) != ""
		if !hasBotToken && (canonical || s.register != nil || strings.TrimSpace(auth.ActorUID) == "") {
			return apperr.Unauthorized("safe remote human delete unavailable", "delete_delegation_required")
		}
		if canonical && s.register == nil {
			return apperr.Upstream("docs-backend deletion unavailable", "delete_failed", nil)
		}
		if s.register != nil {
			deleteCtx, cancel := context.WithTimeout(ctx, docsBackendSideEffectTimeout)
			defer cancel()
			derr := s.register.Delete(deleteCtx, slug, auth.PublisherToken)
			if derr != nil {
				return apperr.Upstream("docs-backend deletion failed", "delete_failed", derr)
			}
		}
		if err := s.blobs.DeleteDoc(ctx, slug); err != nil {
			return err
		}
		assets, err := s.meta.ListAssetMeta(ctx, slug)
		if err != nil {
			return err
		}
		for _, a := range assets {
			if derr := s.meta.DeleteAssetMeta(ctx, slug, a.SHA256); derr != nil {
				return derr
			}
		}
		// Comments are removed before metadata so a failed wipe remains reachable
		// through the document and a retry can finish cleanup.
		if _, err = s.comments.WipeLocked(ctx, slug); err != nil {
			return err
		}
		return s.meta.DeleteMeta(ctx, slug)
	})
}

func (s *DocService) afterPublished(parent context.Context, result *PublishResult, registered bool) {
	if result == nil {
		return
	}
	if result.Registered {
		s.notifyPublished(parent, result)
	}
	if registered || result.Registered {
		// Canonical edits never register again. A title reconcile is optional and
		// must use this request's bot identity, not the process token.
		if result.hadMeta && result.titleChanged && s.register != nil && result.publisherToken != "" {
			ctx, cancel := context.WithTimeout(parent, docsBackendSideEffectTimeout)
			s.register.Rename(ctx, result.Slug, result.title, result.publisherToken)
			cancel()
		}
		if s.reconcileFn != nil {
			if err := s.reconcileFn(parent, result.Slug); err != nil {
				s.log().Error("grant_reconcile_failed", "slug", result.Slug, "err", err.Error())
			}
		}
		return
	}
	// Existing references are never registration creates. Legacy registered rows
	// may only be reconciled through non-create endpoints.
	if result.mountContextKnown && result.mountType == "" {
		result.Status = publishStatusUnregistered
	}
	s.afterLegacyPublished(parent, result)
}

func (s *DocService) notifyPublished(parent context.Context, result *PublishResult) {
	notifier, ok := s.register.(publishNotifier)
	if !ok || strings.TrimSpace(result.publisherToken) == "" {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	var err error
	for attempt := 1; attempt <= docsBackendRegisterAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(parent, docsBackendAttemptTimeout)
		err = notifier.Published(attemptCtx, result.DocID, result.title, result.publisherToken)
		cancel()
		if err == nil {
			return
		}
		if attempt < docsBackendRegisterAttempts && !waitForRetry(parent, docsBackendRegisterDelay) {
			break
		}
	}
	s.log().Error("publish_notification_failed", "doc_id", result.DocID, "attempts", docsBackendRegisterAttempts, "err", err.Error())
}

func (s *DocService) afterLegacyPublished(parent context.Context, result *PublishResult) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, docsBackendSideEffectTimeout)
	defer cancel()
	if result.titleChanged && s.register != nil {
		s.register.Rename(ctx, result.Slug, result.title, result.publisherToken)
	}
	if ctx.Err() == nil && s.reconcileFn != nil {
		if err := s.reconcileFn(ctx, result.Slug); err != nil {
			s.log().Error("grant_reconcile_failed", "slug", result.Slug, "err", err.Error())
		}
	}
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *DocService) preRegister(ctx context.Context, in PublishInput) (*docsbackend.RegistrationResult, error) {
	reg := docsbackend.Registration{DocType: "html", IdempotencyKey: in.IdempotencyKey, MountType: in.MountType, Title: strings.TrimSpace(in.Title)}
	if s.register == nil {
		return nil, apperr.Upstream("docs-backend registration unavailable", "registration_unavailable", nil)
	}
	var result *docsbackend.RegistrationResult
	var err error
	for attempt := 1; attempt <= docsBackendRegisterAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, docsBackendAttemptTimeout)
		result, err = s.register.Register(attemptCtx, reg, in.PublisherToken)
		cancel()
		if err == nil && result != nil {
			return result, nil
		}
		if err == nil {
			err = fmt.Errorf("docs-backend registration returned no result")
		}
		if docsbackend.IsCanonicalDocumentDeleted(err) {
			return nil, apperr.Conflict("canonical document was deleted", "canonical_document_deleted")
		}
		if attempt < docsBackendRegisterAttempts && !waitForRetry(ctx, docsBackendRegisterDelay) {
			break
		}
	}
	return nil, apperr.Upstream("docs-backend registration failed", "registration_failed", err)
}

func normalizeMountType(mountType string) (string, error) {
	mountType = strings.ToLower(strings.TrimSpace(mountType))
	switch mountType {
	case "", "group", "space", "thread":
		return mountType, nil
	default:
		return "", apperr.Validation("mount_type must be one of empty, group, space, or thread", "mount_type_invalid")
	}
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
	if in.mountContextKnown || in.identity != nil || (in.CreatorUID != "" && prev.CreatorUID() == "") {
		extra = map[string]any{}
		maps.Copy(extra, prev.Extra)
	}
	if in.mountContextKnown {
		extra[storage.MountTypeExtraKey] = in.MountType
	}
	if in.CreatorUID != "" && prev.CreatorUID() == "" {
		extra[storage.CreatorUIDExtraKey] = in.CreatorUID
	}
	if in.identity != nil {
		extra[storage.CanonicalDocIDExtraKey] = in.identity.docID
		extra[storage.CanonicalShareURLExtraKey] = in.identity.shareURL
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
		titleChanged: hadMeta && strings.TrimSpace(prev.Title) != "" && prev.Title != title,
	}, nil
}

func publishCommentsMerged(meta *storage.DocMeta, version int) bool {
	if meta == nil || meta.Extra == nil {
		return false
	}
	value, ok := meta.Extra[storage.PublishCommentsMergedVersionExtraKey].(float64)
	return ok && int(value) == version
}

func (s *DocService) markPublishCommentsMerged(ctx context.Context, slug string, version int) error {
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return err
	}
	if meta == nil || publishCommentsMerged(meta, version) {
		return nil
	}
	extra := map[string]any{}
	maps.Copy(extra, meta.Extra)
	extra[storage.PublishCommentsMergedVersionExtraKey] = version
	meta.Extra = extra
	return s.meta.PutMeta(ctx, slug, *meta)
}

func sortVersions(v []storage.VersionRef) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1].N > v[j].N; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}

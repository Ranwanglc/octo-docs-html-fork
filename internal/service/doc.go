package service

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"regexp"
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
	Delete(ctx context.Context, slug, token string)
	DeleteCanonical(ctx context.Context, slug, token string) error
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
	// Empty ⇒ the registrar falls back to its process-configured token.
	PublisherToken string
	// IdempotencyKey selects canonical creation. It is intentionally separate
	// from legacy slug publishing so the old path remains unchanged.
	IdempotencyKey      string
	PublisherUID        string
	PublisherSpaceID    string
	SpaceID             string
	UserPublish         bool
	AuthorizeProvenance ProvenanceAuthorizer

	mountContextKnown bool
	pinnedAID         string
	pinnedTag         string
	anchorMigrations  map[string]string
}

// PublishProvenance is the durable ownership context effective for a write.
type PublishProvenance struct {
	UserPublish bool
	SpaceID     string
	CreatorUID  string
}

// ProvenanceAuthorizer validates effective persisted provenance before commit.
type ProvenanceAuthorizer func(context.Context, PublishProvenance) error

// PublishResult is the result of a successful publish.
type PublishResult struct {
	Slug           string `json:"slug"`
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
	spaceID        string
	owner          string
	userPublish    bool
}

type canonicalIdentity struct{ docID, shareURL string }

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

var spaceIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidSpaceID reports whether a space id is valid for user publishing.
func ValidSpaceID(spaceID string) bool { return spaceIDRe.MatchString(strings.TrimSpace(spaceID)) }

// Publish publishes a new (or explicitly-versioned) document.
func (s *DocService) Publish(ctx context.Context, in PublishInput) (*PublishResult, error) {
	return s.PublishAuthorized(ctx, in, nil)
}

// PublishAuthorized publishes after authorize checks the slug's current
// existence while the per-slug lock is held.
func (s *DocService) PublishAuthorized(ctx context.Context, in PublishInput, authorize func(exists bool) error) (*PublishResult, error) {
	if strings.TrimSpace(in.IdempotencyKey) != "" || strings.TrimSpace(in.Slug) == "" {
		return s.publishCanonicalAuthorized(ctx, in, authorize)
	}
	if in.HTML == "" {
		return nil, apperr.Validation("html (file) required", "html_required")
	}
	if int64(len(in.HTML)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
	}
	if in.UserPublish {
		in.SpaceID = strings.TrimSpace(in.SpaceID)
		in.CreatorUID = strings.TrimSpace(in.CreatorUID)
		if in.CreatorUID == "" {
			return nil, apperr.Conflict("user publish provenance requires a creator", "publish_provenance_conflict")
		}
		if !spaceIDRe.MatchString(in.SpaceID) {
			return nil, apperr.Validation("valid space_id required", "space_id_invalid")
		}
		in.MountType = "space"
		in.MountTypePresent = true
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

	stamped := core.StampAids(in.HTML)

	// Hold the per-slug lock across the whole critical section: version resolution,
	// the immutable blob write, the version-list bump, and the comment merge must be
	// atomic, or two concurrent publishes of the same slug can resolve to the same
	// version and clobber each other (and drift meta vs blobs).
	var result *PublishResult
	canonicalUpdate := false
	err = s.lock.With(ctx, in.Slug, func() error {
		// A canonical marker, not a d_ prefix, selects the canonical update path.
		// Preserve the legacy path unchanged for old slugs that merely look alike.
		meta, metaErr := s.meta.GetMeta(ctx, in.Slug)
		if metaErr != nil {
			return metaErr
		}
		if docID, shareURL, ok := meta.CanonicalIdentity(); ok {
			if docID != in.Slug {
				return apperr.Conflict("canonical document identity mismatch", "canonical_identity_mismatch")
			}
			canonicalUpdate = true
			defer func() {
				if result != nil {
					result.DocID, result.URL, result.ShareURL, result.Registered = docID, shareURL, shareURL, true
				}
			}()
		}
		if in.UserPublish {
			meta, metaErr := s.meta.GetMeta(ctx, in.Slug)
			if metaErr != nil {
				return metaErr
			}
			if meta == nil {
				exists, existsErr := s.slugExists(ctx, in.Slug)
				if existsErr != nil {
					return existsErr
				}
				if exists {
					return apperr.Conflict("document publishing identity is immutable", "publish_provenance_conflict")
				}
			}
		}
		if authorize != nil {
			exists, xerr := s.slugExists(ctx, in.Slug)
			if xerr != nil {
				return xerr
			}
			if aerr := authorize(exists); aerr != nil {
				return aerr
			}
		}
		r, perr := s.publishLocked(ctx, in, stamped)
		result = r
		return perr
	})
	if err != nil {
		return nil, err
	}
	if canonicalUpdate {
		return result, nil
	}
	s.afterPublished(ctx, result)
	return result, nil
}

// publishCanonicalAuthorized is an additive identity-first create path. Backend
// registration allocates the durable d_* key before the one local critical
// section begins; legacy publishes above retain their existing lock and flow.
func (s *DocService) publishCanonicalAuthorized(ctx context.Context, in PublishInput, authorize func(bool) error) (*PublishResult, error) {
	if in.HTML == "" {
		return nil, apperr.Validation("html (file) required", "html_required")
	}
	if int64(len(in.HTML)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, apperr.Validation("idempotency_key required for canonical create", "idempotency_key_required")
	}
	if strings.TrimSpace(in.Slug) != "" {
		return nil, apperr.Validation("canonical create must not include slug", "create_ref_forbidden")
	}
	if strings.TrimSpace(in.PublisherToken) == "" {
		return nil, apperr.Unauthorized("canonical create requires bot authentication", "publisher_bot_required")
	}
	if s.register == nil {
		return nil, apperr.Upstream("docs-backend registrar is disabled", "registration_failed", nil)
	}
	mountType, err := normalizeMountType(in.MountType)
	if err != nil {
		return nil, err
	}
	in.MountType = mountType
	in.mountContextKnown = in.MountTypePresent || mountType != ""
	reg, err := s.register.Register(ctx, docsbackend.Registration{DocType: "html", IdempotencyKey: in.IdempotencyKey, MountType: in.MountType, Title: in.Title}, in.PublisherToken)
	if err != nil {
		return nil, apperr.Upstream("docs-backend registration failed", "registration_failed", err)
	}
	registered := true
	defer func() {
		if registered {
			_ = s.register.DeleteCanonical(ctx, reg.DocID, in.PublisherToken)
		}
	}()
	if config.SafeSlug(reg.DocID) == "" || reg.OctoDocSlug != reg.DocID {
		return nil, apperr.Upstream("docs-backend returned invalid document identity", "registration_invalid", nil)
	}
	if (in.PublisherUID != "" || in.PublisherSpaceID != "") && (in.PublisherUID == "" || in.PublisherSpaceID == "" || reg.PublisherUID != in.PublisherUID || reg.SpaceID != in.PublisherSpaceID) {
		return nil, apperr.Forbidden("registration publisher identity mismatch", "registration_identity_mismatch")
	}
	in.Slug = reg.DocID
	identity := &canonicalIdentity{docID: reg.DocID, shareURL: reg.ShareURL}
	if s.lock == nil {
		return nil, apperr.Upstream("shared canonical initialization guard unavailable", "canonical_guard_unavailable", nil)
	}
	stamped := core.StampAids(in.HTML)
	var result *PublishResult
	err = s.lock.With(ctx, in.Slug, func() error {
		// An idempotency replay must return the original write, never mint v2.
		if versions, e := s.blobs.ListVersions(ctx, in.Slug); e != nil {
			return e
		} else if len(versions) > 0 {
			latest := versions[0]
			for _, v := range versions[1:] {
				if v > latest {
					latest = v
				}
			}
			meta, e := s.meta.GetMeta(ctx, in.Slug)
			if e != nil {
				return e
			}
			if meta != nil {
				if docID, shareURL, ok := meta.CanonicalIdentity(); !ok || docID != reg.DocID {
					return apperr.Conflict("canonical identity conflicts with existing document", "canonical_identity_conflict")
				} else {
					result = &PublishResult{Slug: in.Slug, Version: latest, URL: shareURL, DocID: docID, ShareURL: shareURL, Registered: true, Status: publishStatusPublished, title: meta.Title}
				}
				return nil
			}
		}
		if authorize != nil {
			if e := authorize(false); e != nil {
				return e
			}
		}
		r, e := s.publishLocked(ctx, in, stamped)
		if e == nil {
			// Store the marker and URL atomically with the canonical guard so later
			// d_-prefixed republishes can dispatch by identity type.
			meta, metaErr := s.meta.GetMeta(ctx, in.Slug)
			if metaErr != nil {
				return metaErr
			}
			if meta == nil {
				return apperr.Upstream("canonical metadata missing", "metadata_recovery_failed", nil)
			}
			extra := map[string]any{}
			maps.Copy(extra, meta.Extra)
			extra[storage.CanonicalDocIDExtraKey] = identity.docID
			extra[storage.CanonicalShareURLExtraKey] = identity.shareURL
			e = s.meta.PutMeta(ctx, in.Slug, storage.DocMeta{Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra})
		}
		result = r
		return e
	})
	if err != nil {
		return nil, err
	}
	registered = false
	result.DocID, result.ShareURL, result.URL, result.Registered = identity.docID, identity.shareURL, identity.shareURL, true
	return result, nil
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
	if in.AuthorizeProvenance != nil {
		if err := in.AuthorizeProvenance(ctx, PublishProvenance{UserPublish: in.UserPublish, SpaceID: in.SpaceID, CreatorUID: in.CreatorUID}); err != nil {
			return nil, err
		}
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
	merged := 0
	if body, ok := merge.Body.(map[string]any); ok {
		if m, ok := body["mergedComments"].(int); ok {
			merged = m
		}
	}

	return &PublishResult{
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
		spaceID:           in.SpaceID,
		owner:             in.CreatorUID,
		userPublish:       in.UserPublish,
	}, nil
}

func (s *DocService) restoreMountContext(ctx context.Context, in *PublishInput) error {
	meta, err := s.meta.GetMeta(ctx, in.Slug)
	if err != nil {
		return err
	}
	if meta == nil {
		exists, existsErr := s.slugExists(ctx, in.Slug)
		if existsErr != nil {
			return existsErr
		}
		if exists && in.UserPublish {
			return apperr.Conflict("document publishing identity is immutable", "publish_provenance_conflict")
		}
		in.mountContextKnown = true
		return nil
	}
	existingUser, spaceID, groupNo, threadID := meta.PublishProvenance()
	if in.UserPublish && !existingUser {
		return apperr.Conflict("document publishing identity is immutable", "publish_provenance_conflict")
	}
	if !existingUser && spaceID == "" && (in.MountType != "" || in.SpaceID != "") {
		// Legacy metadata without mount provenance may be completed once. A
		// persisted non-empty mount/space is handled by the immutable checks below.
		existingUser = in.UserPublish
		spaceID = in.SpaceID
	}
	existingMount, hasMount := meta.MountType()
	if !existingUser {
		if restoreErr := restoreLegacyMount(in, existingMount, hasMount, groupNo, threadID); restoreErr != nil {
			return restoreErr
		}
		if creator := meta.CreatorUID(); creator != "" {
			in.CreatorUID = creator
		}
		return nil
	}
	creator := strings.TrimSpace(meta.CreatorUID())
	if creator == "" {
		return apperr.Conflict("user publish provenance is incomplete", "publish_provenance_conflict")
	}
	if !ValidSpaceID(spaceID) {
		return apperr.Conflict("user publish provenance is incomplete", "publish_provenance_conflict")
	}
	if !hasMount {
		existingMount = inferredLegacyMount(existingUser, spaceID, groupNo, threadID)
	}
	if in.SpaceID != "" && spaceID != "" && in.SpaceID != spaceID {
		return apperr.Conflict("document space is immutable", "space_conflict")
	}
	if hasMount || existingMount != "" {
		normalized, normalizeErr := normalizeMountType(existingMount)
		if normalizeErr != nil {
			return normalizeErr
		}
		if normalized != "" && in.mountContextKnown && in.MountType != "" && in.MountType != normalized {
			return apperr.Conflict("document mount is immutable", "mount_conflict")
		}
		if in.GroupNo != "" && in.GroupNo != groupNo {
			return apperr.Conflict("document group is immutable", "mount_conflict")
		}
		if in.ThreadID != "" && in.ThreadID != threadID {
			return apperr.Conflict("document thread is immutable", "mount_conflict")
		}
		if normalized != "" || in.MountType == "" {
			in.MountType = normalized
		}
		in.mountContextKnown = true
	}
	in.UserPublish, in.SpaceID = existingUser, spaceID
	if groupNo != "" {
		in.GroupNo = groupNo
	}
	if threadID != "" {
		in.ThreadID = threadID
	}
	in.CreatorUID = creator
	return nil
}

func restoreLegacyMount(in *PublishInput, existingMount string, hasMount bool, groupNo, threadID string) error {
	requestKnown := in.mountContextKnown || in.MountTypePresent
	normalized := ""
	if in.MountType == "" {
		var err error
		normalized, err = normalizeMountType(existingMount)
		if err != nil {
			return err
		}
	}
	if !hasMount && normalized == "" {
		normalized = inferredLegacyMount(false, "", groupNo, threadID)
	}
	if in.MountType == "" && (!requestKnown || hasMount && normalized != "") {
		in.MountType = normalized
	}
	switch in.MountType {
	case "group":
		if in.GroupNo == "" {
			in.GroupNo = groupNo
		}
		in.ThreadID = ""
	case "thread":
		if in.ThreadID == "" {
			in.ThreadID = threadID
		}
		in.GroupNo = ""
	default:
		in.GroupNo, in.ThreadID = "", ""
	}
	in.mountContextKnown = requestKnown || hasMount || normalized != ""
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
	return s.ReplaceElementAuthorized(ctx, slug, baseVersion, aid, newHTML, nil)
}

// ReplaceElementAuthorized replaces an element after validating provenance.
func (s *DocService) ReplaceElementAuthorized(ctx context.Context, slug string, baseVersion int, aid, newHTML string, authorize ProvenanceAuthorizer) (*PublishResult, error) {
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
		in.pinnedAID = aid
		in.pinnedTag, _ = core.SingleTopLevelTag(newHTML)
		in.anchorMigrations = map[string]string{aid: canonicalAID}
		in.AuthorizeProvenance = authorize
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
	s.afterPublished(ctx, result)
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

// CreateCanonicalDraft allocates a canonical identity before saving its first
// draft. It uses the shared metadata locker exactly once after registration.
func (s *DocService) CreateCanonicalDraft(ctx context.Context, in PublishInput) (*DraftResult, error) {
	if in.HTML == "" {
		return nil, apperr.Validation("html (file) required", "html_required")
	}
	if int64(len(in.HTML)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
	}
	if strings.TrimSpace(in.Slug) != "" {
		return nil, apperr.Validation("canonical create must not include slug", "create_ref_forbidden")
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return nil, apperr.Validation("idempotency_key required for canonical create", "idempotency_key_required")
	}
	if strings.TrimSpace(in.PublisherToken) == "" {
		return nil, apperr.Unauthorized("canonical create requires bot authentication", "publisher_bot_required")
	}
	if s.register == nil {
		return nil, apperr.Upstream("docs-backend registrar is disabled", "registration_failed", nil)
	}
	mountType, err := normalizeMountType(in.MountType)
	if err != nil {
		return nil, err
	}
	in.MountType = mountType
	in.mountContextKnown = in.MountTypePresent || mountType != ""
	reg, err := s.register.Register(ctx, docsbackend.Registration{DocType: "html", IdempotencyKey: in.IdempotencyKey, MountType: in.MountType, Title: in.Title}, in.PublisherToken)
	if err != nil {
		return nil, apperr.Upstream("docs-backend registration failed", "registration_failed", err)
	}
	registered := true
	defer func() {
		if registered {
			_ = s.register.DeleteCanonical(ctx, reg.DocID, in.PublisherToken)
		}
	}()
	if config.SafeSlug(reg.DocID) == "" || reg.OctoDocSlug != reg.DocID {
		return nil, apperr.Upstream("docs-backend returned invalid document identity", "registration_invalid", nil)
	}
	if (in.PublisherUID != "" || in.PublisherSpaceID != "") && (in.PublisherUID == "" || in.PublisherSpaceID == "" || reg.PublisherUID != in.PublisherUID || reg.SpaceID != in.PublisherSpaceID) {
		return nil, apperr.Forbidden("registration publisher identity mismatch", "registration_identity_mismatch")
	}
	if s.lock == nil {
		return nil, apperr.Upstream("shared canonical initialization guard unavailable", "canonical_guard_unavailable", nil)
	}
	in.Slug = reg.DocID
	stamped := core.StampAids(in.HTML)
	var result *DraftResult
	err = s.lock.With(ctx, in.Slug, func() error {
		prev, normalized, e := s.prepareDraftProvenance(ctx, in.Slug, in)
		if e != nil {
			return e
		}
		if prev != nil {
			if marker, ok := prev.Extra[storage.CanonicalDraftCreateExtraKey]; ok && marker != nil {
				if draft, exists, derr := s.blobs.GetDraft(ctx, in.Slug); derr == nil && exists {
					result = &DraftResult{Slug: in.Slug, DocID: in.Slug, URL: fmt.Sprintf("%s/d/%s/draft", s.baseURL, in.Slug), Size: int64(len(draft)), AIDs: len(core.StampAids(draft).AIDs)}
					return nil
				}
				result = &DraftResult{Slug: in.Slug, DocID: in.Slug, URL: fmt.Sprintf("%s/d/%s/draft", s.baseURL, in.Slug)}
				return nil
			}
		}
		size, e := s.blobs.PutDraft(ctx, in.Slug, stamped.HTML)
		if e != nil {
			return apperr.Upstream("draft write failed", "draft_write_failed", e)
		}
		if e = s.setDraftMeta(ctx, in.Slug, in.Title, prev, normalized); e != nil {
			return e
		}
		meta, e := s.meta.GetMeta(ctx, in.Slug)
		if e != nil {
			return e
		}
		extra := map[string]any{}
		maps.Copy(extra, meta.Extra)
		extra[storage.CanonicalDocIDExtraKey] = reg.DocID
		extra[storage.CanonicalShareURLExtraKey] = reg.ShareURL
		extra[storage.CanonicalDraftCreateExtraKey] = true
		if e = s.meta.PutMeta(ctx, in.Slug, storage.DocMeta{Slug: meta.Slug, Title: meta.Title, Versions: meta.Versions, Extra: extra}); e != nil {
			return e
		}
		result = &DraftResult{Slug: in.Slug, DocID: in.Slug, URL: fmt.Sprintf("%s/d/%s/draft", s.baseURL, in.Slug), Size: size, AIDs: len(stamped.AIDs)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	registered = false
	if result == nil {
		return &DraftResult{Slug: in.Slug, DocID: in.Slug, URL: fmt.Sprintf("%s/d/%s/draft", s.baseURL, in.Slug)}, nil
	}
	return result, nil
}

// SaveDraft stamps and writes the mutable draft slot for a slug, creating the
// meta record if the slug is new (draft-only docs have an empty Versions list).
// The draft never enters the immutable version numbering until Promote.
//
// creatorUID is stamped into meta on first create only (draft-first ownership),
// exactly like Publish; a later save by a different caller never reassigns it,
// and the stamped creator carries through to the promoted version.
func (s *DocService) SaveDraft(ctx context.Context, slug, html, title, creatorUID string) (*DraftResult, error) {
	return s.SaveDraftWithProvenance(ctx, slug, html, title, PublishInput{CreatorUID: creatorUID})
}

// SaveDraftWithProvenance saves a draft and durable publishing identity so a
// later promote follows the same registration lifecycle as direct publish.
func (s *DocService) SaveDraftWithProvenance(ctx context.Context, slug, html, title string, provenance PublishInput) (*DraftResult, error) {
	if html == "" {
		return nil, apperr.Validation("html required", "html_required")
	}
	if int64(len(html)) > s.maxBytes {
		return nil, apperr.PayloadTooLarge(fmt.Sprintf("document exceeds %d bytes", s.maxBytes), "html_too_large")
	}
	stamped := core.StampAids(html)
	var result *DraftResult
	err := s.lock.With(ctx, slug, func() error {
		prev, normalized, validationErr := s.prepareDraftProvenance(ctx, slug, provenance)
		if validationErr != nil {
			return validationErr
		}
		if normalized.AuthorizeProvenance != nil {
			if authErr := normalized.AuthorizeProvenance(ctx, PublishProvenance{UserPublish: normalized.UserPublish, SpaceID: normalized.SpaceID, CreatorUID: normalized.CreatorUID}); authErr != nil {
				return authErr
			}
		}
		size, perr := s.blobs.PutDraft(ctx, slug, stamped.HTML)
		if perr != nil {
			return apperr.Upstream("draft write failed", "draft_write_failed", perr)
		}
		if merr := s.setDraftMeta(ctx, slug, title, prev, normalized); merr != nil {
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

// prepareDraftProvenance validates identity while the caller holds the slug
// lock. Existing provenance is inherited and can never be converted or moved.
func (s *DocService) prepareDraftProvenance(ctx context.Context, slug string, in PublishInput) (*storage.DocMeta, PublishInput, error) {
	if in.UserPublish {
		in.SpaceID = strings.TrimSpace(in.SpaceID)
		in.CreatorUID = strings.TrimSpace(in.CreatorUID)
		if in.CreatorUID == "" {
			return nil, PublishInput{}, apperr.Conflict("user publish provenance requires a creator", "publish_provenance_conflict")
		}
		if !ValidSpaceID(in.SpaceID) {
			return nil, PublishInput{}, apperr.Validation("valid space_id required", "space_id_invalid")
		}
		in.MountType, in.MountTypePresent = "space", true
	}
	if in.MountType != "" {
		in.MountTypePresent = true
	}
	mountType, err := normalizeMountType(in.MountType)
	if err != nil {
		return nil, PublishInput{}, err
	}
	in.MountType = mountType
	prev, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return prev, in, err
	}
	if prev == nil {
		exists, existsErr := s.slugExists(ctx, slug)
		if existsErr != nil {
			return nil, PublishInput{}, existsErr
		}
		if exists && in.UserPublish {
			return nil, PublishInput{}, apperr.Conflict("document publishing identity is immutable", "publish_provenance_conflict")
		}
		return nil, in, nil
	}
	existingUser, existingSpace, existingGroup, existingThread := prev.PublishProvenance()
	existingMount, hasMount := prev.MountType()
	if in.UserPublish && !existingUser {
		return nil, PublishInput{}, apperr.Conflict("document publishing identity is immutable", "publish_provenance_conflict")
	}
	if !existingUser {
		if restoreErr := restoreLegacyMount(&in, existingMount, hasMount, existingGroup, existingThread); restoreErr != nil {
			return nil, PublishInput{}, restoreErr
		}
		in.MountTypePresent = in.mountContextKnown
		if creator := prev.CreatorUID(); creator != "" {
			in.CreatorUID = creator
		}
		return prev, in, nil
	}
	creator := strings.TrimSpace(prev.CreatorUID())
	if creator == "" || !ValidSpaceID(existingSpace) {
		return nil, PublishInput{}, apperr.Conflict("user publish provenance is incomplete", "publish_provenance_conflict")
	}
	if hasMount {
		existingMount, err = normalizeMountType(existingMount)
		if err != nil {
			return nil, PublishInput{}, err
		}
	} else {
		existingMount = inferredLegacyMount(existingUser, existingSpace, existingGroup, existingThread)
	}
	if in.SpaceID != "" && existingSpace != "" && in.SpaceID != existingSpace {
		return nil, PublishInput{}, apperr.Conflict("document space is immutable", "space_conflict")
	}
	if in.MountTypePresent && hasMount && existingMount != "" && in.MountType != "" && in.MountType != existingMount {
		return nil, PublishInput{}, apperr.Conflict("document mount is immutable", "mount_conflict")
	}
	if in.GroupNo != "" && in.GroupNo != existingGroup {
		return nil, PublishInput{}, apperr.Conflict("document group is immutable", "mount_conflict")
	}
	if in.ThreadID != "" && in.ThreadID != existingThread {
		return nil, PublishInput{}, apperr.Conflict("document thread is immutable", "mount_conflict")
	}
	in.UserPublish, in.SpaceID = existingUser, existingSpace
	if existingGroup != "" {
		in.GroupNo = existingGroup
	}
	if existingThread != "" {
		in.ThreadID = existingThread
	}
	if existingMount != "" {
		in.MountType, in.MountTypePresent = existingMount, true
	}
	in.CreatorUID = creator
	return prev, in, nil
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
	return s.PromoteAuthorized(ctx, slug, title, nil)
}

// PromoteAuthorized promotes a draft after validating effective provenance.
func (s *DocService) PromoteAuthorized(ctx context.Context, slug, title string, authorize ProvenanceAuthorizer) (*PublishResult, error) {
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
		in.AuthorizeProvenance = authorize
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
	s.afterPublished(ctx, result)
	return result, nil
}

// setDraftMeta records a draft marker in the meta Extra catch-all, creating the
// meta record if the slug is new. It leaves Versions untouched. creatorUID is
// stamped on first create only (same rule as upsertMeta), never reassigning an
// existing creator.
func (s *DocService) setDraftMeta(ctx context.Context, slug, title string, prev *storage.DocMeta, provenance PublishInput) error {
	newMeta := prev == nil
	if newMeta {
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
	if provenance.CreatorUID != "" && prev.CreatorUID() == "" {
		extra[storage.CreatorUIDExtraKey] = provenance.CreatorUID
	}
	if provenance.UserPublish {
		extra[storage.UserPublishExtraKey] = true
		extra[storage.SpaceIDExtraKey] = provenance.SpaceID
		if state, _, _ := prev.DocsBackendRegistration(); newMeta && state == "" {
			extra[storage.DocsBackendRegistrationStateKey] = storage.DocsBackendRegistrationLocalOnly
		}
	}
	if provenance.MountTypePresent {
		extra[storage.MountTypeExtraKey] = provenance.MountType
		extra[storage.GroupNoExtraKey] = provenance.GroupNo
		extra[storage.ThreadIDExtraKey] = provenance.ThreadID
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

func (s *DocService) existingPublishInput(ctx context.Context, slug, html, title string) (PublishInput, error) {
	in := PublishInput{Slug: slug, HTML: html, Title: title}
	meta, err := s.meta.GetMeta(ctx, slug)
	if err != nil {
		return PublishInput{}, err
	}
	if meta == nil {
		return PublishInput{}, apperr.Conflict("document metadata is missing", "publish_provenance_conflict")
	}
	if mountType, ok := meta.MountType(); ok {
		normalized, normalizeErr := normalizeMountType(mountType)
		if normalizeErr != nil {
			return PublishInput{}, normalizeErr
		}
		in.MountType = normalized
		in.mountContextKnown = true
	}
	in.UserPublish, in.SpaceID, in.GroupNo, in.ThreadID = meta.PublishProvenance()
	in.CreatorUID = meta.CreatorUID()
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

// Remove deletes legacy/Bot documents. User-owned callers should use
// RemoveAuthorized so persisted provenance is checked before local cleanup.
func (s *DocService) Remove(ctx context.Context, slug string) error {
	return s.RemoveAuthorized(ctx, slug, nil)
}

// RemoveCanonical deletes the remote canonical identity before local state. A
// failed local cleanup intentionally leaves the already-idempotent backend
// delete retryable: the next request repeats the remote delete then completes
// the remaining local work.
func (s *DocService) RemoveCanonical(ctx context.Context, slug, token string) (bool, error) {
	canonical := false
	err := s.lock.With(ctx, slug, func() error {
		meta, err := s.meta.GetMeta(ctx, slug)
		if err != nil {
			return err
		}
		if meta == nil {
			return nil
		}
		docID, _, ok := meta.CanonicalIdentity()
		if !ok {
			return nil
		}
		canonical = true
		if docID != slug {
			return apperr.Conflict("canonical document identity mismatch", "canonical_identity_mismatch")
		}
		if s.register == nil {
			return apperr.Upstream("docs-backend deletion unavailable", "delete_failed", nil)
		}
		if strings.TrimSpace(token) == "" {
			return apperr.Unauthorized("canonical delete requires bot authentication", "publisher_bot_required")
		}
		if err := s.register.DeleteCanonical(ctx, docID, token); err != nil {
			return apperr.Upstream("docs-backend deletion failed", "delete_failed", err)
		}
		if err := s.blobs.DeleteDoc(ctx, slug); err != nil {
			return err
		}
		assets, err := s.meta.ListAssetMeta(ctx, slug)
		if err != nil {
			return err
		}
		for _, asset := range assets {
			if err := s.meta.DeleteAssetMeta(ctx, slug, asset.SHA256); err != nil {
				return err
			}
		}
		if _, err := s.comments.WipeLocked(ctx, slug); err != nil {
			return err
		}
		return s.meta.DeleteMeta(ctx, slug)
	})
	return canonical, err
}

// RemoveAuthorized deletes user-owned documents only when they were never
// registered in docs-backend; registered documents must start deletion there.
func (s *DocService) RemoveAuthorized(ctx context.Context, slug string, authorize ProvenanceAuthorizer) error {
	userDocument := false
	err := s.lock.With(ctx, slug, func() error {
		meta, metaErr := s.meta.GetMeta(ctx, slug)
		if metaErr != nil {
			return metaErr
		}
		userPublish, spaceID, _, _ := meta.PublishProvenance()
		userDocument = userPublish
		if userPublish {
			provenance := PublishProvenance{UserPublish: true, SpaceID: spaceID, CreatorUID: meta.CreatorUID()}
			if authorize == nil {
				return apperr.Forbidden("user publish provenance authorization required", "space_membership_required")
			}
			if authErr := authorize(ctx, provenance); authErr != nil {
				return authErr
			}
			state, docID, _ := meta.DocsBackendRegistration()
			if state != storage.DocsBackendRegistrationLocalOnly || docID != "" {
				return apperr.Conflict("registered or unconfirmed user documents must be deleted through docs-backend", "user_publish_delete_via_backend")
			}
		}
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
	if !userDocument {
		s.afterRemoved(slug)
	}
	return nil
}

func (s *DocService) setDocsBackendRegistrationState(ctx context.Context, slug, state, docID string, version int) bool {
	updated := false
	err := s.lock.With(ctx, slug, func() error {
		meta, err := s.meta.GetMeta(ctx, slug)
		if err != nil {
			return err
		}
		if meta == nil {
			return fmt.Errorf("metadata missing")
		}
		currentState, _, currentVersion := meta.DocsBackendRegistration()
		if currentState != storage.DocsBackendRegistrationPending || currentVersion != version {
			return fmt.Errorf("registration state changed to %q at version %d", currentState, currentVersion)
		}
		if meta.Extra == nil {
			meta.Extra = map[string]any{}
		}
		meta.Extra[storage.DocsBackendRegistrationStateKey] = state
		meta.Extra[storage.DocsBackendRegistrationVersionKey] = version
		meta.Extra[storage.DocsBackendDocIDExtraKey] = docID
		if err = s.meta.PutMeta(ctx, slug, *meta); err != nil {
			return err
		}
		updated = true
		return nil
	})
	if err != nil {
		s.log().Warn("docs_backend_registration_state failed after publish", "slug", slug, "err", err.Error())
	}
	return updated
}

func (s *DocService) afterPublished(parent context.Context, result *PublishResult) {
	if result == nil {
		return
	}
	reg, ok := s.registrationForMount(result.Slug, result.title, result.mountType)
	if result.userPublish {
		reg.SpaceID = result.spaceID
		reg.Owner = result.owner
		reg.Internal = true
	}
	if !ok {
		if result.mountContextKnown {
			result.Status = publishStatusUnregistered
			return
		}
		s.afterLegacyPublished(parent, result)
		return
	}
	if s.register == nil {
		result.Status = publishStatusRegisterFailed
		s.log().Warn("docs_backend_register unavailable after publish", "slug", result.Slug, "version", result.Version)
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, docsBackendSideEffectTimeout)
	defer cancel()
	var registration *docsbackend.RegistrationResult
	var err error
	for attempt := 1; attempt <= docsBackendRegisterAttempts; attempt++ {
		attemptCtx, attemptCancel := context.WithTimeout(ctx, docsBackendAttemptTimeout)
		registration, err = s.register.Register(attemptCtx, reg, result.publisherToken)
		attemptCancel()
		if err == nil {
			break
		}
		if attempt == docsBackendRegisterAttempts || !waitForRetry(ctx, docsBackendRegisterDelay) {
			break
		}
	}
	if err == nil && registration == nil {
		err = fmt.Errorf("docs-backend registration returned no result")
	}
	if err != nil {
		result.Status = publishStatusRegisterFailed
		s.log().Warn("docs_backend_register failed after publish", "slug", result.Slug, "version", result.Version, "err", err.Error())
		return
	}
	if ctx.Err() != nil {
		result.Status = publishStatusRegisterFailed
		return
	}
	result.Status = publishStatusPublished
	if result.userPublish {
		if !s.setDocsBackendRegistrationState(parent, result.Slug, storage.DocsBackendRegistrationRegistered, registration.DocID, result.Version) {
			result.Status = publishStatusRegisterFailed
			return
		}
	}
	result.DocID = registration.DocID
	result.URL = registration.ShareURL
	result.ShareURL = registration.ShareURL
	result.Registered = true
	if result.hadMeta && result.titleChanged && !result.userPublish {
		s.register.Rename(ctx, result.Slug, reg.Title, result.publisherToken)
	}
	if ctx.Err() != nil {
		return
	}
	if s.reconcileFn != nil {
		if reconcileErr := s.reconcileFn(ctx, result.Slug); reconcileErr != nil {
			s.log().Error("grant_reconcile_failed", "slug", result.Slug, "err", reconcileErr.Error())
		}
	}
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
	switch mountType {
	case "":
		s.log().Debug("docs_backend_register skipped: no mount_type", "slug", slug)
		return docsbackend.Registration{}, false
	case "group", "space", "thread":
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

func normalizeMountType(mountType string) (string, error) {
	mountType = strings.ToLower(strings.TrimSpace(mountType))
	switch mountType {
	case "", "group", "space", "thread":
		return mountType, nil
	default:
		return "", apperr.Validation("mount_type must be one of empty, group, space, or thread", "mount_type_invalid")
	}
}

func inferredLegacyMount(userPublish bool, spaceID, groupNo, threadID string) string {
	switch {
	case threadID != "":
		return "thread"
	case groupNo != "":
		return "group"
	case userPublish || spaceID != "":
		return "space"
	default:
		return ""
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
	if in.mountContextKnown || in.UserPublish || (in.CreatorUID != "" && prev.CreatorUID() == "") {
		extra = map[string]any{}
		maps.Copy(extra, prev.Extra)
	}
	if in.mountContextKnown {
		extra[storage.MountTypeExtraKey] = in.MountType
		extra[storage.GroupNoExtraKey] = in.GroupNo
		extra[storage.ThreadIDExtraKey] = in.ThreadID
	}
	if in.UserPublish {
		extra[storage.UserPublishExtraKey] = true
		extra[storage.SpaceIDExtraKey] = in.SpaceID
		extra[storage.DocsBackendRegistrationStateKey] = storage.DocsBackendRegistrationPending
		extra[storage.DocsBackendRegistrationVersionKey] = version
	}
	if in.CreatorUID != "" && prev.CreatorUID() == "" {
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
		titleChanged: hadMeta && strings.TrimSpace(prev.Title) != "" && prev.Title != title,
	}, nil
}

func sortVersions(v []storage.VersionRef) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1].N > v[j].N; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
}

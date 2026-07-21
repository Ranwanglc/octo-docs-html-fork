package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/core"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/octoidentity"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// publishBody is the parsed publish input from JSON or multipart.
type publishBody struct {
	Slug          string
	HTML          string
	Version       int
	Title         string
	LocalComments []core.Comment

	// Mount info the publishing bot supplies so the doc can be registered into
	// docs-backend (and thus appear in the sidebar) without a doc_binding lookup.
	MountType string
	GroupNo   string
	ThreadID  string
}

func (s *Server) readPublishBody(w http.ResponseWriter, r *http.Request) (publishBody, error) {
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	if strings.Contains(ct, "multipart/form-data") {
		return s.readMultipart(r)
	}
	return s.readJSONPublish(w, r)
}

func (s *Server) readMultipart(r *http.Request) (publishBody, error) {
	if err := r.ParseMultipartForm(s.cfg.MaxHTMLBytes + 1<<20); err != nil {
		return publishBody{}, apperr.Validation("invalid multipart body", "invalid_multipart")
	}
	var b publishBody
	b.Slug = r.FormValue("slug")
	b.Title = r.FormValue("title")
	if v := r.FormValue("version"); v != "" {
		b.Version, _ = strconv.Atoi(v)
	}
	b.MountType = r.FormValue("mount_type")
	b.GroupNo = r.FormValue("group_no")
	b.ThreadID = r.FormValue("thread_id")
	if file, _, err := r.FormFile("file"); err == nil {
		defer func() { _ = file.Close() }()
		data, rerr := io.ReadAll(file)
		if rerr != nil {
			return publishBody{}, apperr.Validation("could not read file", "file_read_failed")
		}
		b.HTML = string(data)
	} else if h := r.FormValue("html"); h != "" {
		b.HTML = h
	}
	return b, nil
}

func (s *Server) readJSONPublish(w http.ResponseWriter, r *http.Request) (publishBody, error) {
	var raw struct {
		Slug    string `json:"slug"`
		HTML    string `json:"html"`
		Version int    `json:"version"`
		Title   string `json:"title"`
		Meta    *struct {
			Title string `json:"title"`
		} `json:"meta"`
		Comments []core.Comment `json:"comments"`
		// Mount info forwarded to docs-backend registration. snake_case matches the
		// rest of the publish contract; the bot supplies where it is publishing.
		MountType string `json:"mount_type"`
		GroupNo   string `json:"group_no"`
		ThreadID  string `json:"thread_id"`
	}
	if r.Body != nil {
		// Publish bodies carry the document HTML, so cap at the HTML limit plus JSON
		// framing headroom rather than the small default JSON cap. The service layer
		// still enforces MAX_HTML_BYTES on the decoded HTML field itself.
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxHTMLBytes+1<<20)
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return publishBody{}, apperr.PayloadTooLarge("request body too large", "body_too_large")
			}
			// Other decode errors fall through: a missing/invalid body surfaces as the
			// service-layer "html required" 400, preserving prior tolerance.
		}
	}
	// The CLI sends the doc's meta.json under `meta` (the documented contract:
	// {slug, version, html, meta, comments}). Honor meta.title, but let an
	// explicit top-level `title` win if both are present.
	title := raw.Title
	if title == "" && raw.Meta != nil {
		title = raw.Meta.Title
	}
	return publishBody{
		Slug: raw.Slug, HTML: raw.HTML, Version: raw.Version, Title: title, LocalComments: raw.Comments,
		MountType: raw.MountType, GroupNo: raw.GroupNo, ThreadID: raw.ThreadID,
	}, nil
}

// creatorUIDFromCtx returns the user uid to stamp as a doc's creator. creator_uid
// always stores a USER uid: for a bot session that is the bot's OwnerUID (the
// user behind the bot), so the bot and its owner share author; for a real octo
// user it is their own Login. "" when no session (caller was already gated).
func creatorUIDFromCtx(ctx context.Context) string {
	if bs := botSessionFromCtx(ctx); bs != nil && bs.OwnerUID != "" {
		return bs.OwnerUID
	}
	if sess := octoSessionFromCtx(ctx); sess != nil {
		return sess.Login
	}
	return ""
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) error {
	body, err := s.readPublishBody(w, r)
	if err != nil {
		return err
	}
	slug, err := requireSlug(body.Slug)
	if err != nil {
		return err
	}
	if body.HTML == "" {
		return apperr.Validation("html (file) required", "html_required")
	}
	// The creator is the user behind the authenticated publisher: a bot stamps its
	// OwnerUID (so bot + owner share author), a real user stamps their own uid.
	// Stamped into DocMeta on first create only; requireWriteOrBotOwnerAuth already
	// guaranteed a session is present.
	creatorUID := creatorUIDFromCtx(r.Context())
	res, err := s.docs.Publish(r.Context(), service.PublishInput{
		Slug: slug, HTML: body.HTML, Version: body.Version, Title: body.Title, LocalComments: body.LocalComments,
		MountType: body.MountType, GroupNo: body.GroupNo, ThreadID: body.ThreadID,
		CreatorUID:     creatorUID,
		PublisherToken: botTokenFromCtx(r.Context()),
	})
	if err != nil {
		return err
	}
	writeData(w, 200, res)
	return nil
}

// handleSaveDraft writes the mutable draft slot (PUT /v1/docs/{slug}/draft).
// Write-auth gated. The body is the same shape as publish, minus version.
func (s *Server) handleSaveDraft(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	body, err := s.readPublishBody(w, r)
	if err != nil {
		return err
	}
	if body.HTML == "" {
		return apperr.Validation("html (file) required", "html_required")
	}
	// Stamp creator on first draft (draft-first create) with the same owner rule
	// as publish, so the draft's author survives into the promoted version.
	res, err := s.docs.SaveDraft(r.Context(), slug, body.HTML, body.Title, creatorUIDFromCtx(r.Context()))
	if err != nil {
		return err
	}
	writeData(w, 200, res)
	return nil
}

// handlePromote promotes the draft to a new immutable version
// (POST /v1/docs/{slug}/draft/promote). Write-auth gated.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	// Optional {title} override.
	var raw struct {
		Title string `json:"title"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&raw)
	}
	res, err := s.docs.Promote(r.Context(), slug, raw.Title)
	if err != nil {
		return err
	}
	writeData(w, 200, res)
	return nil
}

// handleRenderDraft renders the draft slot (GET/HEAD /d/{slug}/draft) with the
// overlay in "draft" mode. Write-auth gated — a draft is author-only until promoted.
func (s *Server) handleRenderDraft(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	data, err := s.docs.GetDraft(r.Context(), slug)
	if err != nil {
		return err
	}
	if data == nil {
		return apperr.NotFound("Not found: " + slug + " draft")
	}
	session, err := s.resolveViewerSession(r)
	if err != nil {
		return err
	}
	// Draft mode: the overlay shows a Publish affordance (promote) instead of
	// Share/Fork. Version 0 signals "not yet a committed version".
	creatorName, creatorAvatar := resolveCreatorDisplay(r.Context(), data.CreatorUID, userToken(r))
	html, err := core.InjectOverlayCfg(data.HTML, s.overlayJS, core.OverlayConfig{
		Slug:           slug,
		Title:          data.Title,
		Version:        0,
		Identity:       identityFromSession(session),
		IsOwner:        s.auth.IsOwner(session),
		AuthConfigured: s.auth.LoginEnabled(),
		Mode:           "draft",
		Versions:       toVersionRefs(data.Versions, 0),
		HostOrigins:    s.cfg.HostOrigins,
		CreatorUID:     data.CreatorUID,
		CreatorName:    creatorName,
		CreatorAvatar:  creatorAvatar,
		CreatedAt:      data.CreatedAt,
	})
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(200)
		return nil
	}
	_, _ = io.WriteString(w, html)
	return nil
}

// handleShare mints (or rotates) the per-doc share code and returns a coded read
// URL. Author-only. POST /v1/docs/{slug}/share.
func (s *Server) handleShare(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	code, err := s.auth.GenerateCode(r.Context(), slug)
	if err != nil {
		return err
	}
	// Point at the latest version if one exists, else the doc root.
	url := s.cfg.BaseURL + "/d/" + slug + "/v/1?code=" + code
	if vl, verr := s.docs.ListVersions(r.Context(), slug); verr == nil && vl != nil && len(vl.Versions) > 0 {
		latest := vl.Versions[len(vl.Versions)-1].N
		url = s.cfg.BaseURL + "/d/" + slug + "/v/" + strconv.Itoa(latest) + "?code=" + code
	}
	writeData(w, 200, map[string]any{"slug": slug, "code": code, "url": url})
	return nil
}

// handleRevokeShare clears the per-doc share code (existing links stop working).
// Author-only. DELETE /v1/docs/{slug}/share.
func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	if err := s.auth.RevokeCode(r.Context(), slug); err != nil {
		return err
	}
	writeData(w, 200, map[string]any{"slug": slug, "revoked": true})
	return nil
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	res, err := s.docs.ListVersions(r.Context(), slug)
	if err != nil {
		return err
	}
	if res == nil {
		return apperr.NotFound("")
	}
	writeData(w, 200, toVersionListDTO(res))
	return nil
}

func (s *Server) handleGetDoc(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	res, err := s.docs.ListVersions(r.Context(), slug)
	if err != nil {
		return err
	}
	if res == nil {
		return apperr.NotFound("")
	}
	refs := toVersionRefDTOs(res)
	dto := docDetailDTO{
		Slug:     res.Slug,
		Title:    res.Title,
		Versions: refs,
	}
	if n := len(res.Versions); n > 0 {
		latest := res.Versions[n-1]
		dto.Latest = latest.N
		dto.Updated = latest.Created
	}
	writeData(w, 200, dto)
	return nil
}

func (s *Server) handleDeleteDoc(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	if err := s.docs.Remove(r.Context(), slug); err != nil {
		return err
	}
	writeData(w, 200, struct{}{})
	return nil
}

func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	version, err := s.resolveVersionParam(r.Context(), slug, chi.URLParam(r, "version"))
	if err != nil {
		return err
	}
	data, err := s.docs.Render(r.Context(), slug, version)
	if err != nil {
		return err
	}
	if data == nil {
		return apperr.NotFound("Not found: " + slug + " v" + chi.URLParam(r, "version"))
	}

	session, err := s.resolveViewerSession(r)
	if err != nil {
		return err
	}
	// Is this viewer the author (write token via Bearer or cookie) or a reader
	// (share code)? Both reach here through requireDocReadHTML, but only the author
	// may mint/rotate a share code — so the overlay must hide the Share CTA from a
	// reader (clicking it would 404). We carry the flag OUTSIDE core.OverlayConfig
	// (which is byte-frozen) as a separate window.__ODOC_CAP__ marker.
	cap, err := s.resolveCap(r, slug)
	if err != nil {
		return err
	}
	// A doc rendered by this server is, by definition, published — so the overlay
	// always runs in "published" mode (Share/Fork, never a Publish button; that
	// belongs to the local preview server). AuthConfigured is always true in
	// fusion: the reverse proxy owns login, so every render is a signed-in one
	// (the overlay never needs to render a sign-in affordance).
	versions := toVersionRefs(data.Versions, version)
	// Sign inline asset URLs so the browser's native <img> loads (which carry no
	// token header) are authorized by a per-asset signature — see signAssetURLs.
	signedHTML := s.signAssetURLs(slug, data.HTML)
	creatorName, creatorAvatar := resolveCreatorDisplay(r.Context(), data.CreatorUID, userToken(r))
	html, err := core.InjectOverlayCfg(signedHTML, s.overlayJS, core.OverlayConfig{
		Slug:           slug,
		Title:          data.Title,
		Version:        version,
		Identity:       identityFromSession(session),
		IsOwner:        s.auth.IsOwner(session),
		AuthConfigured: s.auth.LoginEnabled(),
		Mode:           "published",
		Versions:       versions,
		HostOrigins:    s.cfg.HostOrigins,
		CreatorUID:     data.CreatorUID,
		CreatorName:    creatorName,
		CreatorAvatar:  creatorAvatar,
		CreatedAt:      data.CreatedAt,
	})
	if err != nil {
		return err
	}
	html = injectCapMarker(html, cap == service.CapAuthor)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		w.WriteHeader(200)
		return nil
	}
	_, _ = io.WriteString(w, html)
	return nil
}

func (s *Server) resolveVersionParam(ctx context.Context, slug, raw string) (int, error) {
	if strings.ToLower(strings.TrimSpace(raw)) == "latest" {
		list, err := s.docs.ListVersions(ctx, slug)
		if err != nil {
			return 0, err
		}
		if list == nil || len(list.Versions) == 0 {
			return 0, apperr.NotFound("")
		}
		return list.Versions[len(list.Versions)-1].N, nil
	}
	version, ok := parseVersionParam(raw)
	if !ok {
		return 0, apperr.NotFound("")
	}
	return version, nil
}

// resolveCreatorDisplay looks CreatorUID up through the process octoidentity
// provider and returns (name, avatar) for the overlay. Fail-soft: no provider
// (ErrDisabled), empty uid, upstream error, or empty fields all fold to empty
// strings so the render path never blocks on the identity server — omitempty on
// OverlayConfig then keeps the wire bytes clean and the frontend falls back to
// the raw uid.
func resolveCreatorDisplay(ctx context.Context, uid, callerToken string) (name, avatar string) {
	if uid == "" {
		return "", ""
	}
	provider, err := octoidentity.Get()
	if err != nil || provider == nil {
		return "", ""
	}
	u, err := provider.GetUser(ctx, uid, callerToken)
	if err != nil || u == nil {
		return "", ""
	}
	return u.Name, u.Avatar
}

// injectCapMarker adds window.__ODOC_CAP__ before the overlay script so the
// overlay can gate author-only UI (the Share/mint-code button) without touching
// the byte-frozen core.OverlayConfig. It is injected right before the overlay's
// own <script> so it is defined when the overlay boots.
func injectCapMarker(html string, isAuthor bool) string {
	marker := `<script>window.__ODOC_CAP__ = {isAuthor: ` + strconv.FormatBool(isAuthor) + `};</script>`
	// The overlay boot is the last "<script>" InjectOverlayCfg wrote; place the
	// marker before the window.__ODOC__ config script so both precede the overlay.
	const anchor = "<script>window.__ODOC__ = "
	if i := strings.Index(html, anchor); i >= 0 {
		return html[:i] + marker + "\n" + html[i:]
	}
	return html + marker
}

func (s *Server) handleForkExport(w http.ResponseWriter, r *http.Request) error {
	kind := chi.URLParam(r, "kind")
	if kind != "export" && kind != "fork" {
		return apperr.NotFound("")
	}
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	version, err := s.resolveVersionParam(r.Context(), slug, chi.URLParam(r, "version"))
	if err != nil {
		return err
	}
	data, err := s.docs.Render(r.Context(), slug, version)
	if err != nil {
		return err
	}
	if data == nil {
		return apperr.NotFound("Not found: " + slug + " v" + chi.URLParam(r, "version"))
	}
	list, err := s.comments.List(r.Context(), slug, version)
	if err != nil {
		return err
	}
	out, err := buildForkExport(forkExportInput{
		Slug: slug, Version: version, HTML: data.HTML, Comments: list, Kind: kind,
		OverlayJS: s.overlayJS, Now: nowISO(),
	})
	if err != nil {
		return err
	}
	dl := r.URL.Query().Get("download")
	force := dl == "1" || (kind == "export" && dl != "0")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if force {
		w.Header().Set("Content-Disposition", `attachment; filename="`+slug+"-v"+strconv.Itoa(version)+`-fork.html"`)
	}
	_, _ = io.WriteString(w, out)
	return nil
}

// toVersionRefs converts storage version refs to overlay version refs, falling
// back to the single current version when none are stored.
func toVersionRefs(stored []storage.VersionRef, current int) []core.VersionRef {
	if len(stored) == 0 {
		return []core.VersionRef{{N: current}}
	}
	out := make([]core.VersionRef, 0, len(stored))
	for _, v := range stored {
		out = append(out, core.VersionRef{N: v.N, Created: v.Created})
	}
	return out
}

// docListItem is the DTO returned by handleListDocs. `updated` is omitted when the
// doc has no versions (should not happen in practice, but keeps the envelope stable).
type docListItem struct {
	Slug    string  `json:"slug"`
	Title   string  `json:"title"`
	Latest  int     `json:"latest"`
	Updated *string `json:"updated,omitempty"`
}

// parseListPagination normalises the owner-catalog pagination inputs. Accepts
// the canonical page / page_size pair; also accepts limit / offset aliases and
// converts internally (page_size = limit, page = offset/limit + 1). Canonical
// wins when present: if either `page` or `page_size` is in the query, the
// alias branch is skipped entirely — limit/offset are not parsed or validated,
// so junk aliases alongside a valid canonical pair still yield 200.
// Alias offset must be a multiple of the resolved limit (400 otherwise), so
// non-aligned slicing does not silently drop the sub-page remainder.
// clamp rules: page_size ∈ [1,100] (out-of-range → default 20), page ≥ 1.
func parseListPagination(q map[string][]string) (page, pageSize int, err error) {
	const defPageSize = 20
	const maxPageSize = 100
	page, pageSize = 1, defPageSize

	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	parse := func(k string) (int, bool, error) {
		raw := get(k)
		if raw == "" {
			return 0, false, nil
		}
		n, cerr := strconv.Atoi(raw)
		if cerr != nil {
			return 0, false, apperr.Validation(k+" must be an integer", "invalid_"+k)
		}
		return n, true, nil
	}

	// Canonical short-circuit: presence of `page` or `page_size` locks aliases
	// out completely, so junk `limit=xyz&offset=abc` alongside a canonical pair
	// no longer trips validation.
	_, hasPage := q["page"]
	_, hasSize := q["page_size"]
	if hasPage || hasSize {
		if n, ok, cerr := parse("page"); cerr != nil {
			return 0, 0, cerr
		} else if ok {
			if n < 1 {
				n = 1
			}
			page = n
		}
		if n, ok, cerr := parse("page_size"); cerr != nil {
			return 0, 0, cerr
		} else if ok {
			if n > maxPageSize {
				n = maxPageSize
			} else if n < 1 {
				n = defPageSize
			}
			pageSize = n
		}
		return page, pageSize, nil
	}

	// Alias branch (canonical absent): limit drives page_size; bare offset with
	// no limit is ambiguous and silently ignored (matches most REST APIs).
	// Offset must divide evenly into limit so we do not lose the remainder.
	if n, ok, cerr := parse("limit"); cerr != nil {
		return 0, 0, cerr
	} else if ok {
		if n > maxPageSize {
			n = maxPageSize
		} else if n < 1 {
			n = defPageSize
		}
		pageSize = n
		if off, ok2, cerr2 := parse("offset"); cerr2 != nil {
			return 0, 0, cerr2
		} else if ok2 && off > 0 {
			if off%pageSize != 0 {
				return 0, 0, apperr.Validation("offset must be a multiple of limit", "invalid_offset_alignment")
			}
			page = off/pageSize + 1
		}
	}
	return page, pageSize, nil
}

// handleListDocs is the owner-scope document index. Non-owner viewers are
// refused with the JSON envelope error (401 unauthenticated, 403 authenticated
// but not owner); a page HTML redirect is inappropriate here because callers
// are CLIs and agents. See handleCatalog for the HTML equivalent.
func (s *Server) handleListDocs(w http.ResponseWriter, r *http.Request) error {
	session, err := s.resolveViewerSession(r)
	if err != nil {
		return err
	}
	if session == nil {
		return apperr.Unauthorized("", "")
	}
	if !s.auth.IsOwner(session) {
		return apperr.Forbidden("owner only", "not_owner")
	}
	page, pageSize, err := parseListPagination(r.URL.Query())
	if err != nil {
		return err
	}
	docs, err := s.docs.ListAllForOwner(r.Context())
	if err != nil {
		return err
	}
	total := len(docs)
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	var slice []service.OwnerDoc
	if start < end {
		slice = docs[start:end]
	}
	// Pre-allocated empty slice keeps writeList emitting [] instead of null when
	// the page is empty (out-of-range page, zero-doc store, etc.).
	items := make([]docListItem, 0, len(slice))
	for _, d := range slice {
		items = append(items, docListItem{
			Slug:    d.Slug,
			Title:   d.Title,
			Latest:  d.Latest,
			Updated: d.LatestCreated,
		})
	}
	writeList(w, items, pagination{Total: total, Page: page, PageSize: pageSize})
	return nil
}

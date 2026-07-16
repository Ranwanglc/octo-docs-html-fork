package httpx

import (
	"bytes"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lml2468/octo-doc/internal/platform/apperr"
)

// Media assets: per-doc, content-addressed binary resources (images, video, …)
// that documents reference by same-origin URL. Uploads/deletes are author-only;
// reads carry the doc's reader capability, identical to rendering a version. See
// docs/ASSETS.md.

// handleUploadAsset stores an uploaded asset and returns its stable URL.
// Author-only. POST /v1/docs/{slug}/assets (multipart, field "file").
func (s *Server) handleUploadAsset(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	// Bound the request body at the transport edge so an oversized upload is
	// rejected before ParseMultipartForm spools it to a temp file. The +1 MiB
	// headroom covers multipart framing/other fields; the exact per-asset cap is
	// enforced in the service (with a typed 413).
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxAssetBytes+1<<20)
	if err := r.ParseMultipartForm(s.cfg.MaxAssetBytes + 1<<20); err != nil {
		return apperr.Validation("invalid multipart body", "invalid_multipart")
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		return apperr.Validation("missing file field", "file_required")
	}
	defer func() { _ = file.Close() }()

	name := ""
	if header != nil {
		name = header.Filename
	}
	res, err := s.assets.Put(r.Context(), slug, file, name)
	if err != nil {
		return err
	}
	writeData(w, 200, map[string]any{
		"slug":   slug,
		"sha256": res.SHA256,
		"mime":   res.MIME,
		"size":   res.Size,
		"url":    s.assetURL(slug, res.SHA256),
	})
	return nil
}

// handleListAssets lists a doc's assets. Reader capability. GET /v1/docs/{slug}/assets.
func (s *Server) handleListAssets(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	list, err := s.assets.List(r.Context(), slug)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(list))
	for _, a := range list {
		out = append(out, map[string]any{
			"sha256": a.SHA256, "mime": a.MIME, "size": a.Size,
			"original_name": a.OriginalName, "created": a.Created,
			"url": s.assetURL(slug, a.SHA256),
		})
	}
	writeData(w, 200, map[string]any{"slug": slug, "assets": out})
	return nil
}

// handleDeleteAsset removes one asset. Author-only. DELETE /v1/docs/{slug}/assets/{sha256}.
func (s *Server) handleDeleteAsset(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	sha, err := requireSHA256(chi.URLParam(r, "sha256"))
	if err != nil {
		return err
	}
	if err := s.assets.Delete(r.Context(), slug, sha); err != nil {
		return err
	}
	writeData(w, 200, map[string]any{"slug": slug, "sha256": sha, "deleted": true})
	return nil
}

// handleServeAsset returns raw asset bytes for the doc HTML to reference. Reader
// capability (gated by requireDocReadHTML, same as a version render). The bytes
// are attacker-supplied, so they are served under a locked-down CSP + nosniff so
// they can never execute as an active document — this is why the route does NOT
// reuse docSecurityHeaders. GET/HEAD /d/{slug}/assets/{sha256}.
//
// Serving goes through http.ServeContent, which adds Range / Accept-Ranges / 206
// support so browsers can seek within <video>/<audio> without fetching the whole
// file, and handles HEAD and conditional requests. It respects the Content-Type
// we set (no sniffing) and leaves our other headers intact.
func (s *Server) handleServeAsset(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	sha, err := requireSHA256(chi.URLParam(r, "sha256"))
	if err != nil {
		return err
	}
	data, meta, err := s.assets.Get(r.Context(), slug, sha)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", meta.MIME)
	h.Set("X-Content-Type-Options", "nosniff")
	// Neutralize the asset as an execution context: no scripts, no framing, sandboxed.
	// Critical for user-supplied SVG/HTML-ish bytes served same-origin.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// This route sets headers by hand (not secHeaders, which carries a doc CSP), so
	// re-apply the framing/referrer protections secHeaders would have added: an
	// asset must not be frameable cross-origin or leak a Referer.
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	// Content-addressed URL ⇒ the bytes can never change ⇒ cache forever. Private,
	// since a doc's assets inherit its per-doc access control.
	h.Set("Cache-Control", "private, max-age=31536000, immutable")
	// Empty name + zero modtime: Content-Type is already set (no extension sniff),
	// and the content-addressed immutable cache makes Last-Modified redundant.
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	return nil
}

// assetURL builds the absolute, same-origin URL a document references.
func (s *Server) assetURL(slug, sha string) string {
	return s.cfg.BaseURL + "/d/" + slug + "/assets/" + sha
}

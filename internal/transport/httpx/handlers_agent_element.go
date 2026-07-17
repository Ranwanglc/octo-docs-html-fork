package httpx

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
)

// handleAgentElementGet returns the outer HTML of one artifact located by aid in
// a document version (version 0 = latest). Write-token gated. The heavy lifting
// (version resolve, aid lookup via core parse) lives in DocService.GetElement.
func (s *Server) handleAgentElementGet(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Slug    string `json:"slug"`
		Version int    `json:"version"`
		AID     string `json:"aid"`
	}
	_ = decodeJSON(w, r, &body)
	slug, err := requireSlug(body.Slug)
	if err != nil {
		return err
	}
	if err := s.requireDocAuthorSlug(r, slug); err != nil {
		return err
	}
	if body.AID == "" {
		return apperr.Validation("aid required", "aid_required")
	}
	view, err := s.docs.GetElement(r.Context(), slug, body.Version, body.AID)
	if err != nil {
		return err
	}
	writeData(w, http.StatusOK, view)
	return nil
}

// handleAgentElementReplace swaps one artifact's outer HTML (located by aid in
// base_version, 0 = latest) with new_html and republishes a new version.
// Write-token gated. Validation (single-element fragment, size, aid existence)
// and the re-stamp/anchor-reconcile all happen in DocService.ReplaceElement via
// Publish — the handler never stamps.
func (s *Server) handleAgentElementReplace(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Slug        string `json:"slug"`
		BaseVersion int    `json:"base_version"`
		AID         string `json:"aid"`
		NewHTML     string `json:"new_html"`
	}
	// new_html can approach the doc size limit, so cap the JSON body at the HTML
	// limit (+ framing headroom) instead of the small default JSON cap; the
	// service still enforces MAX_HTML_BYTES on new_html itself.
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxHTMLBytes+1<<20)
		if derr := json.NewDecoder(r.Body).Decode(&body); derr != nil {
			var mbe *http.MaxBytesError
			if errors.As(derr, &mbe) {
				return apperr.PayloadTooLarge("request body too large", "body_too_large")
			}
			// Other decode errors fall through to the field-level validation below.
		}
	}
	slug, err := requireSlug(body.Slug)
	if err != nil {
		return err
	}
	if err := s.requireDocAuthorSlug(r, slug); err != nil {
		return err
	}
	if body.AID == "" {
		return apperr.Validation("aid required", "aid_required")
	}
	if body.NewHTML == "" {
		return apperr.Validation("new_html required", "new_html_required")
	}
	res, err := s.docs.ReplaceElement(r.Context(), slug, body.BaseVersion, body.AID, body.NewHTML)
	if err != nil {
		return err
	}
	writeData(w, http.StatusOK, res)
	return nil
}

package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/lml2468/octo-doc/internal/platform/apperr"
)

// grantDTO is one row of the grants listing. It mirrors the rich-doc members
// contract ({uid, role, source, grantedBy}) so a shared front-end panel can
// consume both backends without special-casing.
type grantDTO struct {
	UID       string `json:"uid"`
	Role      string `json:"role"`
	Source    string `json:"source"`
	GrantedBy string `json:"grantedBy"`
}

// handleListGrants lists a doc's access grants, with the creator synthesized as
// the leading author row. Author-only. GET /v1/docs/{slug}/grants.
func (s *Server) handleListGrants(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	grants, err := s.auth.ListGrants(r.Context(), slug)
	if err != nil {
		return err
	}
	items := []grantDTO{}
	meta, err := s.auth.MetaFor(r.Context(), slug)
	if err != nil {
		return err
	}
	if meta != nil {
		if creator := meta.CreatorUID(); creator != "" {
			items = append(items, grantDTO{UID: creator, Role: "author", Source: "owner"})
		}
	}
	for uid, role := range grants {
		items = append(items, grantDTO{UID: uid, Role: role, Source: "direct"})
	}
	writeData(w, 200, map[string]any{"items": items})
	return nil
}

// handleAddGrant grants a uid reader access (upsert by uid). Author-only.
// PUT /v1/docs/{slug}/grants  body {uid, role}.
func (s *Server) handleAddGrant(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	var body struct {
		UID  string `json:"uid"`
		Role string `json:"role"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body)
	}
	if err := s.auth.AddGrant(r.Context(), slug, body.UID, body.Role, creatorUIDFromCtx(r.Context())); err != nil {
		return err
	}
	writeData(w, 200, map[string]any{"slug": slug, "uid": body.UID, "role": body.Role})
	return nil
}

// handleRemoveGrant revokes a uid's grant. The creator cannot be removed (they
// own the doc). Author-only. DELETE /v1/docs/{slug}/grants/{uid}.
func (s *Server) handleRemoveGrant(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(chi.URLParam(r, "slug"))
	if err != nil {
		return err
	}
	uid := chi.URLParam(r, "uid")
	if uid == "" {
		return apperr.Validation("uid required", "invalid_grant")
	}
	meta, err := s.auth.MetaFor(r.Context(), slug)
	if err != nil {
		return err
	}
	if meta != nil && meta.CreatorUID() == uid {
		return apperr.Conflict("creator cannot be removed", "creator_not_removable")
	}
	if err := s.auth.RemoveGrant(r.Context(), slug, uid); err != nil {
		return err
	}
	writeData(w, 200, map[string]any{"slug": slug, "uid": uid, "removed": true})
	return nil
}

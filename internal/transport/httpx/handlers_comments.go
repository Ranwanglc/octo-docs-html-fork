package httpx

import (
	"encoding/json"
	"net/http"

	"github.com/lml2468/octo-doc/internal/core"
	"github.com/lml2468/octo-doc/internal/platform/apperr"
	"github.com/lml2468/octo-doc/internal/service"
	"github.com/lml2468/octo-doc/internal/storage"
)

// mutationLike mirrors service.MutationResult so both create/reply branches can
// be assigned to one variable.
type mutationLike service.MutationResult

// viewer resolves the current viewer (octo bridge first, then legacy cookie),
// or nil for anonymous. Never errors on "no session".
func (s *Server) viewer(r *http.Request) (*storage.Session, error) {
	return s.resolveViewerSession(r)
}

// parseVersionQuery parses the version query param: a number, "all", or latest.
func parseVersionQuery(raw string) int {
	if raw == "" {
		return core.VersionLatest
	}
	if raw == "all" {
		return core.VersionLatest
	}
	if n, ok := parseVersionParam(raw); ok {
		return n
	}
	return core.VersionLatest
}

func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(r.URL.Query().Get("slug"))
	if err != nil {
		return err
	}
	list, err := s.comments.List(r.Context(), slug, parseVersionQuery(r.URL.Query().Get("version")))
	if err != nil {
		return err
	}
	dtos := toCommentDTOs(list)
	// Comments are returned in full (no server-side paging today); report the
	// single-page offset pagination shape so the list envelope is R5-compliant.
	writeList(w, dtos, pagination{Total: len(dtos), Page: 1, PageSize: len(dtos)})
	return nil
}

func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) error {
	session, err := s.viewer(r)
	if err != nil {
		return err
	}
	var body struct {
		Slug     string       `json:"slug"`
		Text     string       `json:"text"`
		Version  json.Number  `json:"version"`
		ParentID *string      `json:"parent_id"`
		Anchor   *core.Anchor `json:"anchor"`
	}
	_ = decodeJSON(w, r, &body)
	slug, err := requireSlug(body.Slug)
	if err != nil {
		return err
	}
	// A comment requires at least a reader capability (the doc's share code) — or
	// the author write token. Default-private: no credential → 404.
	if err := s.requireDocCap(r, slug); err != nil {
		return err
	}
	if body.Text == "" {
		return apperr.Validation("slug and text required", "text_required")
	}
	version := numOr1(body.Version)
	var res mutationLike
	if body.ParentID != nil && *body.ParentID != "" {
		mr, merr := s.comments.Reply(r.Context(), slug, *body.ParentID, authorFromSession(session), body.Text, version)
		if merr != nil {
			return merr
		}
		res = mutationLike(mr)
	} else {
		mr, merr := s.comments.Create(r.Context(), slug, authorFromSession(session), body.Text, body.Anchor, version)
		if merr != nil {
			return merr
		}
		res = mutationLike(mr)
	}
	writeData(w, res.Status, mutationDTO(res.Body))
	return nil
}

func (s *Server) handlePatchComment(w http.ResponseWriter, r *http.Request) error {
	session, err := s.viewer(r)
	if err != nil {
		return err
	}
	var body struct {
		Slug    string       `json:"slug"`
		ID      string       `json:"id"`
		Anchor  *core.Anchor `json:"anchor"`
		Version json.Number  `json:"version"`
	}
	_ = decodeJSON(w, r, &body)
	slug, err := requireSlug(body.Slug)
	if err != nil {
		return err
	}
	if body.ID == "" || body.Anchor == nil {
		return apperr.Validation("slug, id, anchor required", "anchor_required")
	}
	if err := s.requireDocCap(r, slug); err != nil {
		return err
	}
	if err := s.authorizeMutation(r, slug, body.ID, session); err != nil {
		return err
	}
	actor := actorLogin(session)
	mr, err := s.comments.Reanchor(r.Context(), slug, body.ID, body.Anchor, numOr1(body.Version), actor)
	if err != nil {
		return err
	}
	writeData(w, mr.Status, mutationDTO(mr.Body))
	return nil
}

func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) error {
	slug, err := requireSlug(r.URL.Query().Get("slug"))
	if err != nil {
		return err
	}
	if r.URL.Query().Get("all") == "1" {
		return s.wipeComments(w, r, slug)
	}
	session, err := s.viewer(r)
	if err != nil {
		return err
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		return apperr.Validation("slug and id required", "id_required")
	}
	if err := s.requireDocCap(r, slug); err != nil {
		return err
	}
	if err := s.authorizeMutation(r, slug, id, session); err != nil {
		return err
	}
	version := parseVersionQuery(r.URL.Query().Get("version"))
	if version == core.VersionLatest {
		version = 999_999
	}
	mr, err := s.comments.Remove(r.Context(), slug, id, version, actorLogin(session))
	if err != nil {
		return err
	}
	writeData(w, mr.Status, mutationDTO(mr.Body))
	return nil
}

func (s *Server) handleReact(w http.ResponseWriter, r *http.Request) error {
	session, err := s.viewer(r)
	if err != nil {
		return err
	}
	var body struct {
		Slug      string      `json:"slug"`
		CommentID string      `json:"comment_id"`
		Emoji     string      `json:"emoji"`
		Version   json.Number `json:"version"`
	}
	_ = decodeJSON(w, r, &body)
	slug, err := requireSlug(body.Slug)
	if err != nil {
		return err
	}
	if body.CommentID == "" || body.Emoji == "" {
		return apperr.Validation("slug, comment_id, emoji required", "reaction_fields_required")
	}
	if len(body.Emoji) == 0 || len(body.Emoji) > 32 {
		return apperr.Validation("invalid emoji", "invalid_emoji")
	}
	if err := s.requireDocCap(r, slug); err != nil {
		return err
	}
	by := "anon"
	if session != nil {
		by = session.Login
	}
	mr, err := s.comments.React(r.Context(), slug, body.CommentID, body.Emoji, by, numOr1(body.Version))
	if err != nil {
		return err
	}
	writeData(w, mr.Status, mutationDTO(mr.Body))
	return nil
}

// wipeComments clears every comment on a doc — a high-risk author-only write.
// It reuses the standard author gate (requireDocAuthorSlug: creator uid match or
// octo superAdmin) rather than the retired write-token check, and returns 404 on
// anything less than author so the op stays consistent with hidden-existence.
func (s *Server) wipeComments(w http.ResponseWriter, r *http.Request, slug string) error {
	if err := s.requireDocAuthorSlug(r, slug); err != nil {
		return err
	}
	mr, err := s.comments.Wipe(r.Context(), slug)
	if err != nil {
		return err
	}
	writeData(w, mr.Status, mutationDTO(mr.Body))
	return nil
}

// authorizeMutation enforces author/owner permission on a comment mutation.
//
// In anonymous mode (no login provider, no session) comments are unowned, so
// there is nothing to enforce and the mutation is allowed — this matches the
// upstream "local mode is unauthenticated" behavior. Once a future login
// provider populates sessions, an authenticated viewer may only mutate their own
// comments (or anything, if they are the owner). The seam is ready: it activates
// the moment sessions start carrying a real identity.
func (s *Server) authorizeMutation(r *http.Request, slug, id string, session *storage.Session) error {
	if session == nil {
		return nil // anonymous: unowned comments, nothing to authorize against
	}
	list, err := s.comments.Read(r.Context(), slug)
	if err != nil {
		return err
	}
	author := findAuthorRecord(list, id)
	if author == nil {
		return apperr.Validation("not found", "not_found")
	}
	if s.auth.IsOwner(session) {
		return nil
	}
	if author.Login != "" && author.Login == session.Login {
		return nil
	}
	return apperr.Forbidden("not the author", "not_author")
}

// findAuthorRecord returns the author of a comment or reply id, or nil.
func findAuthorRecord(list []core.Comment, id string) *core.Author {
	comment, reply := core.FindHost(list, id)
	if comment == nil {
		return nil
	}
	if reply != nil {
		return reply.Author
	}
	return comment.Author
}

func actorLogin(session *storage.Session) string {
	if session != nil {
		return session.Login
	}
	return "local"
}

func numOr1(n json.Number) int {
	if n == "" {
		return 1
	}
	v, err := n.Int64()
	if err != nil || v == 0 {
		return 1
	}
	return int(v)
}

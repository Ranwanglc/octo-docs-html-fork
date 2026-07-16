package httpx

import (
	"github.com/lml2468/octo-doc/internal/core"
	"github.com/lml2468/octo-doc/internal/service"
)

// Transport DTOs for comment snapshots.
//
// core.CommentSnapshot uses the field name "created" and is byte-equivalence
// locked (its JSON shape is pinned by the internal/core tests — see CLAUDE.md /
// docs/PORTING.md). The OCTO API contract (R3) requires
// timestamp fields to carry the "_at" suffix. We satisfy R3 at the wire boundary
// by mapping the core snapshot into these DTOs, leaving core untouched.

// replyDTO is the wire shape of a reply (created → created_at per R3).
type replyDTO struct {
	ID          string            `json:"id"`
	ParentID    string            `json:"parent_id"`
	Author      *core.Author      `json:"author"`
	Text        string            `json:"text"`
	AgentStatus *core.AgentStatus `json:"agent_status"`
	CreatedAt   string            `json:"created_at"`
	Reactions   core.Reactions    `json:"reactions"`
	Deleted     bool              `json:"deleted"`
}

// commentDTO is the wire shape of a comment (created → created_at per R3).
type commentDTO struct {
	ID        string         `json:"id"`
	Author    *core.Author   `json:"author"`
	CreatedAt string         `json:"created_at"`
	CreatedIn int            `json:"created_in"`
	Version   int            `json:"version"`
	Anchor    *core.Anchor   `json:"anchor"`
	Text      string         `json:"text"`
	Status    string         `json:"status"`
	AppliedIn *int           `json:"applied_in,omitempty"`
	Replies   []replyDTO     `json:"replies"`
	Reactions core.Reactions `json:"reactions"`
	Deleted   bool           `json:"deleted"`
}

// toCommentDTO maps a core snapshot to its wire DTO.
func toCommentDTO(c core.CommentSnapshot) commentDTO {
	replies := make([]replyDTO, 0, len(c.Replies))
	for _, r := range c.Replies {
		replies = append(replies, replyDTO{
			ID: r.ID, ParentID: r.ParentID, Author: r.Author, Text: r.Text,
			AgentStatus: r.AgentStatus, CreatedAt: r.Created, Reactions: r.Reactions, Deleted: r.Deleted,
		})
	}
	return commentDTO{
		ID: c.ID, Author: c.Author, CreatedAt: c.Created, CreatedIn: c.CreatedIn,
		Version: c.Version, Anchor: c.Anchor, Text: c.Text, Status: c.Status,
		AppliedIn: c.AppliedIn, Replies: replies, Reactions: c.Reactions, Deleted: c.Deleted,
	}
}

// toCommentDTOs maps a snapshot list to wire DTOs (never nil).
func toCommentDTOs(list []core.CommentSnapshot) []commentDTO {
	out := make([]commentDTO, 0, len(list))
	for _, c := range list {
		out = append(out, toCommentDTO(c))
	}
	return out
}

// versionRefDTO is the wire shape of a version ref (created → created_at per R3).
// storage.VersionRef can't carry the "_at" suffix directly: its json tags are
// persisted to metadata storage (see storage/docmeta.go). We remap at the wire
// boundary instead.
type versionRefDTO struct {
	N         int     `json:"n"`
	CreatedAt *string `json:"created_at,omitempty"`
}

// versionListDTO is the wire shape of the versions response.
type versionListDTO struct {
	Slug     string          `json:"slug"`
	Title    string          `json:"title"`
	Versions []versionRefDTO `json:"versions"`
}

// docDetailDTO is single-doc metadata (GET /v1/docs/{slug}), aligned with the
// list item shape plus the version refs.
type docDetailDTO struct {
	Slug     string          `json:"slug"`
	Title    string          `json:"title"`
	Latest   int             `json:"latest"`
	Updated  *string         `json:"updated,omitempty"`
	Versions []versionRefDTO `json:"versions"`
}

// toVersionListDTO maps a service VersionList to its wire DTO.
func toVersionListDTO(vl *service.VersionList) versionListDTO {
	return versionListDTO{Slug: vl.Slug, Title: vl.Title, Versions: toVersionRefDTOs(vl)}
}

func toVersionRefDTOs(vl *service.VersionList) []versionRefDTO {
	refs := make([]versionRefDTO, 0, len(vl.Versions))
	for _, v := range vl.Versions {
		refs = append(refs, versionRefDTO{N: v.N, CreatedAt: v.Created})
	}
	return refs
}

// mutationDTO normalizes a service mutation Body to the wire contract. Body is
// always one of two core-defined (byte-equivalence-locked) shapes: a *core.CommentSnapshot
// for create/reply/reanchor, which we remap (created → created_at per R3), or a
// map[string]any for react/delete/wipe, whose keys are already compliant.
func mutationDTO(body any) any {
	switch v := body.(type) {
	case *core.CommentSnapshot:
		if v == nil {
			return body
		}
		return toCommentDTO(*v)
	default:
		return body
	}
}

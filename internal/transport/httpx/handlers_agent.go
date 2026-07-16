package httpx

import (
	"net/http"

	"github.com/lml2468/octo-doc/internal/core"
	"github.com/lml2468/octo-doc/internal/platform/apperr"
)

// handleAgentReply lets an agent post a reply + verdict (applied/partial/
// question) to a comment. Write-token gated.
func (s *Server) handleAgentReply(w http.ResponseWriter, r *http.Request) error {
	var body struct {
		Slug      string `json:"slug"`
		ParentID  string `json:"parent_id"`
		Text      string `json:"text"`
		Status    string `json:"status"`
		AppliedIn int    `json:"applied_in"`
	}
	_ = decodeJSON(w, r, &body)
	slug, err := requireSlug(body.Slug)
	if err != nil {
		return err
	}
	if err := s.requireDocAuthorSlug(r, slug); err != nil {
		return err
	}
	if body.ParentID == "" || body.Text == "" {
		return apperr.Validation("slug, parent_id, text required", "agent_reply_fields_required")
	}

	list, err := s.comments.Read(r.Context(), slug)
	if err != nil {
		return err
	}
	var parent *core.Comment
	for i := range list {
		if list[i].ID == body.ParentID {
			parent = &list[i]
			break
		}
	}
	if parent == nil {
		return apperr.Validation("parent not found", "parent_not_found")
	}

	verdict := normalizeVerdict(body.Status)
	version := body.AppliedIn
	if version == 0 {
		version = parent.CreatedIn
	}
	if version == 0 {
		version = 1
	}
	now := nowISO()
	replyID := "r_" + compactDigits(now) + "_" + randHex4()
	author := &core.Author{Kind: "agent", Login: "odoc-agent", Name: "odoc-agent"}

	events := agentReplyEvents(replyID, author, body.Text, verdict, version, now)
	respBody := map[string]any{
		"id": replyID, "parent_id": body.ParentID, "text": body.Text,
		"author": author, "agent_status": verdictOrNil(verdict), "created_at": now, "reactions": map[string]any{},
	}
	mr, err := s.comments.AppendRaw(r.Context(), slug, body.ParentID, events, respBody)
	if err != nil {
		return err
	}
	writeData(w, mr.Status, mr.Body)
	return nil
}

func normalizeVerdict(s string) core.AgentStatus {
	switch s {
	case "applied":
		return core.StatusApplied
	case "partial":
		return core.StatusPartial
	case "question":
		return core.StatusQuestion
	default:
		return ""
	}
}

func verdictOrNil(v core.AgentStatus) any {
	if v == "" {
		return nil
	}
	return v
}

// agentReplyEvents builds the event list for an agent reply (+ optional verdict
// state change).
func agentReplyEvents(replyID string, author *core.Author, text string, verdict core.AgentStatus, version int, now string) []core.CommentEvent {
	events := []core.CommentEvent{{
		Kind: "reply_added", AtVersion: version, At: now,
		Reply: &core.ReplySeed{ID: replyID, Author: author, Text: text, AgentStatus: verdict},
	}}
	switch verdict {
	case core.StatusApplied:
		ai := version
		events = append(events, core.CommentEvent{
			Kind: "marked_applied", AtVersion: version, At: now,
			AppliedIn: &ai, By: "odoc-agent", AgentStatus: core.StatusApplied,
		})
	case core.StatusPartial, core.StatusQuestion:
		events = append(events, core.CommentEvent{
			Kind: "marked_open", AtVersion: version, At: now,
			By: "odoc-agent", AgentStatus: verdict,
		})
	}
	return events
}

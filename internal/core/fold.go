package core

import (
	"math"
	"sort"
)

// Folding a comment's event log into a point-in-time CommentSnapshot. Ported from
// comment-fold.ts. The rule: reading "as of version N" replays events with
// at_version <= N. Events are deduped (convergence) and stable-sorted by
// at_version so the fold is independent of physical write order.

// agentStatusEmoji maps an agent verdict to the emoji rendered synthetically on
// the parent at fold time.
var agentStatusEmoji = map[AgentStatus]string{
	StatusApplied:  "✅",
	StatusPartial:  "🟡",
	StatusQuestion: "❓",
}

// VersionLatest folds to the newest state (JS Infinity).
const VersionLatest = math.MaxInt

func isFiniteVersion(v int) bool {
	return v >= 0 && v != VersionLatest
}

type foldState struct {
	snap         *CommentSnapshot
	replyOrder   []string
	replyByID    map[string]*ReplySnapshot
	agentVerdict *AgentStatus
}

// applyReaction toggles a login into/out of a reactions map for one emoji.
func applyReaction(reactions Reactions, emoji, by string, add bool) {
	users := reactions[emoji]
	idx := indexOf(users, by)
	if add {
		if idx < 0 {
			users = append(users, by)
		}
		reactions[emoji] = users
	} else {
		if idx >= 0 {
			users = append(users[:idx], users[idx+1:]...)
		}
		if len(users) > 0 {
			reactions[emoji] = users
		} else {
			delete(reactions, emoji)
		}
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func applyContentEvent(st *foldState, e *CommentEvent) bool {
	snap := st.snap
	switch e.Kind {
	case "created":
		snap.Anchor = e.Anchor
		snap.Text = e.Text
		return true
	case "text_edited":
		snap.Text = e.Text
		return true
	case "anchor_changed":
		snap.Anchor = e.Anchor
		if e.ResetStatus != nil && *e.ResetStatus {
			snap.Status = "open"
			snap.AppliedIn = nil
		}
		return true
	default:
		return false
	}
}

func applyStatusEvent(st *foldState, e *CommentEvent) bool {
	snap := st.snap
	switch e.Kind {
	case "marked_applied":
		snap.Status = "applied"
		if e.AppliedIn != nil {
			snap.AppliedIn = e.AppliedIn
		} else {
			v := e.AtVersion
			snap.AppliedIn = &v
		}
		if e.AgentStatus != "" {
			vs := e.AgentStatus
			st.agentVerdict = &vs
		} else {
			vs := StatusApplied
			st.agentVerdict = &vs
		}
		return true
	case "marked_open":
		snap.Status = "open"
		snap.AppliedIn = nil
		if e.AgentStatus != "" {
			vs := e.AgentStatus
			st.agentVerdict = &vs
		} else {
			st.agentVerdict = nil
		}
		return true
	case "deleted":
		snap.Deleted = true
		return true
	default:
		return false
	}
}

func applyParentReaction(st *foldState, e *CommentEvent) bool {
	if e.Kind == "reaction_added" && e.Emoji != "" && e.By != "" {
		applyReaction(st.snap.Reactions, e.Emoji, e.By, true)
		return true
	}
	if e.Kind == "reaction_removed" && e.Emoji != "" && e.By != "" {
		applyReaction(st.snap.Reactions, e.Emoji, e.By, false)
		return true
	}
	return false
}

func addReply(st *foldState, e *CommentEvent) {
	if e.Reply == nil || e.Reply.ID == "" {
		return
	}
	var status *AgentStatus
	if e.Reply.AgentStatus != "" {
		s := e.Reply.AgentStatus
		status = &s
	}
	st.replyOrder = append(st.replyOrder, e.Reply.ID)
	st.replyByID[e.Reply.ID] = &ReplySnapshot{
		ID:          e.Reply.ID,
		ParentID:    st.snap.ID,
		Author:      e.Reply.Author,
		Text:        e.Reply.Text,
		AgentStatus: status,
		Created:     e.At,
		Reactions:   Reactions{},
		Deleted:     false,
	}
}

func applyReplyReaction(st *foldState, e *CommentEvent) {
	r := st.replyByID[e.ReplyID]
	if r != nil && e.Emoji != "" && e.By != "" {
		applyReaction(r.Reactions, e.Emoji, e.By, e.Kind == "reply_reaction_added")
	}
}

func applyReplyEvent(st *foldState, e *CommentEvent) {
	switch e.Kind {
	case "reply_added":
		addReply(st, e)
	case "reply_text_edited":
		if r := st.replyByID[e.ReplyID]; r != nil {
			r.Text = e.Text
		}
	case "reply_deleted":
		if r := st.replyByID[e.ReplyID]; r != nil {
			r.Deleted = true
		}
	case "reply_reaction_added", "reply_reaction_removed":
		applyReplyReaction(st, e)
	}
}

func applyCommentEvent(st *foldState, e *CommentEvent) {
	if applyContentEvent(st, e) {
		return
	}
	if applyStatusEvent(st, e) {
		return
	}
	if applyParentReaction(st, e) {
		return
	}
	applyReplyEvent(st, e)
}

// orderedEvents dedups then stable-sorts by at_version. Go's sort.SliceStable
// matches JS's stable Array.prototype.sort for equal keys.
func orderedEvents(events []CommentEvent) []CommentEvent {
	out := DedupEvents(events)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].AtVersion < out[j].AtVersion
	})
	return out
}

func emptySnapshot(c *Comment) *CommentSnapshot {
	return &CommentSnapshot{
		ID:        c.ID,
		Author:    c.Author,
		Created:   c.Created,
		CreatedIn: c.CreatedIn,
		Version:   c.CreatedIn,
		Anchor:    nil,
		Text:      "",
		Status:    "open",
		AppliedIn: nil,
		Replies:   []ReplySnapshot{},
		Reactions: Reactions{},
		Deleted:   false,
	}
}

func replay(c *Comment, at int) *foldState {
	st := &foldState{
		snap:       emptySnapshot(c),
		replyOrder: []string{},
		replyByID:  map[string]*ReplySnapshot{},
	}
	for _, e := range orderedEvents(c.Events) {
		ev := e
		if isFiniteVersion(ev.AtVersion) && ev.AtVersion <= at {
			applyCommentEvent(st, &ev)
		}
	}
	return st
}

func finalize(st *foldState) *CommentSnapshot {
	if st.agentVerdict != nil {
		if emoji := agentStatusEmoji[*st.agentVerdict]; emoji != "" {
			applyReaction(st.snap.Reactions, emoji, "odoc-agent", true)
		}
	}
	replies := []ReplySnapshot{}
	for _, id := range st.replyOrder {
		if r := st.replyByID[id]; r != nil && !r.Deleted {
			replies = append(replies, *r)
		}
	}
	st.snap.Replies = replies
	return st.snap
}

// SnapshotAt folds a comment into its snapshot as of version v (VersionLatest for
// the newest state). Returns nil if the comment did not exist at v.
func SnapshotAt(c *Comment, v int) *CommentSnapshot {
	EnsureEventLog(c)
	if len(c.Events) == 0 {
		return nil
	}
	at := v
	if !isFiniteVersion(v) {
		at = VersionLatest
	}
	if c.CreatedIn > at {
		return nil
	}
	return finalize(replay(c, at))
}

// SnapshotList folds a list at version v, returning only alive (non-deleted)
// snapshots.
func SnapshotList(list []Comment, v int) []CommentSnapshot {
	out := []CommentSnapshot{}
	for i := range list {
		s := SnapshotAt(&list[i], v)
		if s != nil && !s.Deleted {
			out = append(out, *s)
		}
	}
	return out
}

// HistoryList folds every comment across all versions at its richest state.
func HistoryList(list []Comment) []CommentSnapshot {
	return SnapshotList(list, VersionLatest)
}

package core

import (
	"encoding/json"
	"sort"
	"strconv"
	"sync/atomic"
	"time"
)

// Comment event-log primitives: stable event ids, dedup/convergence, and lazy
// migration of legacy flat comments into the event-log shape. Ported from
// comment-events.ts.

// eidCounter is a monotonic counter for one-shot event ids. The upstream Worker
// used Math.random(); a counter serves the same uniqueness role without a PRNG.
// Idempotent events keep deterministic ids, so the dedup contract is unchanged.
var eidCounter atomic.Uint64

// EventEID computes a stable event id. Naturally-idempotent events (reactions,
// state flags) get a deterministic id so a concurrent duplicate collapses to one;
// one-shot events (created, reply, edits) get a unique id.
func EventEID(e *CommentEvent) string {
	switch e.Kind {
	case "reaction_added", "reaction_removed":
		return e.Kind + ":" + e.Emoji + ":" + e.By
	case "marked_applied", "marked_open", "deleted":
		return e.Kind + ":" + strconv.Itoa(e.AtVersion)
	default:
		nonce := strconv.FormatUint(eidCounter.Add(1)-1, 36)
		hi := strconv.FormatInt(time.Now().UnixMicro(), 36)
		return e.Kind + ":" + e.At + ":" + nonce + "_" + hi
	}
}

// BackfillEIDs stamps a missing eid on each event. Returns true if anything changed.
func BackfillEIDs(events []CommentEvent) bool {
	changed := false
	for i := range events {
		if events[i].EID == "" {
			events[i].EID = EventEID(&events[i])
			changed = true
		}
	}
	return changed
}

// AppendEvent appends an event to a comment, stamping an eid if absent.
func AppendEvent(c *Comment, event CommentEvent) {
	if event.EID == "" {
		event.EID = EventEID(&event)
	}
	c.Events = append(c.Events, event)
}

// DedupEvents collapses events sharing an eid, keeping the last occurrence. This
// is the convergence point: merging two concurrently-written logs and folding
// through DedupEvents yields the same result regardless of write order.
func DedupEvents(events []CommentEvent) []CommentEvent {
	lastByEID := make(map[string]CommentEvent, len(events))
	for _, e := range events {
		if e.EID != "" {
			lastByEID[e.EID] = e
		}
	}
	out := make([]CommentEvent, 0, len(events))
	emitted := make(map[string]struct{}, len(events))
	for _, e := range events {
		if e.EID == "" {
			out = append(out, e)
			continue
		}
		if _, ok := emitted[e.EID]; ok {
			continue
		}
		emitted[e.EID] = struct{}{}
		out = append(out, lastByEID[e.EID])
	}
	return out
}

// EnsureEventLog ensures a comment has an Events log, migrating a legacy flat
// record in place if needed. Returns true if the record was migrated or had eids
// backfilled.
func EnsureEventLog(c *Comment) bool {
	if c.Events != nil {
		return BackfillEIDs(c.Events)
	}
	if c.ID == "" {
		return false
	}
	events := legacyToEvents(c)
	BackfillEIDs(events)
	c.Events = events
	if len(events) > 0 {
		c.CreatedIn = events[0].AtVersion
	} else if c.Version != nil {
		c.CreatedIn = *c.Version
	} else {
		c.CreatedIn = 1
	}
	if c.Created == "" {
		if len(events) > 0 {
			c.Created = events[0].At
		} else {
			c.Created = nowISO()
		}
	}
	return true
}

// EnsureMigrated migrates every comment in a list to the event-log shape.
// Returns true if any changed.
func EnsureMigrated(list []Comment) bool {
	dirty := false
	for i := range list {
		if EnsureEventLog(&list[i]) {
			dirty = true
		}
	}
	return dirty
}

// CompactComments permanently collapses each comment's log to its deduped form.
// Called at publish time so stored values stop growing unboundedly.
func CompactComments(comments []Comment) bool {
	changed := false
	for i := range comments {
		c := &comments[i]
		if c.Events == nil {
			continue
		}
		BackfillEIDs(c.Events)
		compacted := DedupEvents(c.Events)
		if len(compacted) != len(c.Events) {
			c.Events = compacted
			changed = true
		}
	}
	return changed
}

// legacyToEvents builds a fresh created-and-friends event list from a legacy flat
// comment.
func legacyToEvents(c *Comment) []CommentEvent {
	events := []CommentEvent{}
	at := c.Created
	if at == "" {
		at = nowISO()
	}
	v := 1
	if c.Version != nil {
		v = *c.Version
	}
	var anchor *Anchor
	if c.Anchor != nil {
		anchor = c.Anchor
	}
	text := ""
	if c.Text != nil {
		text = *c.Text
	}
	events = append(events, CommentEvent{Kind: "created", AtVersion: v, At: at, Anchor: anchor, Text: text})

	if c.Status == "applied" {
		appliedIn := v
		if c.AppliedIn != nil {
			appliedIn = *c.AppliedIn
		}
		ai := appliedIn
		events = append(events, CommentEvent{
			Kind: "marked_applied", AtVersion: appliedIn, At: at,
			AppliedIn: &ai, By: "odoc-agent", AgentStatus: StatusApplied,
		})
	}
	events = appendLegacyReactions(events, c.Reactions, v, at)
	events = appendLegacyReplies(events, c.Replies, v, at)
	return events
}

func appendLegacyReactions(events []CommentEvent, reactions map[string][]string, v int, at string) []CommentEvent {
	for _, ev := range legacyReactionEvents(reactions, v, at) {
		ev.Kind = "reaction_added"
		events = append(events, ev)
	}
	return events
}

type rawReply struct {
	ID          string              `json:"id"`
	Author      *Author             `json:"author"`
	Text        string              `json:"text"`
	AgentStatus AgentStatus         `json:"agent_status"`
	Version     *int                `json:"version"`
	Created     string              `json:"created"`
	Reactions   map[string][]string `json:"reactions"`
}

func appendLegacyReplies(events []CommentEvent, replies []json.RawMessage, v int, at string) []CommentEvent {
	for _, raw := range replies {
		var r rawReply
		if err := json.Unmarshal(raw, &r); err != nil || r.ID == "" {
			continue
		}
		events = appendLegacyReply(events, r, v, at)
	}
	return events
}

func appendLegacyReply(events []CommentEvent, r rawReply, v int, at string) []CommentEvent {
	rv := v
	if r.Version != nil {
		rv = *r.Version
	}
	when := r.Created
	if when == "" {
		when = at
	}
	events = append(events, CommentEvent{
		Kind: "reply_added", AtVersion: rv, At: when,
		Reply: &ReplySeed{ID: r.ID, Author: r.Author, Text: r.Text, AgentStatus: r.AgentStatus},
	})
	for _, ev := range legacyReactionEvents(r.Reactions, rv, when) {
		ev.Kind = "reply_reaction_added"
		ev.ReplyID = r.ID
		events = append(events, ev)
	}
	return events
}

func legacyReactionEvents(reactions map[string][]string, v int, at string) []CommentEvent {
	out := []CommentEvent{}
	// Iterate keys in sorted order so migration is deterministic (Go map order
	// is randomized; the fold dedups but stored order should be stable).
	for _, emoji := range sortedKeys(reactions) {
		for _, by := range reactions[emoji] {
			out = append(out, CommentEvent{AtVersion: v, At: at, Emoji: emoji, By: by})
		}
	}
	return out
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

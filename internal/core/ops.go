package core

import "time"

// Comment mutation operations, ported from ops.ts. Each op is applied to an
// in-memory list, returning an OpResult. This is the single place comment
// mutation logic lives; the service layer serializes calls per slug.

// OpResult is the HTTP-shaped result of applying an op.
type OpResult struct {
	Status int
	Body   any
	// Wipe signals the caller to delete the slug's comment key entirely.
	Wipe bool
}

// CommentOp describes one comment operation. Kind selects the variant; only the
// fields relevant to that kind are read.
type CommentOp struct {
	Kind string
	At   string // optional fixed timestamp (tests); defaults to now

	// create
	ID     string
	Author *Author
	Text   string
	Anchor *Anchor

	// reply
	ParentID string
	ReplyID  string

	// patch_anchor
	ResetStatus bool
	Actor       string

	// react
	CommentID string
	Emoji     string
	By        string

	// raw_events
	Events       []CommentEvent
	ResponseBody any

	// publish_merge
	LocalComments []Comment
	AIDs          []StampedArtifact

	// shared
	Version int
}

// FindHost locates a comment or reply by id, returning the host comment and, when
// the id names a reply, that reply's seed.
func FindHost(list []Comment, id string) (comment *Comment, reply *ReplySeed) {
	for i := range list {
		if list[i].ID == id {
			return &list[i], nil
		}
	}
	for i := range list {
		for j := range list[i].Events {
			e := &list[i].Events[j]
			if e.Kind == "reply_added" && e.Reply != nil && e.Reply.ID == id {
				return &list[i], e.Reply
			}
		}
	}
	return nil, nil
}

func reactionsForUser(reactions Reactions, by, emoji string) bool {
	return indexOf(reactions[emoji], by) >= 0
}

// ApplyCommentOp applies one operation to list (mutating it) and returns the
// result. list is migrated in place. Returns the possibly-reallocated list (Go
// slices may grow on append) alongside the result.
func ApplyCommentOp(list []Comment, op CommentOp) ([]Comment, OpResult) {
	EnsureMigrated(list)
	now := op.At
	if now == "" {
		now = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	switch op.Kind {
	case "create":
		return opCreate(list, op, now)
	case "reply":
		return list, opReply(list, op, now)
	case "patch_anchor":
		return list, opPatchAnchor(list, op, now)
	case "react":
		return list, opReact(list, op, now)
	case "delete":
		return list, opDelete(list, op, now)
	case "raw_events":
		return list, opRawEvents(list, op)
	case "wipe":
		return list, OpResult{Status: 200, Body: map[string]any{"ok": true, "deleted": len(list)}, Wipe: true}
	case "publish_merge":
		return opPublishMerge(list, op)
	}
	return list, OpResult{Status: 400, Body: map[string]any{"error": "unknown_op"}}
}

func opCreate(list []Comment, op CommentOp, now string) ([]Comment, OpResult) {
	entry := Comment{
		ID:        op.ID,
		Author:    op.Author,
		Created:   now,
		CreatedIn: op.Version,
		Events: []CommentEvent{{
			Kind: "created", AtVersion: op.Version, At: now, Anchor: op.Anchor, Text: op.Text,
		}},
	}
	BackfillEIDs(entry.Events)
	list = append(list, entry)
	return list, OpResult{Status: 200, Body: SnapshotAt(&list[len(list)-1], op.Version)}
}

func opReply(list []Comment, op CommentOp, now string) OpResult {
	parent := findByID(list, op.ParentID)
	if parent == nil {
		return OpResult{Status: 404, Body: map[string]any{"error": "parent_not_found"}}
	}
	AppendEvent(parent, CommentEvent{
		Kind: "reply_added", AtVersion: op.Version, At: now,
		Reply: &ReplySeed{ID: op.ReplyID, Author: op.Author, Text: op.Text},
	})
	return OpResult{Status: 200, Body: map[string]any{
		"id": op.ReplyID, "parent_id": op.ParentID, "author": op.Author,
		"text": op.Text, "created": now, "version": op.Version,
	}}
}

func opPatchAnchor(list []Comment, op CommentOp, now string) OpResult {
	target := findByID(list, op.ID)
	if target == nil {
		return OpResult{Status: 404, Body: map[string]any{"error": "not_found"}}
	}
	rs := op.ResetStatus
	AppendEvent(target, CommentEvent{
		Kind: "anchor_changed", AtVersion: op.Version, At: now,
		ResetStatus: &rs, Anchor: op.Anchor, By: op.Actor,
	})
	return OpResult{Status: 200, Body: SnapshotAt(target, op.Version)}
}

func reactionsOf(snap *CommentSnapshot, replyID string) Reactions {
	if replyID == "" {
		return snap.Reactions
	}
	for i := range snap.Replies {
		if snap.Replies[i].ID == replyID {
			return snap.Replies[i].Reactions
		}
	}
	return Reactions{}
}

func opReact(list []Comment, op CommentOp, now string) OpResult {
	host, reply := FindHost(list, op.CommentID)
	if host == nil {
		return OpResult{Status: 404, Body: map[string]any{"error": "not_found"}}
	}
	replyID := ""
	if reply != nil {
		replyID = op.CommentID
	}
	snap := SnapshotAt(host, op.Version)
	if snap == nil {
		return OpResult{Status: 404, Body: map[string]any{"error": "not_visible_at_version"}}
	}
	had := reactionsForUser(reactionsOf(snap, replyID), op.By, op.Emoji)
	ev := CommentEvent{AtVersion: op.Version, At: now, Emoji: op.Emoji, By: op.By}
	if replyID != "" {
		ev.ReplyID = replyID
		if had {
			ev.Kind = "reply_reaction_removed"
		} else {
			ev.Kind = "reply_reaction_added"
		}
	} else if had {
		ev.Kind = "reaction_removed"
	} else {
		ev.Kind = "reaction_added"
	}
	AppendEvent(host, ev)
	fresh := SnapshotAt(host, op.Version)
	return OpResult{Status: 200, Body: map[string]any{"ok": true, "reactions": reactionsOf(fresh, replyID)}}
}

func opDelete(list []Comment, op CommentOp, now string) OpResult {
	host, reply := FindHost(list, op.ID)
	if host == nil {
		return OpResult{Status: 404, Body: map[string]any{"error": "not_found"}}
	}
	if reply != nil {
		AppendEvent(host, CommentEvent{
			Kind: "reply_deleted", AtVersion: op.Version, At: now, ReplyID: op.ID, By: op.Actor,
		})
	} else {
		AppendEvent(host, CommentEvent{
			Kind: "deleted", AtVersion: op.Version, At: now, By: op.Actor,
		})
	}
	return OpResult{Status: 200, Body: map[string]any{"ok": true}}
}

func opRawEvents(list []Comment, op CommentOp) OpResult {
	target := findByID(list, op.ID)
	if target == nil {
		return OpResult{Status: 404, Body: map[string]any{"error": "not_found"}}
	}
	for _, ev := range op.Events {
		AppendEvent(target, ev)
	}
	if op.ResponseBody != nil {
		return OpResult{Status: 200, Body: op.ResponseBody}
	}
	return OpResult{Status: 200, Body: map[string]any{"ok": true}}
}

func opPublishMerge(list []Comment, op CommentOp) ([]Comment, OpResult) {
	merged := 0
	if len(op.LocalComments) > 0 {
		have := map[string]struct{}{}
		for i := range list {
			if list[i].ID != "" {
				have[list[i].ID] = struct{}{}
			}
		}
		for i := range op.LocalComments {
			lc := op.LocalComments[i]
			if lc.ID == "" {
				continue
			}
			if _, ok := have[lc.ID]; ok {
				continue
			}
			EnsureEventLog(&lc)
			list = append(list, lc)
			have[lc.ID] = struct{}{}
			merged++
		}
	}
	if len(list) > 0 {
		ReconcileAnchors(list, op.AIDs, op.Version)
		CompactComments(list)
	}
	return list, OpResult{Status: 200, Body: map[string]any{"mergedComments": merged}}
}

func findByID(list []Comment, id string) *Comment {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

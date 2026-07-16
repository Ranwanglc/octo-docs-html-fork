package core

import "testing"

// These tests drive the event-log fold through the public op path (ApplyCommentOp
// → SnapshotList/HistoryList), the same way the service layer does, and assert the
// folded snapshot's observable fields. They replace the former golden fixtures.

// mkComment creates a top-level comment via the op path and returns the list.
func mkComment(t *testing.T, list []Comment, id, login, text, anchorText string, version int) []Comment {
	t.Helper()
	out, res := ApplyCommentOp(list, CommentOp{
		Kind: "create", ID: id, At: "2026-01-01T00:00:00Z", Version: version,
		Author: &Author{Login: login}, Text: text,
		Anchor: &Anchor{Kind: "text", Text: anchorText},
	})
	if res.Status != 200 {
		t.Fatalf("create %s: status %d", id, res.Status)
	}
	return out
}

func TestFoldCreatedComment(t *testing.T) {
	list := mkComment(t, nil, "c1", "alice", "first", "hello", 1)
	snap := SnapshotList(list, 1)
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	c := snap[0]
	if c.ID != "c1" || c.Text != "first" || c.Status != "open" || c.CreatedIn != 1 {
		t.Errorf("folded comment = %+v", c)
	}
	if c.Author == nil || c.Author.Login != "alice" {
		t.Errorf("author = %+v", c.Author)
	}
	if c.Anchor == nil || c.Anchor.Kind != "text" || c.Anchor.Text != "hello" {
		t.Errorf("anchor = %+v", c.Anchor)
	}
	if len(c.Replies) != 0 || len(c.Reactions) != 0 || c.Deleted {
		t.Errorf("expected no replies/reactions/deleted: %+v", c)
	}
}

func TestFoldReplyAndReaction(t *testing.T) {
	list := mkComment(t, nil, "c1", "alice", "first", "hello", 1)
	var res OpResult
	list, res = ApplyCommentOp(list, CommentOp{
		Kind: "reply", ParentID: "c1", ReplyID: "r1", At: "2026-01-01T00:01:00Z",
		Version: 1, Author: &Author{Login: "bob"}, Text: "reply-text",
	})
	if res.Status != 200 {
		t.Fatalf("reply status %d", res.Status)
	}
	list, res = ApplyCommentOp(list, CommentOp{
		Kind: "react", CommentID: "c1", Emoji: "👍", By: "carol",
		At: "2026-01-01T00:02:00Z", Version: 1,
	})
	if res.Status != 200 {
		t.Fatalf("react status %d", res.Status)
	}

	c := SnapshotList(list, 1)[0]
	if len(c.Replies) != 1 {
		t.Fatalf("replies = %d, want 1", len(c.Replies))
	}
	if r := c.Replies[0]; r.ID != "r1" || r.Text != "reply-text" || r.Author.Login != "bob" || r.Deleted {
		t.Errorf("reply = %+v", r)
	}
	if got := c.Reactions["👍"]; len(got) != 1 || got[0] != "carol" {
		t.Errorf("reactions = %+v, want 👍:[carol]", c.Reactions)
	}
}

// A reaction toggles off when the same login reacts again with the same emoji.
func TestFoldReactionToggleOff(t *testing.T) {
	list := mkComment(t, nil, "c1", "alice", "x", "y", 1)
	react := CommentOp{Kind: "react", CommentID: "c1", Emoji: "👍", By: "carol", Version: 1}
	react.At = "2026-01-01T00:01:00Z"
	list, _ = ApplyCommentOp(list, react)
	react.At = "2026-01-01T00:02:00Z"
	list, _ = ApplyCommentOp(list, react) // second identical react → toggle off

	c := SnapshotList(list, 1)[0]
	if got, ok := c.Reactions["👍"]; ok && len(got) != 0 {
		t.Errorf("reaction not toggled off: %+v", c.Reactions)
	}
}

// A deleted comment is excluded from the version snapshot.
func TestFoldDeletedExcluded(t *testing.T) {
	list := mkComment(t, nil, "c1", "alice", "x", "y", 1)
	list, res := ApplyCommentOp(list, CommentOp{
		Kind: "delete", ID: "c1", At: "2026-01-01T00:03:00Z", Version: 1, Actor: "alice",
	})
	if res.Status != 200 {
		t.Fatalf("delete status %d", res.Status)
	}
	if snap := SnapshotList(list, 1); len(snap) != 0 {
		t.Errorf("deleted comment still in snapshot: %+v", snap)
	}
	// A soft-deleted comment is excluded from the history view as well.
	if hist := HistoryList(list); len(hist) != 0 {
		t.Errorf("deleted comment still in history: %+v", hist)
	}
}

// A comment created in a later version is not visible when folding an earlier one.
func TestFoldVersionWindowing(t *testing.T) {
	list := mkComment(t, nil, "c1", "alice", "v1 comment", "a", 1)
	list = mkComment(t, list, "c2", "bob", "v2 comment", "b", 2)

	if got := SnapshotList(list, 1); len(got) != 1 || got[0].ID != "c1" {
		t.Errorf("fold@v1 = %+v, want only c1", got)
	}
	if got := SnapshotList(list, 2); len(got) != 2 {
		t.Errorf("fold@v2 = %d comments, want 2", len(got))
	}
}

// A legacy comment stored with flat pre-event-log fields (Version/Text/Anchor/
// Reactions, no Events) is migrated lazily on read and folds correctly.
func TestFoldLegacyMigration(t *testing.T) {
	txt := "legacy body"
	ver := 1
	legacy := []Comment{{
		ID: "L1", Author: &Author{Login: "a"}, Created: "t0",
		Version: &ver, Text: &txt, Status: "open",
		Anchor:    &Anchor{Kind: "text", Text: "h"},
		Reactions: map[string][]string{"👍": {"bob"}},
	}}
	if dirty := EnsureMigrated(legacy); !dirty {
		t.Fatal("EnsureMigrated reported no change for a legacy comment")
	}
	if len(legacy[0].Events) == 0 {
		t.Fatal("migration produced no events")
	}
	c := SnapshotList(legacy, 1)[0]
	if c.Text != "legacy body" || c.Status != "open" || c.Anchor.Text != "h" {
		t.Errorf("migrated fold = %+v", c)
	}
	if got := c.Reactions["👍"]; len(got) != 1 || got[0] != "bob" {
		t.Errorf("migrated reactions = %+v", c.Reactions)
	}
}

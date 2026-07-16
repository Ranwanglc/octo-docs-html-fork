package core

import "testing"

// These tests pin ApplyCommentOp's status codes and mutation behavior for each op
// kind, including error paths. They replace the former golden fixtures.

func seedComment(t *testing.T) []Comment {
	t.Helper()
	list, res := ApplyCommentOp(nil, CommentOp{
		Kind: "create", ID: "c1", At: "t0", Version: 1,
		Author: &Author{Login: "a"}, Text: "orig", Anchor: &Anchor{Kind: "text", Text: "h"},
	})
	if res.Status != 200 {
		t.Fatalf("seed create status %d", res.Status)
	}
	return list
}

func TestOpCreateReplyReactStatuses(t *testing.T) {
	list := seedComment(t)
	list, res := ApplyCommentOp(list, CommentOp{
		Kind: "reply", ParentID: "c1", ReplyID: "r1", At: "t1", Version: 1,
		Author: &Author{Login: "b"}, Text: "re",
	})
	if res.Status != 200 {
		t.Fatalf("reply status %d", res.Status)
	}
	_, res = ApplyCommentOp(list, CommentOp{
		Kind: "react", CommentID: "c1", Emoji: "👍", By: "c", At: "t2", Version: 1,
	})
	if res.Status != 200 {
		t.Errorf("react status %d", res.Status)
	}
}

func TestOpReplyToMissingParentIs404(t *testing.T) {
	_, res := ApplyCommentOp(seedComment(t), CommentOp{
		Kind: "reply", ParentID: "nope", ReplyID: "r9", At: "t1", Version: 1,
		Author: &Author{Login: "b"}, Text: "x",
	})
	if res.Status != 404 {
		t.Fatalf("status = %d, want 404", res.Status)
	}
	if body, _ := res.Body.(map[string]any); body["error"] != "parent_not_found" {
		t.Errorf("body = %+v, want error=parent_not_found", res.Body)
	}
}

func TestOpUnknownKindIs400(t *testing.T) {
	_, res := ApplyCommentOp(seedComment(t), CommentOp{Kind: "bogus", At: "t9"})
	if res.Status != 400 {
		t.Fatalf("status = %d, want 400", res.Status)
	}
	if body, _ := res.Body.(map[string]any); body["error"] != "unknown_op" {
		t.Errorf("body = %+v, want error=unknown_op", res.Body)
	}
}

func TestOpWipeSignalsWipe(t *testing.T) {
	_, res := ApplyCommentOp(seedComment(t), CommentOp{Kind: "wipe", At: "t9"})
	if res.Status != 200 || !res.Wipe {
		t.Errorf("wipe: status=%d wipe=%v, want 200/true", res.Status, res.Wipe)
	}
}

// patch_anchor replaces the anchor (and can reset an agent verdict). The folded
// snapshot reflects the new anchor.
func TestOpPatchAnchor(t *testing.T) {
	list := seedComment(t)
	list, res := ApplyCommentOp(list, CommentOp{
		Kind: "patch_anchor", ID: "c1", At: "t2", Version: 1, Actor: "a",
		ResetStatus: true, Anchor: &Anchor{Kind: "element", AID: "newaid"},
	})
	if res.Status != 200 {
		t.Fatalf("patch_anchor status %d", res.Status)
	}
	c := SnapshotList(list, 1)[0]
	if c.Anchor == nil || c.Anchor.Kind != "element" || c.Anchor.AID != "newaid" {
		t.Errorf("anchor after patch = %+v", c.Anchor)
	}
}

// An agent reply is attributed to the agent identity in the folded reply.
func TestOpAgentReply(t *testing.T) {
	list := seedComment(t)
	list, res := ApplyCommentOp(list, CommentOp{
		Kind: "reply", ParentID: "c1", ReplyID: "r1", At: "t1", Version: 1,
		Author: &Author{Login: "agent", Kind: "agent"}, Text: "done",
	})
	if res.Status != 200 {
		t.Fatalf("agent reply status %d", res.Status)
	}
	r := SnapshotList(list, 1)[0].Replies[0]
	if r.Author == nil || r.Author.Kind != "agent" || r.Text != "done" {
		t.Errorf("agent reply = %+v", r)
	}
}

// publish_merge folds a set of locally-authored comments into the stored log and
// reconciles their element anchors against the freshly-stamped artifacts.
func TestOpPublishMerge(t *testing.T) {
	local := elementComment(t, "m1", "aidX")
	merged, res := ApplyCommentOp(nil, CommentOp{
		Kind: "publish_merge", At: "t1", Version: 1,
		LocalComments: local,
		AIDs:          []StampedArtifact{{AID: "aidX", Tag: "section"}},
	})
	if res.Status != 200 {
		t.Fatalf("publish_merge status %d", res.Status)
	}
	snap := SnapshotList(merged, 1)
	if len(snap) != 1 || snap[0].ID != "m1" {
		t.Fatalf("merged snapshot = %+v, want the one local comment", snap)
	}
	if a := snap[0].Anchor; a == nil || a.AID != "aidX" {
		t.Errorf("merged anchor lost its aid: %+v", a)
	}
}

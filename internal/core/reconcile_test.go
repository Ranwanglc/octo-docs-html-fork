package core

import "testing"

// ReconcileAnchors re-binds element anchors after a re-stamp: an anchor whose aid
// is still present is left alone; one whose aid vanished is rebound to a surviving
// candidate (or marked lost when it can't be resolved). These replace the former
// reconcile golden fixtures.

func elementComment(t *testing.T, id, aid string) []Comment {
	t.Helper()
	list, res := ApplyCommentOp(nil, CommentOp{
		Kind: "create", ID: id, At: "t0", Version: 1,
		Author: &Author{Login: "a"}, Text: "x", Anchor: &Anchor{Kind: "element", AID: aid},
	})
	if res.Status != 200 {
		t.Fatalf("create %s status %d", id, res.Status)
	}
	return list
}

func TestReconcileKeepsPresentAid(t *testing.T) {
	list := elementComment(t, "e1", "stays")
	ReconcileAnchors(list, []StampedArtifact{{AID: "stays", Tag: "img"}}, 1)
	a := SnapshotList(list, 1)[0].Anchor
	if a == nil || a.Kind != "element" || a.AID != "stays" {
		t.Errorf("present aid was disturbed: %+v", a)
	}
}

// When the anchored aid is gone but exactly one candidate remains, the anchor
// rebinds to it (single-candidate heuristic).
func TestReconcileRebindsToSoleCandidate(t *testing.T) {
	list := elementComment(t, "e1", "gone")
	ReconcileAnchors(list, []StampedArtifact{{AID: "present", Tag: "img"}}, 1)
	a := SnapshotList(list, 1)[0].Anchor
	if a == nil || a.Kind != "element" || a.AID != "present" {
		t.Errorf("expected rebind to 'present', got %+v", a)
	}
}

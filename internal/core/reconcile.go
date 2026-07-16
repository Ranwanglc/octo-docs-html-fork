package core

import (
	"regexp"
	"strings"
	"time"
)

// Publish-time anchor reconciliation, ported from reconcile.ts. When a new
// version is published, an element anchor may no longer resolve. For each comment
// alive at the version we try to re-bind it by fingerprint + nearest-heading,
// appending an anchor_changed event; otherwise we mark it lost.

var aidSelectorRe = regexp.MustCompile(`\[data-odoc-aid="(\w+)"\]`)

// knownAid extracts the aid an anchor currently targets, if any.
func knownAid(a *Anchor) string {
	if a.Kind == "element" && a.AID != "" {
		return a.AID
	}
	if a.Kind == "element" && a.Selector != "" {
		if m := aidSelectorRe.FindStringSubmatch(a.Selector); m != nil {
			return m[1]
		}
	}
	return ""
}

// findRebindAid finds the single confident re-bind target for a drifted anchor.
func findRebindAid(a *Anchor, aids []StampedArtifact) string {
	wantTag := ""
	if a.Fingerprint != nil && a.Fingerprint.Tag != "" {
		wantTag = a.Fingerprint.Tag
	} else if a.Label != "" {
		wantTag = strings.ToLower(a.Label)
	}
	wantHead := ""
	if a.Fallback != nil && a.Fallback.NearestHeading != nil {
		wantHead = a.Fallback.NearestHeading.Text
	}

	var matches []StampedArtifact
	for _, x := range aids {
		tagOK := wantTag == "" || x.Tag == wantTag
		headOK := wantHead == "" || (x.Heading != nil && strings.EqualFold(*x.Heading, wantHead))
		if tagOK && headOK {
			matches = append(matches, x)
		}
	}
	if len(matches) == 1 {
		return matches[0].AID
	}
	if len(matches) == 0 {
		var tagOnly []StampedArtifact
		for _, x := range aids {
			if wantTag == "" || x.Tag == wantTag {
				tagOnly = append(tagOnly, x)
			}
		}
		if len(tagOnly) == 1 {
			return tagOnly[0].AID
		}
	}
	return ""
}

// nextAnchor computes the new anchor to record for a drifted comment: a rebind, a
// lost marker, or nil (no change).
func nextAnchor(a *Anchor, aids []StampedArtifact) *Anchor {
	newAID := findRebindAid(a, aids)
	if newAID != "" {
		label := a.Label
		if label == "" && a.Fingerprint != nil {
			label = a.Fingerprint.Tag
		}
		if label == "" {
			label = "element"
		}
		return &Anchor{
			Kind:        "element",
			AID:         newAID,
			Selector:    `[data-odoc-aid="` + newAID + `"]`,
			Label:       label,
			Fingerprint: a.Fingerprint,
			Fallback:    a.Fallback,
		}
	}
	if a.Kind == "lost" {
		return nil // already lost, no candidate — don't churn the log
	}
	return &Anchor{
		Kind:        "lost",
		Reason:      "no_candidate",
		Label:       a.Label,
		Fingerprint: a.Fingerprint,
		Fallback:    a.Fallback,
	}
}

func reconcileEvent(anchor *Anchor, version int, at string) CommentEvent {
	rs := false
	return CommentEvent{
		Kind: "anchor_changed", AtVersion: version, At: at, By: "reconcile",
		ResetStatus: &rs, Anchor: anchor,
	}
}

func reconcileComment(c *Comment, aids []StampedArtifact, byAID map[string]struct{}, version int, at string) {
	snap := SnapshotAt(c, version)
	if snap == nil || snap.Deleted {
		return
	}
	a := snap.Anchor
	if a == nil || (a.Kind != "element" && a.Kind != "lost") {
		return
	}
	aid := knownAid(a)
	if aid != "" {
		if _, ok := byAID[aid]; ok {
			return // still valid
		}
	}
	if anchor := nextAnchor(a, aids); anchor != nil {
		AppendEvent(c, reconcileEvent(anchor, version, at))
	}
}

// ReconcileAnchors reconciles open comment anchors against the freshly-stamped
// artifact set for a version, mutating comments in place.
func ReconcileAnchors(comments []Comment, aidsInVersion []StampedArtifact, v int) {
	EnsureMigrated(comments)
	byAID := make(map[string]struct{}, len(aidsInVersion))
	for _, a := range aidsInVersion {
		byAID[a.AID] = struct{}{}
	}
	version := v
	if version == 0 {
		version = 1
	}
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	for i := range comments {
		reconcileComment(&comments[i], aidsInVersion, byAID, version, at)
	}
}

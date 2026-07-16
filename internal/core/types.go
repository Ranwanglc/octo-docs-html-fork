package core

// Domain types for the comment event-log model, ported from comment.types.ts.
//
// JavaScript discriminated unions (Anchor, CommentEvent) are represented as flat
// structs with a Kind field and omitempty on optional members, so the JSON shape
// round-trips identically to the upstream representation.

import "encoding/json"

// AgentStatus is the agent's verdict on a comment, rendered as an emoji at fold time.
type AgentStatus string

// Agent verdict values.
const (
	StatusApplied  AgentStatus = "applied"
	StatusPartial  AgentStatus = "partial"
	StatusQuestion AgentStatus = "question"
)

// Author is a user or agent identity attached to a comment, reply, or reaction.
type Author struct {
	Login     string  `json:"login"`
	Name      string  `json:"name,omitempty"`
	AvatarURL *string `json:"avatar_url,omitempty"`
	Kind      string  `json:"kind,omitempty"` // "agent" for the publish-time agent identity
}

// AnchorFallback carries heuristic data used to re-bind a drifted element anchor.
type AnchorFallback struct {
	NearestHeading *struct {
		Text string `json:"text"`
	} `json:"nearestHeading,omitempty"`
	// Ratio is the original vertical position (0..1) used by the overlay to park
	// an unanchored card; preserved verbatim across folds.
	Ratio *float64 `json:"ratio,omitempty"`
}

// AnchorFingerprint is the legacy content fingerprint (tag hint only in the
// current model).
type AnchorFingerprint struct {
	Tag string `json:"tag,omitempty"`
}

// Anchor is where a comment is attached in the document. Kind is one of
// "text", "element", or "lost".
type Anchor struct {
	Kind string `json:"kind"`

	// text
	Text          string `json:"text,omitempty"`
	ContextBefore string `json:"context_before,omitempty"`
	ContextAfter  string `json:"context_after,omitempty"`

	// element
	AID      string `json:"aid,omitempty"`
	Selector string `json:"selector,omitempty"`
	Label    string `json:"label,omitempty"`

	// lost
	Reason string `json:"reason,omitempty"`

	// element + lost shared
	Fingerprint *AnchorFingerprint `json:"fingerprint,omitempty"`
	Fallback    *AnchorFallback    `json:"fallback,omitempty"`
}

// ReplySeed is the payload of a reply_added event.
type ReplySeed struct {
	ID          string      `json:"id"`
	Author      *Author     `json:"author"`
	Text        string      `json:"text"`
	AgentStatus AgentStatus `json:"agent_status,omitempty"`
}

// CommentEvent is one entry in a comment's append-only log. Kind discriminates
// the variant; only the fields relevant to that kind are populated.
type CommentEvent struct {
	Kind      string `json:"kind"`
	EID       string `json:"eid,omitempty"`
	AtVersion int    `json:"at_version"`
	At        string `json:"at"`

	// created / anchor_changed
	Anchor      *Anchor `json:"anchor,omitempty"`
	ResetStatus *bool   `json:"reset_status,omitempty"`

	// created / text_edited / reply_text_edited
	Text string `json:"text,omitempty"`

	// marked_applied
	AppliedIn *int `json:"applied_in,omitempty"`

	// status / authored events
	By          string      `json:"by,omitempty"`
	AgentStatus AgentStatus `json:"agent_status,omitempty"`

	// reactions
	Emoji string `json:"emoji,omitempty"`

	// replies
	Reply   *ReplySeed `json:"reply,omitempty"`
	ReplyID string     `json:"reply_id,omitempty"`
}

// Comment is a stored comment: stable identity plus its append-only event log.
// Legacy flat fields are tolerated on read and migrated lazily into Events.
type Comment struct {
	ID        string         `json:"id"`
	Author    *Author        `json:"author"`
	Created   string         `json:"created"`
	CreatedIn int            `json:"created_in"`
	Events    []CommentEvent `json:"events"`

	// Legacy fields (pre event-log), migrated by EnsureEventLog.
	Version   *int                `json:"version,omitempty"`
	Anchor    *Anchor             `json:"anchor,omitempty"`
	Text      *string             `json:"text,omitempty"`
	Status    string              `json:"status,omitempty"`
	AppliedIn *int                `json:"applied_in,omitempty"`
	Replies   []json.RawMessage   `json:"replies,omitempty"`
	Reactions map[string][]string `json:"reactions,omitempty"`
}

// Reactions maps emoji to the list of logins that reacted.
type Reactions map[string][]string

// ReplySnapshot is a reply folded to a point-in-time view.
type ReplySnapshot struct {
	ID          string       `json:"id"`
	ParentID    string       `json:"parent_id"`
	Author      *Author      `json:"author"`
	Text        string       `json:"text"`
	AgentStatus *AgentStatus `json:"agent_status"`
	Created     string       `json:"created"`
	Reactions   Reactions    `json:"reactions"`
	Deleted     bool         `json:"deleted"`
}

// CommentSnapshot is a comment folded to a point-in-time view (what the overlay
// and API consume).
type CommentSnapshot struct {
	ID        string          `json:"id"`
	Author    *Author         `json:"author"`
	Created   string          `json:"created"`
	CreatedIn int             `json:"created_in"`
	Version   int             `json:"version"`
	Anchor    *Anchor         `json:"anchor"`
	Text      string          `json:"text"`
	Status    string          `json:"status"`
	AppliedIn *int            `json:"applied_in,omitempty"`
	Replies   []ReplySnapshot `json:"replies"`
	Reactions Reactions       `json:"reactions"`
	Deleted   bool            `json:"deleted"`
}

// StampedArtifact is a commentable artifact discovered and stamped by StampAids.
type StampedArtifact struct {
	AID     string  `json:"aid"`
	Tag     string  `json:"tag"`
	Head    string  `json:"head"`
	Heading *string `json:"heading"`
}

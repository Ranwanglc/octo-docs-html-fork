package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
)

// docKeyPrefix + 26 hex chars = 28 total, which satisfies config.SafeSlug's
// ^[a-zA-Z0-9_-]{1,64}$ so a doc key is a drop-in slug for URLs/CLI/front-end.
const (
	docKeyPrefix   = "k_"
	docKeyHexLen   = 26
	docKeyTotalLen = len(docKeyPrefix) + docKeyHexLen // 28
)

// DeriveDocKey derives the server-side document identity from the creating
// user's uid and the client-supplied alias (the old "slug").
//
// key = "k_" + hex(sha256(creatorUID + "\x00" + alias))[:26]
//
// Why deterministic (not a random id): the existing idempotency contract says
// re-publishing under the same name appends a version to the SAME document, so
// the same (creator, alias) pair MUST map to the same key without an extra
// index table or schema change. The NUL separator makes the concatenation
// unambiguous (uid+alias can't collide with a different split).
//
// The key is NOT a secret: anyone who knows the creator uid + alias can recompute
// it. That is intentional — the key's job is to namespace documents PER CREATOR so
// two bots can both publish "weekly-report" without colliding, and so nobody can
// squat a global name. Write authorization is still enforced by CapEdit on the
// resolved document; it never relies on the key being unguessable.
func DeriveDocKey(creatorUID, alias string) string {
	sum := sha256.Sum256([]byte(creatorUID + "\x00" + alias))
	return docKeyPrefix + hex.EncodeToString(sum[:])[:docKeyHexLen]
}

// IsDocKey reports whether id has the server-derived doc-key shape: the "k_"
// prefix, total length 28, and 26 trailing lowercase hex chars. Legacy bare
// slugs (client-chosen names) never match.
func IsDocKey(id string) bool {
	if len(id) != docKeyTotalLen || id[:len(docKeyPrefix)] != docKeyPrefix {
		return false
	}
	for i := len(docKeyPrefix); i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// resolvedIdentity is the output of resolveDocIdentity: the concrete document
// key to read/write, and whether it names a brand-new document.
type resolvedIdentity struct {
	// key is the actual storage identity (blob/meta/cookie/authorization key).
	key string
	// alias is the human-readable client-supplied name, recorded on new docs.
	alias string
	// isNew is true only when neither candidate resolved to existing content.
	isNew bool
	// scheme is true when the new doc should be stamped key_scheme="dockey".
	scheme bool
}

// resolveDocIdentity maps a client identifier + caller creatorUID to the actual
// document key, shared by both creation paths (publish and draft-first) so they
// never diverge into two schemes. It is called BEFORE taking the lock (the lock
// scope is the resolved key, which is not known until resolution runs); the
// caller MUST re-check existence of the resolved key inside that lock so a
// check-then-delete-then-recreate race cannot let an unauthorized caller take
// over a document, and a doc created since resolution is not stamped as new
// (PR#30's TOCTOU protection).
//
// Resolution order:
//
//	① identifier already names existing content ⇒ target = identifier. Covers
//	   legacy bare-slug docs and clients already addressing a doc by its key.
//	② else, if creatorUID != "": key = DeriveDocKey(creatorUID, identifier);
//	   if key names existing content ⇒ target = key (same creator + same alias
//	   appends a version — the idempotency contract).
//	③ else ⇒ brand-new doc under key = DeriveDocKey(creatorUID, identifier),
//	   stamped alias=identifier + key_scheme="dockey".
//
// creatorUID == "" (no identity) never derives: behaviour collapses to ① / the
// legacy bare-slug path, unchanged.
func (s *DocService) resolveDocIdentity(ctx context.Context, identifier, creatorUID string) (resolvedIdentity, error) {
	// ① Does the identifier itself already name existing content?
	exists, err := s.slugExists(ctx, identifier)
	if err != nil {
		return resolvedIdentity{}, err
	}
	if exists {
		return resolvedIdentity{key: identifier, alias: identifier}, nil
	}
	// No identity ⇒ do not derive; keep legacy bare-slug semantics.
	if creatorUID == "" {
		return resolvedIdentity{key: identifier, alias: identifier, isNew: true}, nil
	}
	// ② Same creator + same alias must land on the same derived key.
	key := DeriveDocKey(creatorUID, identifier)
	if !safeSlugKey(key) {
		// Unreachable with the current 28-char shape, but assert rather than assume:
		// an unvalidatable key would be rejected downstream by requireSlug and leave
		// a doc that cannot be addressed.
		return resolvedIdentity{}, apperr.Upstream("derived doc key failed slug validation", "doc_key_invalid", nil)
	}
	keyExists, err := s.slugExists(ctx, key)
	if err != nil {
		return resolvedIdentity{}, err
	}
	if keyExists {
		return resolvedIdentity{key: key, alias: identifier}, nil
	}
	// ③ Brand-new document under the derived key.
	return resolvedIdentity{key: key, alias: identifier, isNew: true, scheme: true}, nil
}

// safeSlugKey reports whether a derived key passes the shared slug validator.
// resolveDocIdentity asserts this on every derived key so the invariant "a doc
// key is always a valid slug" is enforced at runtime, not merely documented.
func safeSlugKey(key string) bool {
	return config.SafeSlug(key) == key
}

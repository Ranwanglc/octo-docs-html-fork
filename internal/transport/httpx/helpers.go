package httpx

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
)

// maxJSONBody bounds JSON request bodies (comments, reactions, agent replies).
// Document HTML is published via multipart and governed separately by
// MAX_HTML_BYTES; no JSON endpoint needs a large body.
const maxJSONBody = 1 << 20 // 1 MiB

// decodeJSON decodes the request body into v, tolerating an empty/invalid body.
// The body is capped at maxJSONBody via http.MaxBytesReader to bound memory.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	return json.NewDecoder(r.Body).Decode(v)
}

// sessionCookieName is the cookie that carries the viewer session id.
const sessionCookieName = "odoc_sid"

// sessionCookie reads the session cookie value, or "".
func sessionCookie(r *http.Request) string {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// clearCookie expires a cookie. Flags mirror a real session cookie so a future
// login provider inherits safe defaults (HttpOnly, SameSite=Lax, Secure).
func clearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// setSessionCookie writes the odoc_sid cookie. Mirror of clearCookie's flags
// so login/logout are symmetric — a stale odoc_sid on a follow-up request
// resolves against a fresh, matching cookie shape.
func setSessionCookie(w http.ResponseWriter, sid string, maxAgeSeconds int, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		MaxAge:   maxAgeSeconds,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// capCookieMaxAge is how long a browser remembers a doc's share-code capability
// (30 days). Re-visiting the ?code= link refreshes it.
const capCookieMaxAge = 60 * 60 * 24 * 30

// setCapCookie stores a validated per-doc share code as an HttpOnly cookie so the
// browser carries it on later reads AND on the /v1 comment/reaction writes. The
// cookie NAME encodes the slug (octo_cap_<hash>), so Path=/ is safe: the browser
// may send several cap cookies, but the server only reads the one matching the
// requested doc — no cross-doc leakage. (A per-doc Path would fail, since /d/<slug>
// would not be sent to /v1/comments.)
func setCapCookie(w http.ResponseWriter, slug, code string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     capCookieName(slug),
		Value:    code,
		Path:     "/",
		MaxAge:   capCookieMaxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// requireSlug validates a slug from a string, returning a typed 400 on failure.
func requireSlug(value string) (string, error) {
	slug := config.SafeSlug(value)
	if slug == "" {
		return "", apperr.Validation("invalid or missing slug", "invalid_slug")
	}
	return slug, nil
}

// parseVersionParam parses a numeric version path segment.
func parseVersionParam(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// requireSHA256 validates a content-address path segment: exactly 64 lowercase
// hex chars. Rejecting anything else keeps the value safe as a storage key and
// hides malformed lookups behind a plain 400.
func requireSHA256(value string) (string, error) {
	if len(value) != 64 {
		return "", apperr.Validation("invalid asset id", "invalid_sha256")
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", apperr.Validation("invalid asset id", "invalid_sha256")
		}
	}
	return value, nil
}

// compactDigits keeps only the digits of a string (for compact ids).
func compactDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

// randHex4 returns 4 random bytes as 8 hex chars.
func randHex4() string {
	return randHex(4)
}

// randHex returns n random bytes as 2n hex chars, or a zero string on the (not
// expected) failure of the system RNG.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

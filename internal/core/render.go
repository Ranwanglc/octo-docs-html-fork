package core

import (
	"encoding/json"
	"strings"
)

// HTML helpers and overlay injection, ported from render.ts. The browser overlay
// is injected before </body>; the bytes reaching the browser are identical to the
// upstream worker's build-time inlining.

// OverlayIdentity is the minimal identity the overlay renders in its toolbar.
type OverlayIdentity struct {
	Login     string  `json:"login"`
	AvatarURL *string `json:"avatar_url,omitempty"`
	Name      string  `json:"name,omitempty"`
}

// OverlayConfig is the boot config injected as window.__ODOC__ for the overlay.
type OverlayConfig struct {
	Slug string `json:"slug"`
	// Title is the human doc title; the overlay top bar shows it instead of
	// degrading to the slug. omitempty keeps legacy byte output when unset.
	Title          string           `json:"title,omitempty"`
	Version        int              `json:"version"`
	Identity       *OverlayIdentity `json:"identity"`
	Mode           string           `json:"mode"`
	AuthConfigured bool             `json:"authConfigured"`
	IsOwner        bool             `json:"isOwner,omitempty"`
	Versions       []VersionRef     `json:"versions,omitempty"`
	OriginalSlug   string           `json:"originalSlug,omitempty"`
	// HostOrigins: postMessage sender allowlist (OCT-171). Derived from FRAME_ANCESTORS —
	// hosts allowed to iframe us are the hosts whose octo:init handshake we accept.
	// Empty ⇒ never accept a handshake (stand-alone deploy).
	HostOrigins []string `json:"hostOrigins,omitempty"`
	// CreatorUID / CreatorName / CreatedAt surface authorship + creation time for
	// display only (OCT-179). Authorship still resolves via __ODOC_CAP__.isAuthor;
	// never derive it from creator_uid. omitempty keeps legacy __ODOC__ bytes for
	// docs without a stamped creator or a draft without any published version.
	// CreatorName is a reserved slot — backend does not fill it yet; the frontend
	// falls back to CreatorUID when absent.
	CreatorUID  string `json:"creator_uid,omitempty"`
	CreatorName string `json:"creator_name,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"` // RFC3339, from Versions[0].Created
}

// VersionRef is one entry in the overlay's version picker.
type VersionRef struct {
	N       int     `json:"n"`
	Created *string `json:"created,omitempty"`
}

// SafeJSONForScript escapes </script> and HTML-comment openers so JSON can't
// break out of a <script> element. It uses an encoder with HTML escaping
// DISABLED so the byte output matches JavaScript's JSON.stringify (Go's default
// escapes <, >, & to \u00XX, which JS does not).
func SafeJSONForScript(v any) (string, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	s := strings.TrimRight(buf.String(), "\n") // Encoder appends a newline
	// Go's encoding/json always escapes U+2028/U+2029 (\u2028 / \u2029) even with
	// SetEscapeHTML(false); JavaScript's JSON.stringify emits them raw. Restore the
	// raw code points so the injected window.__ODOC__ bytes match upstream. (They
	// are valid inside a <script> JSON literal — only bare U+2028/9 in a JS string
	// literal would be a hazard, which this is not.)
	s = unescapeLineSep(s)
	s = neutralizeScriptClose(s)
	s = strings.ReplaceAll(s, "<!--", `<\!--`)
	return s, nil
}

// neutralizeScriptClose defangs every `</script` sequence by escaping its `<` to
// `<\`. HTML end-tag matching is case-INSENSITIVE and treats `</script` followed
// by `>`, whitespace, `/`, or EOF as a real close tag — so `</ScRiPt>` and
// `</script x>` both terminate the enclosing <script>. Matching only the exact
// lowercase `</script>` (the old behavior) let a title like `</ScRiPt><img ...>`
// break out of window.__ODOC__. The escaped `<\/` is harmless inside the JSON
// script literal and the browser never sees a live close tag. The lowercase
// `</script>` case still renders as `<\/script>`, preserving legacy bytes.
func neutralizeScriptClose(s string) string {
	const tag = "</script"
	if len(s) < len(tag) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '<' && i+len(tag) <= len(s) && strings.EqualFold(s[i:i+len(tag)], tag) {
			b.WriteString(`<\`)
			b.WriteString(s[i+1 : i+len(tag)])
			i += len(tag)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// unescapeLineSep rewrites genuine \u2028 / \u2029 JSON escape sequences (the
// 6-char ASCII text json emits) back to their raw code points, matching
// JavaScript JSON.stringify. A sequence is rewritten only when the backslash that
// starts it is itself unescaped (an even number of backslashes precede it), so a
// real escape is restored but the literal text \u2028 inside a string value
// (which json encodes as \\u2028 — an escaped backslash then u2028) is left
// intact. The preceding-backslash parity is tracked in the single forward pass.
func unescapeLineSep(s string) string {
	const esc8, esc9 = `\u2028`, `\u2029`
	if !strings.Contains(s, `\u202`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	bs := 0 // consecutive backslashes immediately before the current index
	for i := 0; i < len(s); {
		if s[i] == '\\' && bs%2 == 0 {
			if strings.HasPrefix(s[i:], esc8) {
				b.WriteRune('\u2028')
				i += len(esc8)
				bs = 0
				continue
			}
			if strings.HasPrefix(s[i:], esc9) {
				b.WriteRune('\u2029')
				i += len(esc9)
				bs = 0
				continue
			}
		}
		if s[i] == '\\' {
			bs++
		} else {
			bs = 0
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

// EscapeHTML escapes a string for interpolation into markup.
func EscapeHTML(s string) string {
	return htmlEscaper.Replace(s)
}

// ForHTMLComment neutralizes -- so an untrusted string can't terminate an HTML comment.
func ForHTMLComment(s string) string {
	return strings.ReplaceAll(s, "--", `-\-`)
}

// InjectOverlayCfg injects the overlay boot script + config before </body>.
func InjectOverlayCfg(rawHTML, overlayJS string, cfg OverlayConfig) (string, error) {
	cfgJSON, err := SafeJSONForScript(cfg)
	if err != nil {
		return "", err
	}
	inject := "<script>window.__ODOC__ = " + cfgJSON + ";</script>\n<script>" + overlayJS + "</script>"
	if strings.Contains(rawHTML, "</body>") {
		return strings.Replace(rawHTML, "</body>", inject+"\n</body>", 1), nil
	}
	return rawHTML + inject, nil
}

package core

import (
	"strings"
	"testing"
)

func TestEscapeHTML(t *testing.T) {
	cases := map[string]string{
		`a & b`:    `a &amp; b`,
		`<script>`: `&lt;script&gt;`,
		`"quoted"`: `&quot;quoted&quot;`,
		`it's`:     `it&#39;s`,
		`plain`:    `plain`,
		`<>&"'`:    `&lt;&gt;&amp;&quot;&#39;`,
	}
	for in, want := range cases {
		if got := EscapeHTML(in); got != want {
			t.Errorf("EscapeHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeJSONForScript(t *testing.T) {
	// Must NOT \u-escape <, >, & (match JS JSON.stringify), but MUST neutralize
	// </script> and <!--.
	cfg := OverlayConfig{Slug: "a<b>&c", Version: 1, Mode: "local", Identity: nil}
	out, err := SafeJSONForScript(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"slug":"a<b>&c"`; !contains(out, want) {
		t.Errorf("expected unescaped %q in %q", want, out)
	}

	tricky := map[string]string{"x": "</script><!--"}
	out2, err := SafeJSONForScript(tricky)
	if err != nil {
		t.Fatal(err)
	}
	if want := `<\/script><\!--`; !contains(out2, want) {
		t.Errorf("expected neutralized %q in %q", want, out2)
	}

	// HTML end-tag matching is case-insensitive and accepts whitespace/attrs after
	// `script`. A title payload like `</ScRiPt>` or `</script x>` must NOT survive
	// as a live close tag, or it breaks out of window.__ODOC__ (XSS). Each `<` that
	// opens a `</script...` sequence must be escaped to `<\`, preserving case.
	breakout := map[string]string{
		"a": "</ScRiPt><img src=x onerror=alert(1)>",
		"b": "</script x>",
		"c": "</SCRIPT\t>",
	}
	out2b, err := SafeJSONForScript(breakout)
	if err != nil {
		t.Fatal(err)
	}
	// No live close tag (case-insensitive) may remain.
	if idx := strings.Index(strings.ToLower(out2b), "</script"); idx >= 0 {
		t.Errorf("live </script close tag survived at %d: %q", idx, out2b)
	}
	// Case must be preserved after the escaped `<\`.
	if !contains(out2b, `<\/ScRiPt>`) || !contains(out2b, `<\/script x>`) || !contains(out2b, "<\\/SCRIPT\\t>") {
		t.Errorf("mixed-case script close not neutralized case-preservingly: %q", out2b)
	}

	// U+2028/U+2029 must survive as raw code points (matching JS JSON.stringify),
	// not Go's default \u2028 / \u2029 escaping. See parity trap 4 in docs/PORTING.md.
	sep := map[string]string{"x": "a\u2028b\u2029c"}
	out3, err := SafeJSONForScript(sep)
	if err != nil {
		t.Fatal(err)
	}
	if contains(out3, `\u2028`) || contains(out3, `\u2029`) {
		t.Errorf("U+2028/U+2029 must not be escaped, got %q", out3)
	}
	if want := "a\u2028b\u2029c"; !contains(out3, want) {
		t.Errorf("expected raw separators in %q", out3)
	}
}

func TestInjectOverlayCfg(t *testing.T) {
	cfg := OverlayConfig{Slug: "s", Version: 2, Mode: "published", Identity: nil}
	html, err := InjectOverlayCfg("<html><body>hi</body></html>", "console.log(1)", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(html, "window.__ODOC__ = ") || !contains(html, "console.log(1)") {
		t.Errorf("overlay not injected: %s", html)
	}
	if !contains(html, "</script>\n</body></html>") {
		t.Errorf("injection point wrong: %s", html)
	}
	// A non-empty Title must surface in __ODOC__ so the overlay top bar can show
	// the human title instead of degrading to the slug. Omitted when empty so the
	// legacy byte output is preserved. Fails before the OverlayConfig.Title field
	// exists; passes after.
	if contains(html, `"title":`) {
		t.Errorf("empty Title must be omitted: %s", html)
	}
	titled, err := InjectOverlayCfg("<html><body>hi</body></html>", "x", OverlayConfig{Slug: "tennis", Title: "网球 Tennis", Version: 1, Mode: "published"})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(titled, `"title":"网球 Tennis"`) {
		t.Errorf("human title missing from __ODOC__: %s", titled)
	}

	// No </body>: append.
	html2, _ := InjectOverlayCfg("<p>no body</p>", "x", cfg)
	if !contains(html2, "<p>no body</p><script>") {
		t.Errorf("append fallback wrong: %s", html2)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestSafeJSONForScriptLiteralEscapeNotCorrupted(t *testing.T) {
	// A value whose CONTENT is the literal 6-char text \u2028 must survive: json
	// encodes it as \\u2028 (escaped backslash + u2028); the unescape step must NOT
	// rewrite it to a raw separator.
	lit := map[string]string{"x": `pre\u2028post`}
	out, err := SafeJSONForScript(lit)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, `\\u2028`) {
		t.Errorf("literal escape corrupted: %q", out)
	}
	if contains(out, "\u2028") {
		t.Errorf("literal escape wrongly became a raw separator: %q", out)
	}
}

// TestOverlayConfigHostOrigins covers the OCT-171 wire contract: HostOrigins
// serializes as `hostOrigins` (camelCase, matches JS window.__ODOC__ readers)
// and is omitted when empty so stand-alone deploys don't ship the field.
func TestOverlayConfigHostOrigins(t *testing.T) {
	cfg := OverlayConfig{Slug: "s", Version: 1, Mode: "published", HostOrigins: []string{"https://web.example.com"}}
	out, err := SafeJSONForScript(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, `"hostOrigins":["https://web.example.com"]`) {
		t.Errorf("missing hostOrigins in output: %q", out)
	}

	empty := OverlayConfig{Slug: "s", Version: 1, Mode: "local"}
	out2, err := SafeJSONForScript(empty)
	if err != nil {
		t.Fatal(err)
	}
	if contains(out2, "hostOrigins") {
		t.Errorf("empty HostOrigins should be omitted, got %q", out2)
	}
}

// TestOverlayConfigCreatorFields covers the OCT-179 wire contract: creator_uid,
// creator_name, and created_at serialize as snake_case and are omitted when
// empty so legacy docs (no stamped creator, or a draft with no version yet) keep
// the old __ODOC__ byte output. Written against SafeJSONForScript(cfg) — the
// same substring-matching pattern TestOverlayConfigHostOrigins uses — so each
// field can be asserted positively and negatively without noise from unrelated
// InjectOverlayCfg wrapping.
func TestOverlayConfigCreatorFields(t *testing.T) {
	// Empty case: none of the three keys may appear in the payload.
	empty := OverlayConfig{Slug: "s", Version: 1, Mode: "published"}
	out, err := SafeJSONForScript(empty)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"creator_uid", "creator_name", "created_at"} {
		if contains(out, key) {
			t.Errorf("empty %s should be omitted, got %q", key, out)
		}
	}

	// CreatorUID + CreatedAt: keys and values both surface.
	cfg := OverlayConfig{
		Slug:       "s",
		Version:    1,
		Mode:       "published",
		CreatorUID: "u-abc",
		CreatedAt:  "2026-07-20T11:00:00Z",
	}
	out, err = SafeJSONForScript(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out, `"creator_uid":"u-abc"`) {
		t.Errorf("creator_uid missing: %q", out)
	}
	if !contains(out, `"created_at":"2026-07-20T11:00:00Z"`) {
		t.Errorf("created_at missing: %q", out)
	}
	if contains(out, "creator_name") {
		t.Errorf("empty CreatorName should be omitted, got %q", out)
	}

	// CreatorName reserved slot: filled independently to prove the field is wired.
	cfg2 := OverlayConfig{Slug: "s", Version: 1, Mode: "published", CreatorName: "张三"}
	out2, err := SafeJSONForScript(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(out2, `"creator_name":"张三"`) {
		t.Errorf("creator_name missing: %q", out2)
	}
}

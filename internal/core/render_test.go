package core

import "testing"

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

	// No </body>: append.
	html2, _ := InjectOverlayCfg("<p>no body</p>", "x", cfg)
	if !contains(html2, "<p>no body</p><script>") {
		t.Errorf("append fallback wrong: %s", html2)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOfStr(s, sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
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

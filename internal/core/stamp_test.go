package core

import (
	"strings"
	"testing"
)

// StampAids is a byte-exact port; these tests pin its observable behavior — which
// tags get a data-odoc-aid, the exact aid strings (a function of the frozen
// Cyrb53 over stripped content), idempotence, and the parse traps (attribute
// values containing '>', raw-text tags, void elements, already-stamped input).

func TestStampStampsArtifactTags(t *testing.T) {
	in := `<body><section><p>hi</p><img src="a.png"></section></body>`
	want := `<body><section data-odoc-aid="1l6mnuqtjhy"><p>hi</p><img src="a.png" data-odoc-aid="1etotygyt3m"></section></body>`
	res := StampAids(in)
	if res.HTML != want {
		t.Errorf("HTML:\n got %q\nwant %q", res.HTML, want)
	}
	if len(res.AIDs) != 2 {
		t.Fatalf("aids = %d, want 2", len(res.AIDs))
	}
	tags := map[string]bool{res.AIDs[0].Tag: true, res.AIDs[1].Tag: true}
	if !tags["section"] || !tags["img"] {
		t.Errorf("tags = %q/%q, want the set {section, img}", res.AIDs[0].Tag, res.AIDs[1].Tag)
	}
}

func TestStampNoArtifactsIsPassthrough(t *testing.T) {
	in := `<body><p>plain text no artifacts</p></body>`
	res := StampAids(in)
	if res.HTML != in {
		t.Errorf("passthrough changed HTML: %q", res.HTML)
	}
	if len(res.AIDs) != 0 {
		t.Errorf("aids = %d, want 0", len(res.AIDs))
	}
}

func TestStampIsIdempotent(t *testing.T) {
	in := `<body><section><p>x</p></section></body>`
	once := StampAids(in)
	twice := StampAids(once.HTML)
	if once.HTML != twice.HTML {
		t.Errorf("not idempotent:\n once %q\ntwice %q", once.HTML, twice.HTML)
	}
	// A pre-stamped element keeps its existing aid rather than getting a new one.
	pre := `<body><section data-odoc-aid="14m9wlpaboz"><p>x</p></section></body>`
	if got := StampAids(pre); got.HTML != pre {
		t.Errorf("re-stamped an already-stamped doc: %q", got.HTML)
	}
}

// The tag scanner must not treat a '>' inside a quoted attribute value as the end
// of the tag — a classic HTML-parse trap.
func TestStampAttributeValueWithGreaterThan(t *testing.T) {
	in := `<body><img alt="a > b" src="x.png"></body>`
	want := `<body><img alt="a > b" src="x.png" data-odoc-aid="2b8fykuv7qz"></body>`
	if got := StampAids(in); got.HTML != want {
		t.Errorf("attr-with-> :\n got %q\nwant %q", got.HTML, want)
	}
}

// Raw-text tags (script/style) are never stamped, and content inside them must not
// be mis-scanned for artifact tags.
func TestStampSkipsRawTextTags(t *testing.T) {
	in := `<body><script>var x=1</script><section><p>y</p></section></body>`
	res := StampAids(in)
	if len(res.AIDs) != 1 || res.AIDs[0].Tag != "section" {
		t.Fatalf("aids = %+v, want a single section", res.AIDs)
	}
	want := `<body><script>var x=1</script><section data-odoc-aid="1ywg46qkab5"><p>y</p></section></body>`
	if res.HTML != want {
		t.Errorf("script-skip:\n got %q\nwant %q", res.HTML, want)
	}
}

func TestStampVoidAndSvg(t *testing.T) {
	// Void element (img) gets the attribute inside the self-terminating tag.
	if got := StampAids(`<body><img src="a.png"></body>`).HTML; got != `<body><img src="a.png" data-odoc-aid="1etotygyt3m"></body>` {
		t.Errorf("void img: %q", got)
	}
	// SVG is stampable; viewBox is preserved verbatim (case-sensitive attr).
	svg := `<body><svg viewBox="0 0 24 24"><path d="M3 8"/></svg></body>`
	want := `<body><svg viewBox="0 0 24 24" data-odoc-aid="28osv6m0k8m"><path d="M3 8"/></svg></body>`
	if got := StampAids(svg).HTML; got != want {
		t.Errorf("svg:\n got %q\nwant %q", got, want)
	}
}

// The same content stamped twice yields the same aid (content-addressed); changing
// the content changes the aid.
func TestStampAidIsContentAddressed(t *testing.T) {
	a := StampAids(`<body><section><p>alpha</p></section></body>`).AIDs
	b := StampAids(`<body><section><p>alpha</p></section></body>`).AIDs
	c := StampAids(`<body><section><p>beta</p></section></body>`).AIDs
	if len(a) != 1 || len(b) != 1 || len(c) != 1 {
		t.Fatalf("expected one aid each: %d/%d/%d", len(a), len(b), len(c))
	}
	if a[0].AID != b[0].AID {
		t.Errorf("same content gave different aids: %s vs %s", a[0].AID, b[0].AID)
	}
	if a[0].AID == c[0].AID {
		t.Errorf("different content gave the same aid: %s", a[0].AID)
	}
}

// ElementByAID must locate an artifact in an already-stamped doc by the aid the
// stamper wrote, returning its full outer HTML (open tag through close tag) and
// tag name, and miss cleanly on an unknown aid.
func TestElementByAIDHitAndMiss(t *testing.T) {
	in := `<body><section><p>hi</p><img src="a.png"></section></body>`
	stamped := StampAids(in)
	// Find the section's aid from the stamp index.
	var sectionAID, imgAID string
	for _, a := range stamped.AIDs {
		switch a.Tag {
		case "section":
			sectionAID = a.AID
		case "img":
			imgAID = a.AID
		}
	}
	if sectionAID == "" || imgAID == "" {
		t.Fatalf("expected section+img aids, got %+v", stamped.AIDs)
	}

	outer, tag, ok := ElementByAID(stamped.HTML, sectionAID)
	if !ok {
		t.Fatal("section aid not found")
	}
	if tag != "section" {
		t.Errorf("tag = %q, want section", tag)
	}
	// Outer fragment must include the open and close tags and the inner <img>.
	if !strings.HasPrefix(outer, "<section") || !strings.HasSuffix(outer, "</section>") {
		t.Errorf("outer not a full section fragment: %q", outer)
	}
	if !strings.Contains(outer, `data-odoc-aid="`+sectionAID+`"`) {
		t.Errorf("outer missing the section aid: %q", outer)
	}

	// A void element (img) returns just its self-terminating tag.
	outerImg, tagImg, okImg := ElementByAID(stamped.HTML, imgAID)
	if !okImg || tagImg != "img" {
		t.Fatalf("img aid lookup = %q,%v", tagImg, okImg)
	}
	if !strings.HasPrefix(outerImg, "<img") || strings.Contains(outerImg, "</") {
		t.Errorf("img outer should be a single void tag: %q", outerImg)
	}

	if _, _, ok := ElementByAID(stamped.HTML, "no-such-aid"); ok {
		t.Error("unknown aid unexpectedly matched")
	}
	if _, _, ok := ElementByAID(stamped.HTML, ""); ok {
		t.Error("empty aid unexpectedly matched")
	}
}

// ReplaceElementByAID must swap exactly the located element's outer HTML, leaving
// the rest of the document byte-identical, and miss cleanly on an unknown aid.
func TestReplaceElementByAID(t *testing.T) {
	in := `<body><section><p>old</p></section><figure>keep</figure></body>`
	stamped := StampAids(in)
	var sectionAID string
	for _, a := range stamped.AIDs {
		if a.Tag == "section" {
			sectionAID = a.AID
		}
	}
	if sectionAID == "" {
		t.Fatalf("no section aid in %+v", stamped.AIDs)
	}
	out, ok := ReplaceElementByAID(stamped.HTML, sectionAID, `<section><p>new</p></section>`)
	if !ok {
		t.Fatal("replace missed a present aid")
	}
	if strings.Contains(out, "old") {
		t.Errorf("old content still present: %q", out)
	}
	if !strings.Contains(out, "new") {
		t.Errorf("new content missing: %q", out)
	}
	// The untouched sibling (and its stamped aid) must survive verbatim.
	if !strings.Contains(out, "<figure") || !strings.Contains(out, "keep") {
		t.Errorf("sibling figure clobbered: %q", out)
	}

	if _, ok := ReplaceElementByAID(stamped.HTML, "nope", `<section></section>`); ok {
		t.Error("replace matched an unknown aid")
	}
}

// SingleTopLevelTag gates aid replacements: exactly one top-level element passes;
// multi-element fragments, non-elements, and raw-text/script fragments are
// rejected so a replace can't smuggle extra nodes or scripts past the boundary.
func TestSingleTopLevelTag(t *testing.T) {
	pass := []string{
		`<section><p>x</p></section>`,
		`  <figure>only</figure>  `,
		`<img src="a.png">`,
		`<img src="a.png"/>`,
		`<div>a <span>nested</span> b</div>`,
	}
	for _, s := range pass {
		if _, ok := SingleTopLevelTag(s); !ok {
			t.Errorf("expected single-element accept: %q", s)
		}
	}
	fail := []string{
		``,
		`   `,
		`plain text`,
		`<section></section><section></section>`, // two top-level elements
		`<section></section> trailing`,           // trailing non-whitespace
		`<script>alert(1)</script>`,              // raw-text/script fragment
		`<style>.a{}</style>`,
		`text <section></section>`, // leading non-element
	}
	for _, s := range fail {
		if _, ok := SingleTopLevelTag(s); ok {
			t.Errorf("expected reject: %q", s)
		}
	}
}

// Fix C: a NON-void tag written self-closed (<section/>) must not be treated as
// void — the browser would swallow following siblings, so it needs an explicit
// close tag. Only true void tags (img/iframe) may skip the close, with or without
// the trailing slash.
func TestSingleTopLevelTagNonVoidSelfCloseRejected(t *testing.T) {
	reject := []string{
		`<section/>`,         // non-void self-closed, no close tag
		`<div/>`,             // same
		`<section/><p>x</p>`, // self-closed then a sibling
	}
	for _, s := range reject {
		if _, ok := SingleTopLevelTag(s); ok {
			t.Errorf("non-void self-close must be rejected: %q", s)
		}
	}
	accept := []string{
		`<section></section>`, // explicit close is fine
		`<iframe/>`,           // true void with slash
		`<iframe src="x">`,    // true void without slash
		`<img/>`,
	}
	for _, s := range accept {
		if _, ok := SingleTopLevelTag(s); !ok {
			t.Errorf("expected accept: %q", s)
		}
	}
}

// Fix B: SafeReplacementFragment layers injection scanning on top of the
// structural single-element check. Raw-text tags, event handlers, and
// javascript: URLs at ANY nesting depth are rejected even when the fragment is a
// single top-level element.
func TestSafeReplacementFragment(t *testing.T) {
	pass := []string{
		`<section><p>hello</p></section>`,
		`<img src="a.png">`,
		`<div><a href="https://example.com">ok</a></div>`,
	}
	for _, s := range pass {
		if _, ok := SafeReplacementFragment(s); !ok {
			t.Errorf("expected safe accept: %q", s)
		}
	}
	fail := []string{
		`<img src=x onerror=alert(1)>`,                   // event handler on a void tag
		`<section onload="x()"><p>y</p></section>`,       // event handler on top-level tag
		`<div><script>alert(1)</script></div>`,           // nested raw-text (inner script)
		`<div><style>.a{}</style></div>`,                 // nested style
		`<div><a href="javascript:alert(1)">x</a></div>`, // javascript: URL
		`<a href="JavaScript:evil()">x</a>`,              // case-insensitive scheme
		`<button onClick="go()">x</button>`,              // mixed-case handler
	}
	for _, s := range fail {
		if _, ok := SafeReplacementFragment(s); ok {
			t.Errorf("expected injection reject: %q", s)
		}
	}
}

// Fix D: hand-written replacements must not carry stamper-owned data-odoc-*
// attributes (Publish re-stamps only stampable open tags; residue ⇒ ambiguous
// selector).
func TestHasDataOdocAttr(t *testing.T) {
	has := []string{
		`<section data-odoc-aid="abc"><p>x</p></section>`,
		`<div data-odoc-artifact="1">y</div>`,
		`<img data-odoc-aid = "z">`,
	}
	for _, s := range has {
		if !HasDataOdocAttr(s) {
			t.Errorf("expected data-odoc detected: %q", s)
		}
	}
	clean := []string{
		`<section><p>x</p></section>`,
		`<div data-foo="1">y</div>`,
	}
	for _, s := range clean {
		if HasDataOdocAttr(s) {
			t.Errorf("false positive data-odoc: %q", s)
		}
	}
}

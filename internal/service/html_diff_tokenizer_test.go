package service

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
)

func TestDiffSVGTitleUsesForeignContentTokenization(t *testing.T) {
	before := `<svg><title><b>x</b></title></svg>`
	after := `<svg><title>&lt;b>x&lt;/b></title></svg>`
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 {
		t.Fatal("structure layer lost the svg:title child-element change")
	}
	if len(result.CodeHunks) == 0 {
		t.Fatal("code layer lost the foreign-content markup/text change")
	}

	// The standard parser is the semantic oracle: SVG title is not HTML RCDATA,
	// so the first document contains a child element and the second does not.
	parsed, err := xhtml.Parse(strings.NewReader(before))
	if err != nil {
		t.Fatal(err)
	}
	var title *xhtml.Node
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Namespace == "svg" && n.Data == "title" {
			title = n
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(parsed)
	if title == nil || title.FirstChild == nil || title.FirstChild.Type != xhtml.ElementNode || title.FirstChild.Data != "b" {
		t.Fatalf("parser oracle did not produce svg:title > svg:b: %#v", title)
	}
}

func TestDiffForeignIntegrationPointsFollowParserOracle(t *testing.T) {
	tests := []struct {
		name, source, wantPath string
	}{
		{"svg_foreignObject", `<svg><foreignObject><p>x</p></foreignObject></svg>`, `/html[1]/body[1]/svg[1]/foreignobject[1]/p[1]`},
		{"svg_desc", `<svg><desc><p>x</p></desc></svg>`, `/html[1]/body[1]/svg[1]/desc[1]/p[1]`},
		{"math_annotation_html", `<math><annotation-xml encoding="text/html"><p>x</p></annotation-xml></math>`, `/html[1]/body[1]/math[1]/annotation-xml[1]/p[1]`},
		{"math_annotation_xhtml", `<math><annotation-xml encoding="APPLICATION/XHTML+XML"><p>x</p></annotation-xml></math>`, `/html[1]/body[1]/math[1]/annotation-xml[1]/p[1]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := xhtml.Parse(strings.NewReader(test.source))
			if err != nil {
				t.Fatal(err)
			}
			var parserHasHTMLP bool
			var walk func(*xhtml.Node)
			walk = func(n *xhtml.Node) {
				parserHasHTMLP = parserHasHTMLP || n.Type == xhtml.ElementNode && n.Data == "p" && n.Namespace == ""
				for child := n.FirstChild; child != nil; child = child.NextSibling {
					walk(child)
				}
			}
			walk(parsed)
			if !parserHasHTMLP {
				t.Fatal("parser oracle did not create an HTML p")
			}
			if paths := diffNodePaths(t, test.source); !hasDiffPath(paths, test.wantPath) {
				t.Fatalf("paths = %v, want %s", paths, test.wantPath)
			}
		})
	}
}

func TestDiffMathMLTextIntegrationPointsFollowParserOracle(t *testing.T) {
	for _, tag := range []string{"mi", "mo", "mn", "ms", "mtext"} {
		t.Run(tag, func(t *testing.T) {
			selfClosed := "<math><" + tag + "><div/><p>x</p></" + tag + "></math>"
			explicit := "<math><" + tag + "><div><p>x</p></div></" + tag + "></math>"

			parsed, err := xhtml.Parse(strings.NewReader(selfClosed))
			if err != nil {
				t.Fatal(err)
			}
			var integration *xhtml.Node
			var walk func(*xhtml.Node)
			walk = func(n *xhtml.Node) {
				if n.Type == xhtml.ElementNode && n.Namespace == "math" && n.Data == tag {
					integration = n
				}
				for child := n.FirstChild; child != nil; child = child.NextSibling {
					walk(child)
				}
			}
			walk(parsed)
			if integration == nil || integration.FirstChild == nil || integration.FirstChild.Namespace != "" || integration.FirstChild.Data != "div" ||
				integration.FirstChild.FirstChild == nil || integration.FirstChild.FirstChild.Namespace != "" || integration.FirstChild.FirstChild.Data != "p" {
				t.Fatalf("parser oracle did not produce math:%s > html:div > html:p: %#v", tag, integration)
			}

			gotPaths := diffNodePaths(t, selfClosed)
			wantPaths := diffNodePaths(t, explicit)
			if strings.Join(gotPaths, "\n") != strings.Join(wantPaths, "\n") {
				t.Fatalf("self-closing paths = %v, explicit paths = %v", gotPaths, wantPaths)
			}
			wantP := "/html[1]/body[1]/math[1]/" + tag + "[1]/div[1]/p[1]"
			if !hasDiffPath(gotPaths, wantP) {
				t.Fatalf("paths = %v, want p nested under div at %s", gotPaths, wantP)
			}

			for _, source := range []string{selfClosed, explicit} {
				if err := scanDiffHTML(source, func(token diffHTMLToken) error {
					if (token.tag == "div" || token.tag == "p") && token.namespace != "" {
						t.Errorf("%s token namespace = %q, want HTML", token.tag, token.namespace)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDiffForeignBreakoutMatchesParserOracle(t *testing.T) {
	tests := []struct {
		name, before, after string
	}{
		{"svg_html_breakout", `<svg><div/><p>x</p></div></svg>`, `<svg><div></div><p>x</p></svg>`},
		{"math_mglyph_exception", `<math><mi><mglyph/><mrow>x</mrow></mi></math>`, `<math><mi><mglyph><mrow>x</mrow></mglyph></mi></math>`},
		{"annotation_xml_svg_exception", `<math><annotation-xml><svg><title><div/><p>x</p></div></title></svg></annotation-xml></math>`, `<math><annotation-xml><svg><title><div></div><p>x</p></title></svg></annotation-xml></math>`},
		{"font_empty_attribute_breakout", `<svg><g><font color=""/><circle/></g></svg>`, `<svg><g><font color=""></font><circle/></g></svg>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			beforeTree := parserElementTree(t, test.before)
			afterTree := parserElementTree(t, test.after)
			if beforeTree == afterTree {
				t.Fatal("parser oracle trees unexpectedly match")
			}
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 {
				t.Fatalf("different parser trees produced no structural changes\nbefore: %s\nafter:  %s", beforeTree, afterTree)
			}
		})
	}
}

func TestDiffForeignCDATAAtIntegrationPointMatchesParserOracle(t *testing.T) {
	before := `<svg><foreignObject><![CDATA[visible]]></foreignObject></svg>`
	after := `<svg><foreignObject></foreignObject></svg>`
	if parserElementTree(t, before) == parserElementTree(t, after) {
		t.Fatal("parser oracle trees unexpectedly match")
	}
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 {
		t.Fatal("CDATA text deletion produced no structural changes")
	}
}

func TestDiffForeignStartTagsDoNotLeakRawTextMode(t *testing.T) {
	tests := []struct {
		name, source string
		want         []string
	}{
		{"self_closing_title", `<svg><title/>tail<circle/></svg><p>A</p>`, []string{"/html[1]/body[1]/svg[1]/circle[1]", "/html[1]/body[1]/p[1]"}},
		{"plaintext", `<div><svg><plaintext>a</plaintext><circle/></svg><p>A</p></div>`, []string{"/html[1]/body[1]/div[1]/svg[1]/circle[1]", "/html[1]/body[1]/div[1]/p[1]"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			paths := diffNodePaths(t, test.source)
			for _, want := range test.want {
				if !hasDiffPath(paths, want) {
					t.Fatalf("paths = %v, want %s", paths, want)
				}
			}
		})
	}
}

func TestDiffConstrainsDOMPathTagBytes(t *testing.T) {
	tests := []struct {
		source, want string
	}{
		{`<div><b){x=1}<>y</div>`, `/html[1]/body[1]/div[1]/b[1]`},
		{`<div><a"onerror=alert(1)">y</div>`, `/html[1]/body[1]/div[1]/a[1]`},
		{`<A!>x</A!>`, `/html[1]/body[1]/a[1]`},
	}
	for _, test := range tests {
		paths := diffNodePaths(t, test.source)
		if !hasDiffPath(paths, test.want) {
			t.Fatalf("paths = %v, want %s", paths, test.want)
		}
	}
}

func TestDiffDOMPathSanitizingDoesNotChangeForeignSemantics(t *testing.T) {
	before := `<svg><g><div!/><circle/></g></svg>`
	after := `<svg><g><div!></div!><circle/></g></svg>`
	if parserElementTree(t, before) != parserElementTree(t, after) {
		t.Fatal("parser oracle trees unexpectedly differ")
	}
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 {
		t.Fatalf("parser-equivalent foreign tags produced changes: %+v", result.Changes)
	}
}

func TestDiffForeignEndTagBreakoutMatchesParserOracle(t *testing.T) {
	for _, source := range []string{
		`<svg><g></p><x></x><circle/></g></svg>`,
		`<math><mrow></br><x></x></mrow></math>`,
	} {
		parserTree := parserElementTree(t, source)
		paths := diffNodePaths(t, source)
		if !strings.Contains(parserTree, ":p\n") && !strings.Contains(parserTree, ":br\n") {
			t.Fatalf("parser oracle did not create breakout element: %s", parserTree)
		}
		if !hasDiffPath(paths, "/html[1]/body[1]/x[1]") {
			t.Fatalf("paths = %v, want breakout sibling /html[1]/body[1]/x[1]", paths)
		}
	}
}

func TestDiffSemanticTagIdentityMatchesParserOracle(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{"component_underscore", `<div><icon_home></icon_home></div>`, `<div><icon_close></icon_close></div>`},
		{"component_dot", `<div><app.header>a</app.header></div>`, `<div><app.footer>a</app.footer></div>`},
		{"malformed_suffix", `<div><b"q">y</b></div>`, `<div><b>y</b></div>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertParserStructuralChangeDetected(t, test.before, test.after)
		})
	}
}

func TestDiffSanitizedPathTagsRemainUnique(t *testing.T) {
	nodes, err := parseDiffHTML(`<div><icon_home>A</icon_home><icon_close>B</icon_close></div>`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]struct{}{}
	for _, node := range nodes {
		if _, duplicate := seen[node.path]; duplicate {
			t.Fatalf("duplicate DOM path %q", node.path)
		}
		seen[node.path] = struct{}{}
	}
}

func TestDiffSanitizedTagDoesNotInheritBuiltInSemantics(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{"pre", `<pre_x> a  b </pre_x>`, `<pre_x> a b </pre_x>`},
		{"script", `<script_x>&lt;b&gt;</script_x>`, `<script_x><b></b></script_x>`},
		{"textarea", `<textarea_x>&lt;b&gt;</textarea_x>`, `<textarea_x><b></b></textarea_x>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "pre" && len(result.Changes) != 0 {
				t.Fatalf("ordinary custom element inherited preformatted semantics: %+v", result.Changes)
			}
			if test.name != "pre" && len(result.Changes) == 0 {
				t.Fatal("ordinary custom element inherited raw-text semantics")
			}
		})
	}
}

func TestDiffForeignVoidNamesMatchParserOracle(t *testing.T) {
	for _, tag := range []string{"input", "link", "wbr"} {
		t.Run(tag, func(t *testing.T) {
			before := `<svg><` + tag + `><circle/></svg>`
			after := `<svg><` + tag + `/><circle/></svg>`
			assertParserStructuralChangeDetected(t, before, after)
		})
	}
}

func assertParserStructuralChangeDetected(t *testing.T, before, after string) {
	t.Helper()
	if parserElementTree(t, before) == parserElementTree(t, after) {
		t.Fatal("parser oracle trees unexpectedly equal")
	}
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 {
		t.Fatalf("structural change was silently missed: summary=%+v", result.Summary)
	}
}

func parserElementTree(t *testing.T, source string) string {
	t.Helper()
	root, err := xhtml.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	var walk func(*xhtml.Node, int)
	walk = func(node *xhtml.Node, depth int) {
		if node.Type == xhtml.ElementNode {
			out.WriteString(strings.Repeat("  ", depth))
			out.WriteString(node.Namespace)
			out.WriteByte(':')
			out.WriteString(node.Data)
			out.WriteByte('\n')
			depth++
		} else if node.Type == xhtml.TextNode && strings.TrimSpace(node.Data) != "" {
			out.WriteString(strings.Repeat("  ", depth))
			out.WriteString(strconv.Quote(node.Data))
			out.WriteByte('\n')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, depth)
		}
	}
	walk(root, 0)
	return out.String()
}

func TestDiffInvalidUTF8DoesNotDriftBetweenLayers(t *testing.T) {
	result, err := buildVersionDiff(1, 2, "<p>a\xffb</p>", "<p>a\xfeb</p>")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 {
		t.Fatal("structure layer lost the invalid UTF-8 text change")
	}
	if len(result.CodeHunks) == 0 {
		t.Fatal("code layer lost the invalid UTF-8 source-byte change")
	}
}

func TestDiffSequentialOptgroupsDoNotCreateFalseDepth(t *testing.T) {
	source := "<select>" + strings.Repeat("<optgroup><option>x", maxDiffDepth+1) + "</select>"
	if _, err := parseDiffHTML(source); err != nil {
		t.Fatalf("sequential implied-close optgroups rejected as nested depth: %v", err)
	}
}

func TestDiffScannerRawOffsetsAreExact(t *testing.T) {
	source := "π\r\n<!DOCTYPE html><DIV data-x='&amp;>z'>a&amp;b<!-- x --!><svg><path d='M0 0'/></svg></DIV>tail"
	cursor := 0
	err := scanDiffHTML(source, func(token diffHTMLToken) error {
		if token.start != cursor || token.end != cursor+len(token.raw) {
			t.Fatalf("offsets = [%d,%d), raw bytes=%d, want [%d,%d)", token.start, token.end, len(token.raw), cursor, cursor+len(token.raw))
		}
		if got := source[token.start:token.end]; got != token.raw {
			t.Fatalf("source slice = %q, Raw = %q", got, token.raw)
		}
		cursor = token.end
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cursor != len(source) {
		t.Fatalf("tokens end at byte %d, source has %d bytes", cursor, len(source))
	}
}

func TestDiffTokenizerMalformedAndForeignSemantics(t *testing.T) {
	for _, source := range []string{"<!--", "<div><!-- never closes", "<!--!>"} {
		if err := scanDiffHTML(source, func(diffHTMLToken) error { return nil }); !errors.Is(err, errDiffLimit) {
			t.Fatalf("scanDiffHTML(%q) err = %v, want errDiffLimit", source, err)
		}
	}

	source := `<html><body><div data-x=a/><p>html child</p></div><svg><g/><path d="x"/></svg><math><mspace/><mi>y</mi></math></body></html>`
	paths := diffNodePaths(t, source)
	for _, want := range []string{
		"/html[1]/body[1]/div[1]/p[1]",
		"/html[1]/body[1]/svg[1]/path[1]",
		"/html[1]/body[1]/math[1]/mi[1]",
	} {
		if !hasDiffPath(paths, want) {
			t.Fatalf("paths = %v, want %s", paths, want)
		}
	}
}

// TestDiffSnippetUsesNormalizedRendering keeps raw bytes exclusive to source hunks.
func TestDiffSnippetUsesNormalizedRendering(t *testing.T) {
	before := `<html><body><DIV DATA-X='&amp;>z' weird="a  b">old</DIV></body></html>`
	after := `<html><body><DIV DATA-X='&amp;>z' weird="a  b">new</DIV></body></html>`
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range result.Changes {
		if change.DOMPath != "/html[1]/body[1]/div[1]" {
			continue
		}
		if change.BeforeHTML != `<div data-x="&amp;&gt;z" weird="a  b">old</div>` {
			t.Fatalf("BeforeHTML = %q; want the renderer's normalized spelling", change.BeforeHTML)
		}
		return
	}
	t.Fatalf("div change missing: %+v", result.Changes)
}

func TestParseDiffHTMLLargeTokenAcrossFormerPrefixBoundary(t *testing.T) {
	padding := strings.Repeat("x", (128<<10)-len("<div><!--"))
	for _, source := range []string{
		"<div><!--" + padding + "--><p>ok</p></div>",
		`<div title="` + padding + `"><p>ok</p></div>`,
		"<style>/*" + padding + "*/</style><p>ok</p>",
	} {
		if _, err := parseDiffHTML(source); err != nil {
			t.Fatalf("valid large document rejected: %v", err)
		}
	}
}

func TestDiffTextUsesTokenizerRCDATAAndRAWTEXTSemantics(t *testing.T) {
	source := `<textarea>&amp;amp;</textarea><script>&amp;amp;</script>`
	if got := diffNodeWithTag(t, source, "textarea").text; got != "&amp;" {
		t.Fatalf("textarea text = %q, want one tokenizer entity decode", got)
	}
	if got := diffNodeWithTag(t, source, "script").text; got != "&amp;amp;" {
		t.Fatalf("script text = %q, want literal raw text", got)
	}
}

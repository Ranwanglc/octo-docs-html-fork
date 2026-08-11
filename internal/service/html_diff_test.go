package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

func TestBuildVersionDiffDetectsTextChangePastDisplayLimit(t *testing.T) {
	prefix := strings.Repeat("a", maxDiffCompareText+100)
	before := "<main><p>" + prefix + " before</p></main>"
	after := "<main><p>" + prefix + " after</p></main>"

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified == 0 || len(result.Changes) == 0 {
		t.Fatalf("change past comparison display limit was lost: %+v", result)
	}
}

// TestParseDiffHTMLImpliedEndTagsHavePathsAndSnippets prevents sibling swallowing.
func TestParseDiffHTMLImpliedEndTagsHavePathsAndSnippets(t *testing.T) {
	source := `<p>one<main>two</main><p>three<hgroup>head</hgroup><p>four<search>find</search><ul><li>a<li>b</ul><dl><dt>term<dd>value</dl><select><option>a<option>b</select><table><thead><tr><th>h<tbody><tr><td>x<td>y</table>`
	nodes, err := parseDiffHTML(source)
	if err != nil {
		t.Fatal(err)
	}
	const body = "/html[1]/body[1]"
	want := map[string]string{
		body + "/p[1]":                          "<p>one</p>",
		body + "/p[2]":                          "<p>three</p>",
		body + "/p[3]":                          "<p>four</p>",
		body + "/ul[1]/li[1]":                   "<li>a</li>",
		body + "/dl[1]/dt[1]":                   "<dt>term</dt>",
		body + "/dl[1]/dd[1]":                   "<dd>value</dd>",
		body + "/select[1]/option[1]":           "<option>a</option>",
		body + "/table[1]/thead[1]":             "<thead><tr><th>h</th></tr></thead>",
		body + "/table[1]/tbody[1]/tr[1]/td[1]": "<td>x</td>",
	}
	for _, node := range nodes {
		snippet := diffNodeSnippet(node)
		if expected, ok := want[node.path]; ok {
			if snippet != expected {
				t.Errorf("%s snippet = %q; want %q", node.path, snippet, expected)
			}
			delete(want, node.path)
		}
		if snippet == "" {
			t.Errorf("%s has no snippet", node.path)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing paths: %v", want)
	}
}

func TestDiffTextDigestUsesCompleteCanonicalSemantics(t *testing.T) {
	before := diffNodeWithTag(t, `<p>A &amp; B   &#x63;`+strings.Repeat(" ", 20)+`tail</p>`, "p")
	after := diffNodeWithTag(t, `<p>A &#38; B c tail</p>`, "p")
	if before.textDigest != after.textDigest || diffNodeSignature(before) != diffNodeSignature(after) {
		t.Fatalf("semantically equivalent text differs: %q / %q", before.text, after.text)
	}

	var split htmlDiffNode
	appendDiffNodeText(&split, "A &am")
	appendDiffNodeText(&split, "p; B")
	if got := strings.Join(strings.Fields(html.UnescapeString(strings.Join(split.textParts, ""))), " "); got != "A & B" {
		t.Fatalf("entity split across chunks = %q", got)
	}
}

func TestDiffRawTextDigestPreservesBytes(t *testing.T) {
	before := diffNodeWithTag(t, `<script>let x = 1</script>`, "script")
	after := diffNodeWithTag(t, `<script>let  x = 1</script>`, "script")
	if before.textDigest == after.textDigest {
		t.Fatal("script whitespace change was lost")
	}
}

// diffNodeWithTag ignores synthetic document wrappers in tests.
func diffNodeWithTag(t *testing.T, source, tag string) htmlDiffNode {
	t.Helper()
	nodes, err := parseDiffHTML(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if node.tag == tag {
			return node
		}
	}
	t.Fatalf("no %s node in %q", tag, source)
	return htmlDiffNode{}
}

func TestParseDiffAttrsDuplicateNamesUseFirstValue(t *testing.T) {
	var attrs map[string]string
	err := scanDiffHTML(`<input VALUE="first" value="second" VaLuE="third">`, func(token diffHTMLToken) error {
		if token.tag == "input" {
			attrs = token.attrs
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attrs["value"] != "first" {
		t.Fatalf("value = %q; want first", attrs["value"])
	}
}

func TestBuildVersionDiffPreservesQuotedAttributeWhitespace(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{"duplicate first wins", `<input value="A B" value="same">`, `<input value="A  B" value="same">`},
		{"ordinary attribute", `<p title="a b">x</p>`, `<p title="a  b">x</p>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.CodeHunks) == 0 {
				t.Fatalf("quoted whitespace change produced no source hunk: %+v", result)
			}
			if test.name == "duplicate first wins" && len(result.Changes) == 0 {
				t.Fatalf("browser-visible duplicate attribute change was lost: %+v", result)
			}
		})
	}
}

func TestNormalizedHTMLLinesIgnoreFormattingWhitespace(t *testing.T) {
	compact := `<html><body><p>alpha</p><p>beta</p></body></html>`
	pretty := "<html>\n  <body>\n    <p>alpha</p>\n    <p>beta</p>\n  </body>\n</html>\n"
	result, err := buildVersionDiff(1, 2, compact, pretty)
	if err != nil {
		t.Fatal(err)
	}
	// The whitespace fingerprint is now unconditional, so a format-only reindent
	// surfaces as exactly one bounded synthetic whitespace record and no structural
	// changes (the old zero-hunk contract is superseded).
	if len(result.Changes) != 0 {
		t.Fatalf("format-only revision produced structural changes: %+v", result)
	}
	total := 0
	for _, h := range result.CodeHunks {
		total += len(h.Lines)
	}
	if total > reindentMaxHunkLines {
		t.Fatalf("format-only revision produced %d hunk lines; want a small bounded hunk", total)
	}
	if total > 0 && !strings.Contains(hunksBody(result.CodeHunks), "[formatting whitespace changed]") {
		t.Fatalf("format-only hunk is not the whitespace record: %+v", result)
	}
	assertNoLeakedInternals(t, result.CodeHunks)
	lines, ok := normalizedHTMLLines("<p>x</p>\u00a0")
	// One trailing NBSP text run plus the unconditional ws-doc record: <p> tag,
	// x text, </p> tag, NBSP text, ws-doc.
	if !ok || len(lines) != 5 || lines[3].display != "\u00a0" {
		t.Fatalf("NBSP text run lost: %#v, ok=%v", lines, ok)
	}
}

// TestStructuralDigestIgnoresBlockSiblingIndentation guards the reindent-only
// P1: pure formatting whitespace between block-like siblings must not pollute
// the parent's structural textDigest, while whitespace at a visible inline
// boundary must still register.
func TestStructuralDigestIgnoresBlockSiblingIndentation(t *testing.T) {
	for _, count := range []int{100, 500} {
		t.Run(fmt.Sprintf("main_%d_paragraphs", count), func(t *testing.T) {
			var compact, pretty strings.Builder
			compact.WriteString("<main>")
			pretty.WriteString("<main>\n")
			for index := 0; index < count; index++ {
				fmt.Fprintf(&compact, "<p>item %d</p>", index)
				fmt.Fprintf(&pretty, "  <p>item %d</p>\n", index)
			}
			compact.WriteString("</main>")
			pretty.WriteString("</main>\n")
			result, err := buildVersionDiff(1, 2, compact.String(), pretty.String())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) != 0 {
				t.Fatalf("reindent-only revision polluted the structural digest: %+v", result)
			}
		})
	}

	blockCases := []struct {
		name          string
		before, after string
	}{
		{"div_block_children", `<div><p>x</p><ul><li>y</li></ul></div>`, "<div>\n  <p>x</p>\n  <ul>\n    <li>y</li>\n  </ul>\n</div>"},
		{"table_rows", `<table><tr><td>a</td></tr><tr><td>b</td></tr></table>`, "<table>\n  <tr><td>a</td></tr>\n  <tr><td>b</td></tr>\n</table>"},
	}
	for _, test := range blockCases {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) != 0 {
				t.Fatalf("block-sibling reindent polluted the structural digest: %+v", result)
			}
		})
	}

	// Inline-sibling whitespace is a visible boundary and must still be detected.
	inlineCases := []struct {
		name          string
		before, after string
	}{
		{"inline_span_gap", `<p><a>x</a><a>y</a></p>`, `<p><a>x</a> <a>y</a></p>`},
		{"see_here_gap", `<p>see<a>here</a></p>`, `<p>see <a>here</a></p>`},
		{"custom_element_gap", `<p><x-tag>x</x-tag><x-tag>y</x-tag></p>`, `<p><x-tag>x</x-tag> <x-tag>y</x-tag></p>`},
	}
	for _, test := range inlineCases {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 {
				t.Fatalf("visible inline-boundary whitespace change was lost: %+v", result)
			}
		})
	}
}

func TestBuildVersionDiffPreservesWhitespaceInsideChildlessInlineElement(t *testing.T) {
	inlineCases := []struct {
		name          string
		before, after string
	}{
		{"span", `<p>x<span></span>y</p>`, `<p>x<span> </span>y</p>`},
		{"bold", `<p>x<b></b>y</p>`, `<p>x<b> </b>y</p>`},
		{"anchor", `<p>x<a href="#"></a>y</p>`, `<p>x<a href="#"> </a>y</p>`},
		{"em_newline", `<p>x<em></em>y</p>`, "<p>x<em>\n</em>y</p>"},
	}
	for _, test := range inlineCases {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) != 1 || result.Summary.Modified != 1 {
				t.Fatalf("visible inline whitespace change was lost: %+v", result)
			}
		})
	}

	result, err := buildVersionDiff(1, 2, `<section><div></div></section>`, `<section><div> </div></section>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 || result.Summary.Modified != 0 {
		t.Fatalf("block-only formatting whitespace became structural: %+v", result)
	}
}

func TestBuildVersionDiffPrettyPrintedDocumentsStayBounded(t *testing.T) {
	var source strings.Builder
	source.WriteString("<main>\n")
	for index := 0; index < 3500; index++ {
		fmt.Fprintf(&source, "  <i>%d</i>\n", index)
	}
	source.WriteString("</main>\n")
	result, err := buildVersionDiff(1, 2, source.String(), source.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 || len(result.CodeHunks) != 0 {
		t.Fatalf("identical pretty document differed: %+v", result)
	}
}

func TestMatchDiffNodesIsDeterministicNearBudget(t *testing.T) {
	var before, after strings.Builder
	before.WriteString("<main>")
	after.WriteString("<main>")
	for index := 0; index < 350; index++ {
		fmt.Fprintf(&before, "<p>before %d</p>", index)
		fmt.Fprintf(&after, "<p>after %d</p>", index)
	}
	before.WriteString("</main>")
	after.WriteString("</main>")
	var first string
	for run := 0; run < 100; run++ {
		result, err := buildVersionDiff(1, 2, before.String(), after.String())
		encoded, _ := json.Marshal(result)
		got := fmt.Sprintf("%v:%s", err, encoded)
		if run == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d = %q; first = %q", run, got, first)
		}
	}
}

func TestMatchDiffNodesBoundsAsymmetricSiblingWork(t *testing.T) {
	var beforeSource, afterSource strings.Builder
	beforeSource.WriteString("<main>")
	for index := 0; index < 3900; index++ {
		fmt.Fprintf(&beforeSource, "<i>b%d</i>", index)
	}
	beforeSource.WriteString("</main>")
	afterSource.WriteString("<main>")
	for index := 0; index < 51; index++ {
		fmt.Fprintf(&afterSource, "<i>a%d</i>", index)
	}
	afterSource.WriteString("</main>")
	before, err := parseDiffHTML(beforeSource.String())
	if err != nil {
		t.Fatal(err)
	}
	after, err := parseDiffHTML(afterSource.String())
	if err != nil {
		t.Fatal(err)
	}
	for run := 0; run < 2; run++ {
		if _, err := matchDiffNodes(before, after); err != errDiffLimit {
			t.Fatalf("run %d error = %v; want diff limit", run, err)
		}
	}
}

func TestMatchManyIdenticalSiblingsDoesNotExhaustBudget(t *testing.T) {
	// Leave room for the tree builder's html/head/body wrappers.
	const wrappers = 3
	for _, siblings := range []int{700, maxDiffNodes - wrappers - 1} {
		t.Run(strconv.Itoa(siblings), func(t *testing.T) {
			source := `<main>` + strings.Repeat(`<span>same</span>`, siblings) + `</main>`
			before, err := parseDiffHTML(source)
			if err != nil {
				t.Fatal(err)
			}
			after, err := parseDiffHTML(source)
			if err != nil {
				t.Fatal(err)
			}
			matches, err := matchDiffNodes(before, after)
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != siblings+1+wrappers {
				t.Fatalf("matches = %d; want %d", len(matches), siblings+1+wrappers)
			}
		})
	}
}

func TestDiffCodeHunksRejectsHunkLineOverflow(t *testing.T) {
	before := strings.Repeat("<br>", maxDiffHunkLines/2+1)
	after := strings.Repeat("<hr>", maxDiffHunkLines/2+1)
	if _, err := diffCodeHunks(before, after); err != errDiffLimit {
		t.Fatalf("error = %v; want diff limit", err)
	}
}

func TestDiffOutputSizeIsEncodedJSONSize(t *testing.T) {
	result := &VersionDiff{Changes: []ElementChange{{Kind: "modified", BeforeHTML: strings.Repeat(`"`, 100)}}}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if got := diffOutputSize(result); got != len(encoded) {
		t.Fatalf("size = %d; want %d", got, len(encoded))
	}
}

func TestDiffTruncationPreservesUTF8(t *testing.T) {
	line := strings.Repeat("界", 400)
	if got := displayDiffLine(line); !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8: %q", got)
	}
}

// TestDiffUnclosedTagTailIsPlainTextAndTerminates covers the malformed-input
// hardening for the diff parser: an unclosed '<' tail (no closing '>' exists)
// must be treated as plain text and terminate the scan — never an invalid
// slice, panic, or infinite loop — while the existing bounds still hold. The
// cases mirror the acceptance list ('<', 'abc<', '<div', '<!--', '<script')
// plus a few adjacent shapes. Exercises BOTH parse paths (parseDiffHTML for the
// structural tree and normalizedHTMLLines for the code-hunk view) via the full
// buildVersionDiff entry point.
func TestDiffUnclosedTagTailIsPlainTextAndTerminates(t *testing.T) {
	cases := []string{
		"<",
		"abc<",
		"<div",
		"<script",
		"text<",
		"<!",
		"<?",
		"</",
		"</div",
		"<div class=\"x",
		"<a href='",
		"<script>alert(1)",
		"<style>body{",
		"<textarea>hi",
		"<p>ok</p>trailing<",
	}
	for _, source := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("parse panicked on %q: %v", source, r)
				}
			}()
			if _, err := parseDiffHTML(source); err != nil && err != errDiffLimit {
				t.Fatalf("parseDiffHTML(%q) unexpected err = %v", source, err)
			}
			if _, ok := normalizedHTMLLines(source); !ok {
				t.Fatalf("normalizedHTMLLines(%q) returned ok=false for a bounded input", source)
			}
			if _, err := buildVersionDiff(1, 2, source, source+"x"); err != nil && err != errDiffLimit {
				t.Fatalf("buildVersionDiff(%q) unexpected err = %v", source, err)
			}
		}()
	}
	// Both views accept the tokenizer's EOF recovery for an open comment.
	for _, source := range []string{"<!--", "<!--unterminated comment"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("parse panicked on %q: %v", source, r)
				}
			}()
			if _, ok := normalizedHTMLLines(source); !ok {
				t.Fatalf("normalizedHTMLLines(%q) returned ok=false", source)
			}
			if _, err := buildVersionDiff(1, 2, source, source+"x"); err != nil {
				t.Fatalf("buildVersionDiff(%q) err = %v", source, err)
			}
		}()
	}
}

func TestBuildVersionDiffMatchesChangedElementWithoutStableAID(t *testing.T) {
	before := `<html><body><section data-odoc-aid="old"><p class="lead">alpha text</p></section></body></html>`
	after := `<html><body><section data-odoc-aid="new"><p class="lead">alpha text updated</p></section></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 1 || result.Summary.Added != 0 || result.Summary.Removed != 0 {
		t.Fatalf("summary = %+v", result.Summary)
	}
	change := result.Changes[0]
	if change.Kind != "modified" || change.DOMPath != "/html[1]/body[1]/section[1]/p[1]" {
		t.Fatalf("change = %+v", change)
	}
	if change.BeforeHTML != `<p class="lead">alpha text</p>` || change.AfterHTML != `<p class="lead">alpha text updated</p>` {
		t.Fatalf("unexpected local HTML: %+v", change)
	}
	if len(result.CodeHunks) != 1 || len(result.CodeHunks[0].Lines) == 0 {
		t.Fatalf("code hunks = %+v", result.CodeHunks)
	}
}

func TestBuildVersionDiffRejectsExcessiveNormalizedLines(t *testing.T) {
	var source strings.Builder
	for range maxDiffInputLines + 1 {
		source.WriteString("x<br>")
	}
	if _, err := buildVersionDiff(1, 2, source.String(), source.String()+"changed"); err != errDiffLimit {
		t.Fatalf("error = %v; want diff limit", err)
	}
}

func TestLayoutCSSNewlineRelocationSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name, css, gap, tag string
		distance            int
	}{
		{"style-lf", `<style>p{display:inline}</style>`, "\n", "p", 1},
		{"style-crlf", `<style>li{display:inline}</style>`, "\r\n", "li", 1},
		{"style-indent", `<style>div{display:inline}</style>`, "\n  ", "div", 1},
		{"move-four", `<style>p{display:inline}</style>`, "\n", "p", 4},
		{"stylesheet", `<link rel="stylesheet" href="x.css">`, "\n", "p", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			element := func(i int) string { return fmt.Sprintf("<%s>%d</%s>", tc.tag, i, tc.tag) }
			var before, after strings.Builder
			before.WriteString(tc.css)
			after.WriteString(tc.css)
			for i := 0; i < tc.distance+2; i++ {
				before.WriteString(element(i))
				after.WriteString(element(i))
				if i == 0 {
					before.WriteString(tc.gap)
				}
				if i == tc.distance {
					after.WriteString(tc.gap)
				}
			}
			result, err := buildVersionDiff(1, 2, before.String(), after.String())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
				t.Fatal("newline relocation produced an empty diff")
			}
			assertNoLeakedInternals(t, result.CodeHunks)
		})
	}
}

func TestManyIdenticalLFEventsRelocationStaysNonEmptyAndBounded(t *testing.T) {
	for _, count := range []int{130, 500, 1500} {
		t.Run("n-"+strconv.Itoa(count), func(t *testing.T) {
			var before, after strings.Builder
			before.WriteString(`<style>span{display:inline}</style><div>`)
			after.WriteString(`<style>span{display:inline}</style><div>`)
			for i := 0; i < count+2; i++ {
				element := fmt.Sprintf("<span>%d</span>", i)
				before.WriteString(element)
				after.WriteString(element)
				if i < count {
					before.WriteByte('\n')
				}
				if i != count-1 && i <= count {
					after.WriteByte('\n')
				}
			}
			before.WriteString(`</div>`)
			after.WriteString(`</div>`)

			if fpBefore, fpAfter := wsDocFingerprintForSource(t, before.String()), wsDocFingerprintForSource(t, after.String()); fpBefore == fpAfter {
				t.Fatalf("count=%d: relocated LF event left fingerprint unchanged", count)
			}
			result, err := buildVersionDiff(1, 2, before.String(), after.String())
			if err != nil {
				t.Fatalf("count=%d: relocation failed: %v", count, err)
			}
			if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
				t.Fatalf("count=%d: relocation produced an empty diff", count)
			}
			total := 0
			for _, hunk := range result.CodeHunks {
				total += len(hunk.Lines)
			}
			if total > 20 {
				t.Fatalf("count=%d: relocation produced %d hunk lines", count, total)
			}
			assertNoLeakedInternals(t, result.CodeHunks)
		})
	}
}

func TestLineDiffResyncsAfterSingleParagraphInsert(t *testing.T) {
	var before, after strings.Builder
	for i := range 600 {
		line := fmt.Sprintf("<p>paragraph %d</p>\n", i)
		before.WriteString(line)
		after.WriteString(line)
		if i == 299 {
			after.WriteString("<p>inserted</p>\n")
		}
	}
	result, err := buildVersionDiff(1, 2, before.String(), after.String())
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, hunk := range result.CodeHunks {
		total += len(hunk.Lines)
	}
	if total > 20 {
		t.Fatalf("single insert produced %d hunk lines", total)
	}
}

func TestLineDiffAllParagraphTextSubstitutionsStayBounded(t *testing.T) {
	var before, after strings.Builder
	for i := range 338 {
		fmt.Fprintf(&before, "<p>before %d</p>\n", i)
		fmt.Fprintf(&after, "<p>after %d</p>\n", i)
	}
	result, err := buildVersionDiff(1, 2, before.String(), after.String())
	if err != nil {
		t.Fatalf("all-text substitution returned 413: %v", err)
	}
	if len(result.CodeHunks) != 1 {
		t.Fatalf("code hunks = %d; want 1", len(result.CodeHunks))
	}
	// Myers must keep every unchanged structural line as context (2n+1 tags plus
	// n '-' and n '+'), not dump whole windows.
	context, changed := 0, 0
	for _, line := range result.CodeHunks[0].Lines {
		switch {
		case strings.HasPrefix(line, "-"), strings.HasPrefix(line, "+"):
			changed++
		default:
			context++
		}
	}
	if want := 2*338 + 1; context != want {
		t.Fatalf("context lines = %d; want %d", context, want)
	}
	if want := 2 * 338; changed != want {
		t.Fatalf("changed lines = %d; want %d", changed, want)
	}
}

func TestDiffLineOpsMyersCases(t *testing.T) {
	tests := []struct {
		name, wantKinds    string
		oldLines, newLines []string
	}{
		{"repeated lines", " +  ", []string{"q", "q", "q"}, []string{"q", "insert", "q", "q"}},
		{"single insertion", "  + ", []string{"a", "b", "c"}, []string{"a", "b", "x", "c"}},
		{"asymmetric eof", "  --", []string{"a", "b", "c", "d"}, []string{"a", "b"}},
		{"short eof", "+ ", []string{"q"}, []string{"insert", "q"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ops, ok := diffLineOps(diffTestLines(tc.oldLines...), diffTestLines(tc.newLines...))
			if !ok {
				t.Fatal("diffLineOps rejected bounded input")
			}
			if got := diffOpKinds(ops); got != tc.wantKinds {
				t.Fatalf("operation kinds = %q; want %q", got, tc.wantKinds)
			}
		})
	}
}

func TestDiffLineOpsDeterministic(t *testing.T) {
	oldLines := diffTestLines("a", "q", "q", "old", "q", "z")
	newLines := diffTestLines("a", "q", "new", "q", "q", "z")
	var first string
	for run := range 100 {
		ops, ok := diffLineOps(oldLines, newLines)
		if !ok {
			t.Fatalf("run %d rejected bounded input", run)
		}
		got := diffOpKinds(ops)
		if run == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d = %q; first = %q", run, got, first)
		}
	}
}

func TestDiffLineOpsResyncsRepeatedSuffixAtEOF(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suffix []string
	}{
		{name: "two-lines", suffix: []string{"q", "q"}},
		{name: "one-line", suffix: []string{"q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldIDs := append([]string(nil), tc.suffix...)
			newIDs := append([]string{"insert"}, tc.suffix...)
			ops, ok := diffLineOps(diffTestLines(oldIDs...), diffTestLines(newIDs...))
			if !ok {
				t.Fatal("diffLineOps rejected bounded input")
			}
			wantKinds := "+" + strings.Repeat(" ", len(tc.suffix))
			if got := diffOpKinds(ops); got != wantKinds {
				t.Fatalf("operation kinds = %q; want %q", got, wantKinds)
			}
		})
	}
}

func TestDiffLineOpsFindsShortestRepeatedLineAlignment(t *testing.T) {
	for _, tc := range []struct {
		name, wantKinds    string
		oldLines, newLines []string
	}{
		{"both-have-tail", "+ -+ ", []string{"q", "old", "q"}, []string{"insert", "q", "new", "q"}},
		{"old-ends-amid-repeats", "+ + ", []string{"q", "q"}, []string{"insert", "q", "tail", "q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops, ok := diffLineOps(diffTestLines(tc.oldLines...), diffTestLines(tc.newLines...))
			if !ok {
				t.Fatal("diffLineOps rejected bounded input")
			}
			if got := diffOpKinds(ops); got != tc.wantKinds {
				t.Fatalf("operation kinds = %q; want %q", got, tc.wantKinds)
			}
		})
	}
}

func TestDiffLineOpsRejectsOversizedInput(t *testing.T) {
	lines := make([]diffSourceLine, maxDiffInputLines+1)
	if _, ok := diffLineOps(lines, nil); ok {
		t.Fatal("diffLineOps accepted oversized input")
	}
}

func diffTestLines(identities ...string) []diffSourceLine {
	lines := make([]diffSourceLine, len(identities))
	for i, identity := range identities {
		lines[i] = diffSourceLine{identity: identity, display: identity}
	}
	return lines
}

func diffOpKinds(ops []diffLineOp) string {
	kinds := make([]byte, len(ops))
	for i, op := range ops {
		kinds[i] = op.kind
	}
	return string(kinds)
}

func TestBuildVersionDiffRejectsExcessiveDepth(t *testing.T) {
	var source strings.Builder
	for range maxDiffDepth + 1 {
		source.WriteString("<div>")
	}
	for range maxDiffDepth + 1 {
		source.WriteString("</div>")
	}
	if _, err := buildVersionDiff(1, 2, source.String(), source.String()+"x"); err != errDiffLimit {
		t.Fatalf("error = %v; want diff limit", err)
	}
}

func TestParseDiffHTMLBoundsDeepLongTagPaths(t *testing.T) {
	tag := "x" + strings.Repeat("a", maxDiffTagBytes-1)
	var source strings.Builder
	source.Grow(5 << 20)
	for range maxDiffDepth {
		source.WriteByte('<')
		source.WriteString(tag)
		source.WriteByte('>')
	}
	for source.Len() < 5<<20 {
		source.WriteString("<!-- padding -->")
	}
	for range maxDiffDepth {
		source.WriteString("</")
		source.WriteString(tag)
		source.WriteByte('>')
	}
	htmlSource := source.String()

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := parseDiffHTML(htmlSource); err != errDiffLimit {
		t.Fatalf("error = %v; want diff limit", err)
	}
	runtime.ReadMemStats(&after)
	// Parse must materialize the tree before the path walk can reject it.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 16*uint64(len(htmlSource)) {
		t.Fatalf("parse allocated %d bytes for a %d-byte document before rejecting oversized paths", allocated, len(htmlSource))
	}
}

func TestParseDiffHTMLRejectsOversizedTagName(t *testing.T) {
	source := "<x" + strings.Repeat("a", maxDiffTagBytes) + "></x>"
	if _, err := parseDiffHTML(source); err != errDiffLimit {
		t.Fatalf("error = %v; want diff limit", err)
	}
}

func TestDiffMapsPathLimitToPayloadTooLarge(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	docs := NewDocService(store, store, NewCommentService(store, sluglock.NewMemory()), sluglock.NewMemory(), "", 5<<20)
	tag := "x" + strings.Repeat("a", maxDiffTagBytes-1)
	var source strings.Builder
	for range maxDiffDepth {
		source.WriteByte('<')
		source.WriteString(tag)
		source.WriteByte('>')
	}
	for range maxDiffDepth {
		source.WriteString("</")
		source.WriteString(tag)
		source.WriteByte('>')
	}
	for version := 1; version <= 2; version++ {
		value := source.String()
		if version == 2 {
			value += "x"
		}
		if _, err := store.PutDoc(ctx, "path-limit", version, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutMeta(ctx, "path-limit", storage.DocMeta{Slug: "path-limit", Versions: []storage.VersionRef{{N: 1}, {N: 2}}}); err != nil {
		t.Fatal(err)
	}

	_, err := docs.Diff(ctx, "path-limit", 1, 2)
	var appErr *apperr.Error
	if !errors.As(err, &appErr) || appErr.Status != 413 || appErr.Code != "diff_too_complex" {
		t.Fatalf("error = %#v; want 413 diff_too_complex", err)
	}
}

func TestMatchDiffNodesRejectsCumulativeComparisonBytes(t *testing.T) {
	before := []htmlDiffNode{{tag: "main", parent: -1, children: []int{1}}}
	before = append(before, htmlDiffNode{tag: "p", parent: 0, path: "/before", compareText: strings.Repeat("a", maxDiffCompareText)})
	after := []htmlDiffNode{{tag: "main", parent: -1}}
	for index := 0; index < maxDiffNodes-1; index++ {
		after = append(after, htmlDiffNode{
			tag:         "p",
			parent:      0,
			path:        "/after/" + string(rune(index+1)),
			compareText: strings.Repeat("b", maxDiffCompareText-8) + string(rune(index+1)),
		})
		after[0].children = append(after[0].children, index+1)
	}
	if _, err := matchDiffNodes(before, after); err != errDiffLimit {
		t.Fatalf("error = %v; want diff limit", err)
	}
}

func TestBuildVersionDiffBoundsLargeModifiedContainerSnippet(t *testing.T) {
	body := strings.Repeat("content", 30_000)
	before := `<html><body><main class="before">` + body + `</main></body></html>`
	after := `<html><body><main class="after">` + body + `</main></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 1 {
		t.Fatalf("summary = %+v", result.Summary)
	}
	change := result.Changes[0]
	if len(change.BeforeHTML) > maxDiffSnippetBytes || len(change.AfterHTML) > maxDiffSnippetBytes {
		t.Fatalf("snippet sizes = %d, %d", len(change.BeforeHTML), len(change.AfterHTML))
	}
	if strings.Contains(change.BeforeHTML, body[:10_000]) || strings.Contains(change.AfterHTML, body[:10_000]) {
		t.Fatal("large container body leaked into snippet")
	}
	if !strings.Contains(change.BeforeHTML, "omitted") || !strings.Contains(change.AfterHTML, "omitted") {
		t.Fatalf("snippets = %q / %q", change.BeforeHTML, change.AfterHTML)
	}
}

func TestBuildVersionDiffListHeadInsertionDoesNotCascade(t *testing.T) {
	before := `<html><body><ul><li>alpha</li><li>beta</li><li>gamma</li></ul></body></html>`
	after := `<html><body><ul><li>new</li><li>alpha</li><li>beta</li><li>gamma</li></ul></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Added != 1 || result.Summary.Modified != 0 || result.Summary.Removed != 0 {
		t.Fatalf("summary = %+v; changes = %+v", result.Summary, result.Changes)
	}
	if len(result.Changes) != 1 || result.Changes[0].AfterHTML != "<li>new</li>" {
		t.Fatalf("changes = %+v", result.Changes)
	}
}

func TestBuildVersionDiffDuplicateListHeadInsertionDoesNotCascade(t *testing.T) {
	before := `<html><body><ul><li>x</li><li>x</li></ul></body></html>`
	after := `<html><body><ul><li>new</li><li>x</li><li>x</li></ul></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Added != 1 || result.Summary.Modified != 0 || result.Summary.Removed != 0 {
		t.Fatalf("summary = %+v; changes = %+v", result.Summary, result.Changes)
	}
	if len(result.Changes) != 1 || result.Changes[0].AfterHTML != "<li>new</li>" {
		t.Fatalf("changes = %+v", result.Changes)
	}
}

func TestBuildVersionDiffPreservesScriptEntityLiterals(t *testing.T) {
	before := `<html><head><script>const marker = "&lt;";</script></head><body></body></html>`
	after := `<html><head><script>const marker = "<";</script></head><body></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 1 || result.Summary.Added != 0 || result.Summary.Removed != 0 {
		t.Fatalf("summary = %+v; changes = %+v", result.Summary, result.Changes)
	}
	if len(result.Changes) != 1 || result.Changes[0].DOMPath != "/html[1]/head[1]/script[1]" {
		t.Fatalf("changes = %+v", result.Changes)
	}
}

func TestBuildVersionDiffCodeHunksPreserveLiteralRawTextEntities(t *testing.T) {
	for _, tag := range []string{"script", "style"} {
		t.Run(tag, func(t *testing.T) {
			before := "<html><head><" + tag + `>value = "&amp;";</` + tag + "></head><body></body></html>"
			after := "<html><head><" + tag + `>value = "&";</` + tag + "></head><body></body></html>"

			result, err := buildVersionDiff(1, 2, before, after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.CodeHunks) != 1 {
				t.Fatalf("code hunks = %+v", result.CodeHunks)
			}
			lines := strings.Join(result.CodeHunks[0].Lines, "\n")
			if !strings.Contains(lines, `-value = "&amp;";`) || !strings.Contains(lines, `+value = "&";`) {
				t.Fatalf("code hunk lost raw-text difference:\n%s", lines)
			}
		})
	}
}

func TestParseDiffHTMLBoundsCommentSeparatedTextStorage(t *testing.T) {
	var source strings.Builder
	source.Grow(5 << 20)
	source.WriteString("<main>")
	for source.Len() < 5<<20 {
		source.WriteString("x<!-- separator -->")
	}
	source.WriteString("</main>")

	nodes, err := parseDiffHTML(source.String())
	if err != nil {
		t.Fatal(err)
	}
	main := diffMainNode(t, nodes)
	if len(main.text) > maxDiffCompareText || len(main.compareText) > maxDiffCompareText {
		t.Fatalf("stored text sizes = %d, %d", len(main.text), len(main.compareText))
	}
}

// diffMainNode skips synthetic document wrappers.
func diffMainNode(t *testing.T, nodes []htmlDiffNode) htmlDiffNode {
	t.Helper()
	if len(nodes) != 4 {
		t.Fatalf("nodes = %d; want html/head/body/main", len(nodes))
	}
	if nodes[3].tag != "main" {
		t.Fatalf("nodes[3].tag = %q; want main", nodes[3].tag)
	}
	return nodes[3]
}

func TestParseDiffHTMLCommentSeparatedTextAllocationsStayLinear(t *testing.T) {
	var source strings.Builder
	source.Grow(1 << 20)
	source.WriteString("<main>")
	for source.Len() < 1<<20 {
		source.WriteString("x<!-- separator -->")
	}
	source.WriteString("</main>")

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	nodes, err := parseDiffHTML(source.String())
	if err != nil {
		t.Fatal(err)
	}
	diffMainNode(t, nodes)
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("parse allocated %d bytes for %d-byte input", allocated, source.Len())
	}
}

func TestParseDiffHTMLFiveMiBCommentSeparatedTextAllocationAndResult(t *testing.T) {
	var source strings.Builder
	source.Grow(5 << 20)
	source.WriteString("<main>")
	for source.Len() < 5<<20 {
		source.WriteString("x<!-- separator -->")
	}
	source.WriteString("</main>")

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	nodes, err := parseDiffHTML(source.String())
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	diffMainNode(t, nodes)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("5MiB comment-separated parse: input=%d nodes=%d allocated=%d", source.Len(), len(nodes), allocated)
	if allocated > 256<<20 {
		t.Fatalf("parse allocated %d bytes for %d-byte input", allocated, source.Len())
	}
}

func TestBuildVersionDiffDetectsCommentChangePastDisplayLimit(t *testing.T) {
	prefix := strings.Repeat("z", 4000)
	before := "<html><body><p>hi</p><!-- " + prefix + "ALPHA --></body></html>"
	after := "<html><body><p>hi</p><!-- " + prefix + "OMEGA --></body></html>"

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CodeHunks) == 0 {
		t.Fatal("comment-only change produced no code hunk")
	}
}

func TestBuildVersionDiffCodeHunksDetectLongTextTailChange(t *testing.T) {
	prefix := strings.Repeat("a", 6000)
	before := "<html><body><p>" + prefix + "ALPHA</p></body></html>"
	after := "<html><body><p>" + prefix + "OMEGA</p></body></html>"

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 1 || len(result.CodeHunks) == 0 {
		t.Fatalf("summary = %+v; code hunks = %+v", result.Summary, result.CodeHunks)
	}
}

func TestBuildVersionDiffCommentDoesNotChangeVisibleText(t *testing.T) {
	before := `<html><body><p>ab</p></body></html>`
	after := `<html><body><p>a<!-- comment -->b</p></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 {
		t.Fatalf("comment changed structural text: %+v", result.Changes)
	}
	if len(result.CodeHunks) == 0 {
		t.Fatal("comment source change produced no code hunk")
	}
}

func TestBuildVersionDiffDoesNotJoinEntityAcrossComment(t *testing.T) {
	before := `<html><body><p>&am<!--x-->p;</p></body></html>`
	after := `<html><body><p>&amp;</p></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 {
		t.Fatalf("split entity was treated as equal: %+v", result)
	}
}

func TestBuildVersionDiffPreservesTextAtChildBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
	}{
		{"visible whitespace", `<p>see <a>here</a></p>`, `<p>see<a>here</a></p>`},
		{"redistributed text", `<p>abc<b>X</b>def</p>`, `<p>ab<b>X</b>cdef</p>`},
		{"leading first child", `<p>a<b>x</b></p>`, `<p><b>x</b>a</p>`},
		{"trailing last child", `<p><b>x</b>a</p>`, `<p><b>x</b></p>a`},
		{"adjacent children boundary", `<p><b>x</b><i>y</i></p>`, `<p>z<b>x</b><i>y</i></p>`},
		{"void child boundary", `<p>a<br>b</p>`, `<p>ab<br></p>`},
		{"reviewer repro one", `<p>a<b>x</b><i>y</i>b</p>`, `<p><b>x</b>a<i>y</i>b</p>`},
		{"reviewer repro two", `<p>a<b>x</b>b<i>y</i></p>`, `<p>a<b>x</b><i>y</i>b</p>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Modified == 0 || len(result.Changes) == 0 {
				t.Fatalf("child-boundary edit was equal: %+v", result)
			}
		})
	}
}

func TestBuildVersionDiffTreatsLiteralNBSPAsDistinctFromASCIISpace(t *testing.T) {
	// HTML only folds ASCII whitespace, so a literal U+00A0 must not compare
	// equal to a plain space in the text summary; the edit is a real change.
	before := "<html><body><p>a\u00a0b</p></body></html>"
	after := "<html><body><p>a b</p></body></html>"

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified == 0 || len(result.Changes) == 0 {
		t.Fatalf("NBSP vs ASCII space was treated as equal: %+v", result)
	}

	// The same visible NBSP text must stay equal to itself (no spurious change).
	same := "<html><body><p>a\u00a0b</p></body></html>"
	result, err = buildVersionDiff(1, 2, same, same)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 0 || len(result.Changes) != 0 {
		t.Fatalf("identical NBSP text reported a change: %+v", result)
	}
}

func TestBuildVersionDiffAllowsManyOrdinaryEdits(t *testing.T) {
	var before, after strings.Builder
	before.WriteString("<html><body>")
	after.WriteString("<html><body>")
	for index := range 100 {
		fmt.Fprintf(&before, "<p>paragraph %03d has the original ordinary text.</p>", index)
		fmt.Fprintf(&after, "<p>paragraph %03d has the revised ordinary text.</p>", index)
	}
	before.WriteString("</body></html>")
	after.WriteString("</body></html>")

	result, err := buildVersionDiff(1, 2, before.String(), after.String())
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 100 || len(result.CodeHunks) == 0 {
		t.Fatalf("unexpected diff: summary=%+v hunks=%d", result.Summary, len(result.CodeHunks))
	}
}

func TestParseDiffHTMLBoundsMultipleRawTextElements(t *testing.T) {
	payload := strings.Repeat("A", 256<<10)
	var source strings.Builder
	source.WriteString("<html><head>")
	for range 8 {
		source.WriteString("<style>")
		source.WriteString(payload)
		source.WriteString("</STYLE><script>")
		source.WriteString(payload)
		source.WriteString("</SCRIPT>")
	}
	source.WriteString("</head><body><p>after raw text</p></body></html>")

	nodes, err := parseDiffHTML(source.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 20 {
		t.Fatalf("nodes = %d", len(nodes))
	}
	for _, node := range nodes {
		if len(node.text) > maxDiffCompareText || len(node.compareText) > maxDiffCompareText {
			t.Fatalf("%s stored text sizes = %d, %d", node.tag, len(node.text), len(node.compareText))
		}
	}
	if nodes[len(nodes)-1].tag != "p" || nodes[len(nodes)-1].text != "after raw text" {
		t.Fatalf("last node = %+v", nodes[len(nodes)-1])
	}
}

func TestBuildVersionDiffParsesAfterRawTextCloseSlash(t *testing.T) {
	before := `<html><body><script>const value = 1;</script/><p>before</p></body></html>`
	after := `<html><body><script>const value = 1;</script/><p>after</p></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 1 || result.Summary.Added != 0 || result.Summary.Removed != 0 {
		t.Fatalf("summary = %+v; changes = %+v", result.Summary, result.Changes)
	}
	if len(result.Changes) != 1 || result.Changes[0].DOMPath != "/html[1]/body[1]/p[1]" {
		t.Fatalf("changes = %+v", result.Changes)
	}
}

func TestBuildVersionDiffRejectsExcessiveNodeCount(t *testing.T) {
	var source strings.Builder
	source.WriteString("<html><body>")
	for range maxDiffNodes + 1 {
		source.WriteString("<i></i>")
	}
	source.WriteString("</body></html>")
	if _, err := buildVersionDiff(1, 2, source.String(), source.String()+"x"); err != errDiffLimit {
		t.Fatalf("error = %v; want diff limit", err)
	}
}

// reindentEqual builds a compact and a pretty-printed variant of the same block
// content and asserts the pair stays a bounded reindent: ordinary reindent of
// plain block/table/list elements produces no structural changes. Since the
// document-level whitespace fingerprint is now built for every document (it is
// no longer gated on a CSS heuristic, so no unknown/dynamic style can
// double-blind a whitespace-layout change), a compact-vs-pretty reindent whose
// inter-tag whitespace layout differs surfaces as exactly one bounded synthetic
// "[formatting whitespace changed]" hunk line. That single record is accepted
// (the old zero-hunk contract is superseded); it must never 413, never grow with
// document size, and never leak the internal digest/token.
func reindentEqual(t *testing.T, compact, pretty string) {
	t.Helper()
	result, err := buildVersionDiff(1, 2, compact, pretty)
	if err != nil {
		t.Fatalf("reindent returned error (413?): %v", err)
	}
	if len(result.Changes) != 0 {
		t.Fatalf("reindent produced structural changes: %d", len(result.Changes))
	}
	total := 0
	for _, h := range result.CodeHunks {
		total += len(h.Lines)
	}
	if total > reindentMaxHunkLines {
		t.Fatalf("reindent produced %d hunk lines; want a small bounded hunk", total)
	}
	if total > 0 {
		body := hunksBody(result.CodeHunks)
		if !strings.Contains(body, "[formatting whitespace changed]") {
			t.Fatalf("reindent hunk is not the whitespace record:\n%s", body)
		}
		assertNoLeakedInternals(t, result.CodeHunks)
	}
}

// reindentMaxHunkLines bounds the synthetic whitespace hunk a reindent may emit;
// it is a small fixed constant that does not grow with document size.
const reindentMaxHunkLines = 16

func TestBuildVersionDiffPlainReindentIsZeroNoise(t *testing.T) {
	for _, count := range []int{100, 500} {
		t.Run("p-"+strconv.Itoa(count), func(t *testing.T) {
			var compact, pretty strings.Builder
			for range count {
				compact.WriteString("<p>x</p>")
				pretty.WriteString("  <p>x</p>\n")
			}
			reindentEqual(t, "<main>"+compact.String()+"</main>", "<main>\n"+pretty.String()+"</main>")
		})
	}
	reindentEqual(t,
		`<table><tbody><tr><td>a</td><td>b</td></tr></tbody></table>`,
		"<table>\n  <tbody>\n    <tr>\n      <td>a</td>\n      <td>b</td>\n    </tr>\n  </tbody>\n</table>")
	reindentEqual(t,
		`<ul><li>a</li><li>b</li></ul>`,
		"<ul>\n  <li>a</li>\n  <li>b</li>\n</ul>")
	reindentEqual(t,
		`<div><section><p>a</p><p>b</p></section></div>`,
		"<div>\n  <section>\n    <p>a</p>\n    <p>b</p>\n  </section>\n</div>")
}

// The four final-review repros: a whitespace change that could be visible must
// surface in at least one layer, never an empty diff (Goal B).
func TestBuildVersionDiffWhitespaceSensitiveContextsAreNotEmptyDiffs(t *testing.T) {
	cases := []struct {
		name          string
		before, after string
	}{
		{"pre-block-children", `<pre><div>x</div><div>y</div></pre>`, `<pre><div>x</div> <div>y</div></pre>`},
		{"ancestor-white-space-pre", `<div style="white-space:pre"><span>x</span><span>y</span></div>`, `<div style="white-space:pre"><span>x</span> <span>y</span></div>`},
		{"style-display-inline", `<style>div{display:inline}</style><div>x</div><div>y</div>`, `<style>div{display:inline}</style><div>x</div> <div>y</div>`},
		{"summary-display-inline", `<summary style="display:inline">x</summary><summary style="display:inline">y</summary>`, `<summary style="display:inline">x</summary> <summary style="display:inline">y</summary>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, tc.before, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
				t.Fatalf("whitespace change vanished: changes=0 hunks=0 for %q vs %q", tc.before, tc.after)
			}
		})
	}
}

func TestBuildVersionDiffPreformattedRunLengthChangeIsVisible(t *testing.T) {
	before := "<pre>a b</pre>"
	after := "<pre>a  b</pre>"
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CodeHunks) == 0 {
		t.Fatal("differing whitespace-run length under <pre> produced no code hunk")
	}
}

// assertVisibleAndValid fails when a change that must surface vanishes from both
// layers, and verifies every emitted hunk line stays bounded and UTF-8 valid.
func assertVisibleAndValid(t *testing.T, before, after string) {
	t.Helper()
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
		t.Fatalf("change vanished: changes=0 hunks=0 for %q vs %q", before, after)
	}
	const maxLineBytes = 1 << 12
	for _, hunk := range result.CodeHunks {
		for _, line := range hunk.Lines {
			if !utf8.ValidString(line) {
				t.Fatalf("hunk line is not valid UTF-8: %q", line)
			}
			if len(line) > maxLineBytes {
				t.Fatalf("hunk line unbounded: %d bytes", len(line))
			}
		}
	}
}

// Raw-text (RCDATA/literal) content changes must surface, including when the
// content holds a stray '<' that is text, not markup: the parser must scan to
// the close tag rather than splitting the run into a spurious tag.
func TestBuildVersionDiffRawTextSourceWhitespaceIsBytePreserving(t *testing.T) {
	for _, tag := range []string{"script", "style", "textarea", "title", "pre"} {
		for _, change := range []struct {
			name          string
			before, after string
		}{
			{name: "space", before: "a b", after: "a  b"},
			{name: "tab", before: "a b", after: "a	b"},
			{name: "newline", before: "a b", after: "a\nb"},
		} {
			t.Run(tag+"_"+change.name, func(t *testing.T) {
				before := "<" + tag + ">" + change.before + "</" + tag + ">"
				after := "<" + tag + ">" + change.after + "</" + tag + ">"
				result, err := buildVersionDiff(1, 2, before, after)
				if err != nil {
					t.Fatal(err)
				}
				if len(result.CodeHunks) == 0 {
					t.Fatalf("%s source whitespace change missing from code hunks: %+v", tag, result)
				}
				body := hunksBody(result.CodeHunks)
				if !strings.Contains(body, "-"+change.before) || !strings.Contains(body, "+"+change.after) {
					t.Fatalf("%s source bytes missing from code hunks:\n%s", tag, body)
				}
			})
		}
	}
}

func TestBuildVersionDiffRawTextWhitespaceAndStrayLtAreVisible(t *testing.T) {
	assertStructuralChange := func(before, after string) {
		t.Helper()
		result, err := buildVersionDiff(1, 2, before, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Changes) == 0 || result.Summary.Modified == 0 {
			t.Fatalf("preformatted whitespace change missing from structural diff: %+v", result)
		}
	}
	assertStructuralChange("<pre>a b</pre>", "<pre>a  b</pre>")
	assertStructuralChange("<pre><code>if x:\n  y\n</code></pre>", "<pre><code>if x:\n    y\n</code></pre>")
	assertStructuralChange("<textarea>a b</textarea>", "<textarea>a  b</textarea>")
	assertStructuralChange(`<div style="white-space: pre">a b</div>`, `<div style="white-space: pre">a  b</div>`)
	assertStructuralChange(`<div style="white-space: normal; white-space: pre">a b</div>`, `<div style="white-space: normal; white-space: pre">a  b</div>`)
	assertStructuralChange(`<div style="white-space: pre !important; white-space: normal">a b</div>`, `<div style="white-space: pre !important; white-space: normal">a  b</div>`)

	collapsed, err := buildVersionDiff(1, 2, "<div>a b</div>", "<div>a  b</div>")
	if err != nil {
		t.Fatal(err)
	}
	if len(collapsed.Changes) != 0 || collapsed.Summary.Modified != 0 {
		t.Fatalf("collapsible whitespace became structural: %+v", collapsed)
	}
	collapsedCases := []struct {
		name          string
		before, after string
		rawText       bool
	}{
		{"last_declaration_wins", `<div style="white-space: pre; white-space: normal">a b</div>`, `<div style="white-space: pre; white-space: normal">a  b</div>`, false},
		{"important_wins", `<div style="white-space: normal !important; white-space: pre">a b</div>`, `<div style="white-space: normal !important; white-space: pre">a  b</div>`, false},
		{"descendant_override", `<pre><code style="white-space: normal">a b</code></pre>`, `<pre><code style="white-space: normal">a  b</code></pre>`, false},
		{"self_override", `<pre style="white-space: normal">a b</pre>`, `<pre style="white-space: normal">a  b</pre>`, false},
		{"textarea_self_override", `<textarea style="white-space: normal">a b</textarea>`, `<textarea style="white-space: normal">a  b</textarea>`, true},
	}
	for _, test := range collapsedCases {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) != 0 || result.Summary.Modified != 0 {
				t.Fatalf("CSS-collapsible whitespace became structural: %+v", result)
			}
			if test.rawText {
				if len(result.CodeHunks) == 0 {
					t.Fatalf("raw source whitespace missing from code hunks: %+v", result)
				}
				return
			}
			if len(result.CodeHunks) != 0 {
				t.Fatalf("collapsible ordinary text surfaced in code hunks: %+v", result)
			}
		})
	}

	assertVisibleAndValid(t, "<textarea>a < b</textarea>", "<textarea>a <  b</textarea>")
	assertVisibleAndValid(t, "<title>a &amp; b</title>", "<title>a &amp;&amp; b</title>")
	// Identical raw-text content still yields no diff.
	ident, err := buildVersionDiff(1, 2, "<textarea>a < b</textarea>", "<textarea>a < b</textarea>")
	if err != nil {
		t.Fatal(err)
	}
	if len(ident.Changes) != 0 || len(ident.CodeHunks) != 0 {
		t.Fatalf("identical raw text produced a diff: changes=%d hunks=%d", len(ident.Changes), len(ident.CodeHunks))
	}
}

// Long content that shares its first >1024 bytes but diverges in the tail must
// not collide once display truncation applies; the full-content digest keeps
// distinct text distinct across pre, white-space:pre, and pure-whitespace runs.
func TestBuildVersionDiffLongTailChangesDoNotCollide(t *testing.T) {
	prefix := strings.Repeat("x", 1500)
	assertVisibleAndValid(t, "<pre>"+prefix+"ALPHA</pre>", "<pre>"+prefix+"OMEGA</pre>")

	long := strings.Repeat("word ", 400) // > 1024 bytes after collapse
	assertVisibleAndValid(t,
		`<div style="white-space:pre">`+long+"ALPHA</div>",
		`<div style="white-space:pre">`+long+"OMEGA</div>")

	// Pure-whitespace runs differing only in length or tail beyond 1024 must not
	// collide via the whitespace-run token.
	ws := strings.Repeat(" ", 1400)
	assertVisibleAndValid(t,
		`<div style="white-space:pre"><span>x</span>`+ws+"<span>y</span></div>",
		`<div style="white-space:pre"><span>x</span>`+ws+" <span>y</span></div>")
	assertVisibleAndValid(t,
		`<pre><span>x</span>`+strings.Repeat(" ", 1400)+"a<span>y</span></pre>",
		`<pre><span>x</span>`+strings.Repeat(" ", 1400)+"b<span>y</span></pre>")
}

func TestBuildVersionDiffDeterministicWhitespaceHunks(t *testing.T) {
	before := `<style>x{}</style><div>a</div><div>b</div>`
	after := `<style>x{}</style><div>a</div> <div>b</div>`
	first, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	for range 5 {
		next, err := buildVersionDiff(1, 2, before, after)
		if err != nil {
			t.Fatal(err)
		}
		nextJSON, _ := json.Marshal(next)
		if string(firstJSON) != string(nextJSON) {
			t.Fatalf("nondeterministic diff:\n%s\n%s", firstJSON, nextJSON)
		}
	}
}

func TestVersionDiffReviewRegressions(t *testing.T) {
	t.Run("spaced style equals", func(t *testing.T) {
		before := `<html><body><div style = "display:inline">x</div><div style = "display:inline">y</div></body></html>`
		after := `<html><body><div style = "display:inline">x</div> <div style = "display:inline">y</div></body></html>`
		assertVisibleAndValid(t, before, after)
	})

	t.Run("200 row table", func(t *testing.T) {
		before, after := tableDocuments(200, 3)
		assertVisibleAndValid(t, before, after)
	})

	for _, count := range []int{200, 800} {
		t.Run(fmt.Sprintf("nested_%d", count), func(t *testing.T) {
			before, after := nestedDocuments(count)
			assertVisibleAndValid(t, before, after)
		})
	}
	t.Run("nested 200 identical", func(t *testing.T) {
		before, _ := nestedDocuments(200)
		result, err := buildVersionDiff(1, 2, before, before)
		if err != nil || len(result.Changes) != 0 || len(result.CodeHunks) != 0 {
			t.Fatalf("identical nested document: result=%+v err=%v", result, err)
		}
	})

	t.Run("456 to 100", func(t *testing.T) {
		before := repeatedParagraphs(456)
		after := repeatedParagraphs(100)
		if _, err := buildVersionDiff(1, 2, before, after); err != nil {
			t.Fatal(err)
		}
	})

	for _, count := range []int{500, 1500} {
		t.Run(fmt.Sprintf("style_reindent_%d", count), func(t *testing.T) {
			compact := `<style>p{color:red}</style>` + repeatedParagraphs(count)
			pretty := strings.ReplaceAll(compact, "</p>", "</p>\n  ")
			result, err := buildVersionDiff(1, 2, compact, pretty)
			if err != nil {
				t.Fatal(err)
			}
			// Under layout-affecting CSS (<style>), the reindent changes the
			// inter-tag whitespace layout, which could render visibly, so it must
			// NOT be a double-blind empty diff. The document-level whitespace
			// fingerprint surfaces it as a single, bounded hunk that never grows
			// with the element count and never 413s (the old zero-hunk contract was
			// the double-blind this PR fixes; zero-noise is not required here).
			if len(result.CodeHunks) == 0 {
				t.Fatalf("layout-CSS reindent vanished (double-blind): 0 hunks for count=%d", count)
			}
			total := 0
			for _, h := range result.CodeHunks {
				total += len(h.Lines)
			}
			if total > 32 {
				t.Fatalf("layout-CSS reindent produced %d hunk lines for count=%d; want a small bounded hunk", total, count)
			}
			assertNoLeakedInternals(t, result.CodeHunks)
		})
	}

	t.Run("public lines hide identity", func(t *testing.T) {
		result, err := buildVersionDiff(1, 2, `<pre>`+strings.Repeat("a", 1500)+`x</pre>`, `<pre>`+strings.Repeat("a", 1500)+`y</pre>`)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result.CodeHunks)
		if strings.Contains(string(encoded), "␈ws") || strings.Contains(string(encoded), "␈sha256") {
			t.Fatalf("internal identity leaked: %s", encoded)
		}
	})
}

func tableDocuments(rows, cells int) (string, string) {
	var before, after strings.Builder
	before.WriteString("<table>")
	after.WriteString("<table>")
	for row := range rows {
		before.WriteString("<tr>")
		after.WriteString("<tr>")
		for cell := range cells {
			fmt.Fprintf(&before, "<td>%d-%d</td>", row, cell)
			value := fmt.Sprintf("%d-%d", row, cell)
			if row == rows/2 && cell == cells/2 {
				value = "changed"
			}
			fmt.Fprintf(&after, "<td>%s</td>", value)
		}
		before.WriteString("</tr>")
		after.WriteString("</tr>")
	}
	before.WriteString("</table>")
	after.WriteString("</table>")
	return before.String(), after.String()
}

func nestedDocuments(count int) (string, string) {
	var before, after strings.Builder
	for index := range count {
		fmt.Fprintf(&before, "<div><p>%d</p><p>x</p></div>", index)
		value := "x"
		if index == count/2 {
			value = "changed"
		}
		fmt.Fprintf(&after, "<div><p>%d</p><p>%s</p></div>", index, value)
	}
	return before.String(), after.String()
}

func repeatedParagraphs(count int) string {
	var result strings.Builder
	for range count {
		result.WriteString("<p>x</p>")
	}
	return result.String()
}

// TestNormalizedLineIdentityIsBoundedAndDistinct pins finding P1(2): a source
// line's diff identity is a small fixed-width key (kind + canonical byte length
// + SHA-256), never the canonical text itself, so line-diff memory stays linear
// regardless of line length; distinct canonical content never collides even
// when the display is truncated.
func TestNormalizedLineIdentityIsBoundedAndDistinct(t *testing.T) {
	const identityBound = 128
	huge := newDiffSourceLine("text", strings.Repeat("z", 200_000), strings.Repeat("z", 200_000))
	if len(huge.identity) >= identityBound {
		t.Fatalf("identity length %d exceeds bound %d", len(huge.identity), identityBound)
	}
	if len(huge.display) > 1<<12 {
		t.Fatalf("display length %d unbounded", len(huge.display))
	}
	if !utf8.ValidString(huge.display) {
		t.Fatal("display not valid UTF-8")
	}
	// Long tails sharing a truncated display prefix keep distinct identity.
	prefix := strings.Repeat("a", 4000)
	if newDiffSourceLine("text", prefix+"ALPHA", "").identity == newDiffSourceLine("text", prefix+"OMEGA", "").identity {
		t.Fatal("distinct long-tail canonical content collided in identity")
	}
	// The kind participates in identity so a tag line never equals a text line
	// with the same bytes.
	if newDiffSourceLine("tag", "x", "x").identity == newDiffSourceLine("text", "x", "x").identity {
		t.Fatal("distinct kinds collided in identity")
	}
}

// TestNormalizedHTMLLinesMemoryStaysLinear pins the P1(2) memory contract: a
// large document with a few very long lines yields per-line identities of low
// constant size, so total allocation stays a small multiple of the input rather
// than scaling with line length.
func TestNormalizedHTMLLinesMemoryStaysLinear(t *testing.T) {
	var source strings.Builder
	source.Grow(5 << 20)
	source.WriteString("<main>")
	for source.Len() < 5<<20 {
		source.WriteString("<p>" + strings.Repeat("word ", 20000) + "</p>")
	}
	source.WriteString("</main>")

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	lines, ok := normalizedHTMLLines(source.String())
	if !ok {
		t.Fatal("normalizedHTMLLines rejected a bounded 5MiB input")
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64<<20 {
		t.Fatalf("normalization allocated %d bytes for %d-byte input", allocated, source.Len())
	}
	for _, line := range lines {
		if len(line.identity) >= 128 {
			t.Fatalf("identity length %d exceeds bound", len(line.identity))
		}
	}
}

// TestBuildVersionDiffInlineNewlineWhitespaceSurfaces pins finding P0: whitespace
// that can render as a visible space must surface even when it is a newline run
// (not just a lone space) — an inline neighbour (default, unknown/custom, or an
// inline display override) makes the run significant regardless of its bytes.
func TestBuildVersionDiffInlineNewlineWhitespaceSurfaces(t *testing.T) {
	cases := []struct{ name, before, after string }{
		{"span-newline", `<span>x</span><span>y</span>`, "<span>x</span>\n<span>y</span>"},
		{"display-inline-newline",
			`<span style="display:inline">x</span><span style="display:inline">y</span>`,
			"<span style=\"display:inline\">x</span>\n<span style=\"display:inline\">y</span>"},
		{"display-inline-space-eqspace",
			`<summary style = "display:inline">x</summary><summary style = "display:inline">y</summary>`,
			`<summary style = "display:inline">x</summary> <summary style = "display:inline">y</summary>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, tc.before, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
				t.Fatalf("inline whitespace change vanished: %q vs %q", tc.before, tc.after)
			}
		})
	}
}

// TestBuildVersionDiffStyleReindentDoesNotOverflow pins the P0 no-413 contract:
// a large newline-based reindent of plain block elements stays ignorable even
// when a document-level <style> is present, so it never overflows the hunk
// budget with a per-element whitespace line.
func TestBuildVersionDiffStyleReindentDoesNotOverflow(t *testing.T) {
	for _, count := range []int{500, 1500} {
		compact := `<style>p{color:red}</style>` + repeatedParagraphs(count)
		pretty := strings.ReplaceAll(compact, "</p>", "</p>\n  ")
		if _, err := buildVersionDiff(1, 2, compact, pretty); err != nil {
			t.Fatalf("count=%d: style reindent overflowed: %v", count, err)
		}
	}
}

// TestSiblingOrderBoundsRefreshReflectsNewMatches pins finding P1(3): a
// per-parent bounds refresh must reflect matches added after the initial
// snapshot, so an anchor established mid-pass is honoured. A stale bound would
// permit a crossing pairing; refreshParent must update lower/upper for the
// parent's children from the current matches.
func TestSiblingOrderBoundsRefreshReflectsNewMatches(t *testing.T) {
	// parent 0 with three children 1,2,3.
	before := []htmlDiffNode{
		{tag: "ul", parent: -1, children: []int{1, 2, 3}},
		{tag: "li", parent: 0, siblingPos: 0},
		{tag: "li", parent: 0, siblingPos: 1},
		{tag: "li", parent: 0, siblingPos: 2},
	}
	after := make([]htmlDiffNode, 4)
	matches := map[int]int{}
	bounds := newSiblingOrderBounds(before, after, matches)
	// With no matches, no child has an anchoring sibling in either direction
	// (lower/upper < 0 means "no anchor" per compatible()).
	if bounds.lower[3] >= 0 || bounds.upper[1] >= 0 {
		t.Fatalf("initial bounds wrong: lower[3]=%d upper[1]=%d", bounds.lower[3], bounds.upper[1])
	}
	// Establish middle child 2 as an anchor, then refresh: child 1 must gain an
	// upper anchor and child 3 a lower anchor reflecting the new match.
	matches[2] = 7
	bounds.refreshParent(before, 0, matches)
	if bounds.upper[1] != 7 {
		t.Fatalf("stale upper bound after refresh: upper[1]=%d want 7", bounds.upper[1])
	}
	if bounds.lower[3] != 7 {
		t.Fatalf("stale lower bound after refresh: lower[3]=%d want 7", bounds.lower[3])
	}
}

// TestMatchDiffNodesNewAnchorDoesNotCrossExisting pins P1(3) end-to-end: a
// heuristic (non-AID) sibling match must respect committed sibling anchors and
// never cross one. Two AID-stable list items anchor the ends and an unanchored
// middle item changes text; it must map within the anchors as one modified,
// never a stale-bound crossing that yields spurious add/remove.
func TestMatchDiffNodesNewAnchorDoesNotCrossExisting(t *testing.T) {
	before := `<html><body><ul>` +
		`<li data-odoc-aid="a">first</li>` +
		`<li>middle old</li>` +
		`<li data-odoc-aid="c">last</li>` +
		`</ul></body></html>`
	after := `<html><body><ul>` +
		`<li data-odoc-aid="a">first</li>` +
		`<li>middle new</li>` +
		`<li data-odoc-aid="c">last</li>` +
		`</ul></body></html>`

	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 1 || result.Summary.Added != 0 || result.Summary.Removed != 0 {
		t.Fatalf("summary = %+v; want exactly one modified", result.Summary)
	}
}

// TestMatchDiffNodesLargeSiblingSetStaysBounded keeps the P1(3) performance
// contract: incremental per-parent bounds refresh after anchoring must not
// degrade to O(nodes×parents). A wide reordered same-tag AID sibling set must
// match within the comparison budget.
func TestMatchDiffNodesLargeSiblingSetStaysBounded(t *testing.T) {
	const n = 1500
	var before, after strings.Builder
	before.WriteString("<main>")
	after.WriteString("<main>")
	for i := 0; i < n; i++ {
		before.WriteString(`<span data-odoc-aid="s` + strconv.Itoa(i) + `">x</span>`)
	}
	for i := n - 1; i >= 0; i-- {
		after.WriteString(`<span data-odoc-aid="s` + strconv.Itoa(i) + `">x</span>`)
	}
	before.WriteString("</main>")
	after.WriteString("</main>")

	beforeNodes, err := parseDiffHTML(before.String())
	if err != nil {
		t.Fatal(err)
	}
	afterNodes, err := parseDiffHTML(after.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := matchDiffNodes(beforeNodes, afterNodes); err != nil {
		t.Fatalf("large reordered AID sibling match failed: %v", err)
	}
}

// hunksBody joins every emitted hunk line so a test can assert user-visible
// content (and the absence of any internal digest/token) in one string.
func hunksBody(hunks []CodeHunk) string {
	var b strings.Builder
	for _, h := range hunks {
		for _, line := range h.Lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// assertNoLeakedInternals fails if any hunk line exposes the internal whitespace
// fingerprint digest or its identity kind/domain token. Public hunks must carry
// only the bounded human-readable marker, never a hash or internal key.
func assertNoLeakedInternals(t *testing.T, hunks []CodeHunk) {
	t.Helper()
	body := hunksBody(hunks)
	for _, needle := range []string{"ws-doc:", "ws-fingerprint", "odoc-ws-fingerprint", "sha256:"} {
		if strings.Contains(body, needle) {
			t.Fatalf("public hunk leaked internal token %q in:\n%s", needle, body)
		}
	}
}

// TestBuildVersionDiffLayoutCSSBlockNewlineWhitespaceSurfaces pins the PR #26 P0:
// under layout-affecting CSS, an inter-tag whitespace change between two
// otherwise-block boundaries — including a pure newline — must never yield a
// double-blind empty diff. Both the newline and the space form of the repro must
// surface as a single, bounded, comprehensible whitespace record with no digest
// leaked, while the per-line block-newline reindent stays otherwise ignored.
func TestBuildVersionDiffLayoutCSSBlockNewlineWhitespaceSurfaces(t *testing.T) {
	base := `<style>p{display:inline}</style><p>a</p>`
	cases := []struct {
		name  string
		after string
	}{
		{"newline-between-blocks", base + "\n" + `<p>b</p>`},
		{"space-between-blocks", base + ` ` + `<p>b</p>`},
	}
	before := base + `<p>b</p>`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, before, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
				t.Fatalf("layout-CSS whitespace change vanished (double-blind) for %q vs %q", before, tc.after)
			}
			body := hunksBody(result.CodeHunks)
			if !strings.Contains(body, "[formatting whitespace changed]") {
				t.Fatalf("expected comprehensible whitespace marker; hunks:\n%s", body)
			}
			assertNoLeakedInternals(t, result.CodeHunks)
			total := 0
			for _, h := range result.CodeHunks {
				total += len(h.Lines)
			}
			if total > 16 {
				t.Fatalf("whitespace record produced %d hunk lines; want a small bounded hunk", total)
			}
		})
	}
}

// TestBuildVersionDiffLayoutCSSWhiteSpacePreBlockNewline pins that a block-level
// newline under a white-space:pre style surfaces rather than vanishing.
func TestBuildVersionDiffLayoutCSSWhiteSpacePreBlockNewline(t *testing.T) {
	before := `<div style="white-space:pre"><span>x</span><span>y</span></div>`
	after := `<div style="white-space:pre"><span>x</span>` + "\n" + `<span>y</span></div>`
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
		t.Fatalf("white-space:pre block newline vanished for %q vs %q", before, after)
	}
	assertNoLeakedInternals(t, result.CodeHunks)
}

// TestBuildVersionDiffStylesheetLinkBlockNewline pins that a rel="stylesheet"
// link makes an inter-tag block newline change surface rather than double-blind.
func TestBuildVersionDiffStylesheetLinkBlockNewline(t *testing.T) {
	before := `<link rel="stylesheet" href="a.css"><div>a</div><div>b</div>`
	after := `<link rel="stylesheet" href="a.css"><div>a</div>` + "\n" + `<div>b</div>`
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
		t.Fatalf("stylesheet-link block newline vanished for %q vs %q", before, after)
	}
	assertNoLeakedInternals(t, result.CodeHunks)
}

// TestBuildVersionDiffLayoutCSSLargeReindentStaysBounded pins that a large pretty
// reindent under a <style> block does not 413 and, when it surfaces a whitespace
// change, stays a small bounded hunk (a fixed small constant) independent of
// document size (500 and 1500 elements).
func TestBuildVersionDiffLayoutCSSLargeReindentStaysBounded(t *testing.T) {
	const maxHunkLinesBound = 32
	for _, count := range []int{500, 1500} {
		t.Run("n-"+strconv.Itoa(count), func(t *testing.T) {
			var compact, pretty strings.Builder
			compact.WriteString(`<style>p{color:red}</style><div>`)
			pretty.WriteString(`<style>p{color:red}</style>` + "\n<div>\n")
			for range count {
				compact.WriteString("<p>x</p>")
				pretty.WriteString("  <p>x</p>\n")
			}
			compact.WriteString("</div>")
			pretty.WriteString("</div>")
			result, err := buildVersionDiff(1, 2, compact.String(), pretty.String())
			if err != nil {
				t.Fatalf("large reindent under <style> returned error (413?): %v", err)
			}
			total := 0
			for _, h := range result.CodeHunks {
				total += len(h.Lines)
			}
			if total > maxHunkLinesBound {
				t.Fatalf("reindent produced %d hunk lines for %d elements; want <= %d", total, count, maxHunkLinesBound)
			}
			assertNoLeakedInternals(t, result.CodeHunks)
		})
	}
}

// TestBuildVersionDiffInlineStyleColorRedReindentStaysBounded pins that an inline
// style unrelated to layout (color:red) is not treated as layout-affecting for
// per-slot display, so a large reindent produces no structural changes and never
// 413s. The document-level whitespace fingerprint is unconditional, so the
// reindent still surfaces as exactly one bounded synthetic whitespace record
// that does not grow with the element count.
func TestBuildVersionDiffInlineStyleColorRedReindentStaysBounded(t *testing.T) {
	for _, count := range []int{500, 1500} {
		t.Run("n-"+strconv.Itoa(count), func(t *testing.T) {
			var compact, pretty strings.Builder
			compact.WriteString(`<div style="color:red">`)
			pretty.WriteString(`<div style="color:red">` + "\n")
			for range count {
				compact.WriteString("<p>x</p>")
				pretty.WriteString("  <p>x</p>\n")
			}
			compact.WriteString("</div>")
			pretty.WriteString("</div>")
			result, err := buildVersionDiff(1, 2, compact.String(), pretty.String())
			if err != nil {
				t.Fatalf("color:red reindent returned error (413?): %v", err)
			}
			if len(result.Changes) != 0 {
				t.Fatalf("color:red reindent produced structural changes: %d", len(result.Changes))
			}
			total := 0
			for _, h := range result.CodeHunks {
				total += len(h.Lines)
			}
			if total > reindentMaxHunkLines {
				t.Fatalf("color:red reindent produced %d hunk lines for %d elements; want a small bounded hunk", total, count)
			}
			assertNoLeakedInternals(t, result.CodeHunks)
		})
	}
}

// TestBuildVersionDiffIdenticalLayoutCSSDocIsEmpty pins that an identical document
// under layout-affecting CSS yields no changes and no hunks: the whitespace
// fingerprint compares equal for identical whitespace layout.
func TestBuildVersionDiffIdenticalLayoutCSSDocIsEmpty(t *testing.T) {
	docs := []string{
		`<style>p{display:inline}</style><p>a</p>` + "\n" + `<p>b</p>`,
		`<link rel="stylesheet" href="a.css"><div>a</div> <div>b</div>`,
		`<div style="white-space:pre"><span>x</span>` + "\n\n" + `<span>y</span></div>`,
	}
	for _, doc := range docs {
		result, err := buildVersionDiff(1, 2, doc, doc)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Changes) != 0 || len(result.CodeHunks) != 0 {
			t.Fatalf("identical layout-CSS doc produced a diff: changes=%d hunks=%d for %q", len(result.Changes), len(result.CodeHunks), doc)
		}
	}
}

// TestWhitespaceFingerprintIsDeterministicAndLayoutSensitive pins the fingerprint
// contract directly: it is built for every document (unconditional, never gated
// on CSS), depends only on the ordered inter-tag whitespace layout (slot order,
// presence, and exact bytes), is stable across runs, and stays a bounded key.
// Distinct whitespace layouts differ; identical layouts match; a literal-text
// line can never forge the synthetic record's "ws-doc" kind.
func TestWhitespaceFingerprintIsDeterministicAndLayoutSensitive(t *testing.T) {
	linesFor := func(src string) []diffSourceLine {
		lines, ok := normalizedHTMLLines(src)
		if !ok {
			t.Fatalf("normalizedHTMLLines rejected %q", src)
		}
		return lines
	}
	fpIdentity := func(lines []diffSourceLine) (string, bool) {
		for _, l := range lines {
			if strings.HasPrefix(l.identity, "ws-doc:") {
				return l.identity, true
			}
		}
		return "", false
	}

	noNewline := linesFor(`<style>p{display:inline}</style><p>a</p><p>b</p>`)
	withNewline := linesFor(`<style>p{display:inline}</style><p>a</p>` + "\n" + `<p>b</p>`)
	withSpace := linesFor(`<style>p{display:inline}</style><p>a</p> <p>b</p>`)

	id0, ok0 := fpIdentity(noNewline)
	id1, ok1 := fpIdentity(withNewline)
	id2, ok2 := fpIdentity(withSpace)
	if !ok0 || !ok1 || !ok2 {
		t.Fatal("expected a whitespace fingerprint record under layout-affecting CSS")
	}
	if id0 == id1 || id0 == id2 || id1 == id2 {
		t.Fatalf("distinct whitespace layouts collided: %q %q %q", id0, id1, id2)
	}
	if len(id0) >= 128 {
		t.Fatalf("fingerprint identity length %d exceeds bound", len(id0))
	}
	for range 4 {
		again, _ := fpIdentity(linesFor(`<style>p{display:inline}</style><p>a</p>` + "\n" + `<p>b</p>`))
		if again != id1 {
			t.Fatalf("nondeterministic fingerprint: %q != %q", again, id1)
		}
	}
	if _, ok := fpIdentity(linesFor(`<main><p>a</p><p>b</p></main>`)); !ok {
		t.Fatal("expected a whitespace fingerprint for every document (unconditional)")
	}
	// A literal text line cannot forge the synthetic "ws-doc" kind: real content
	// lines are kind "text"/"tag"/"pre"/"ws".
	forge := newDiffSourceLine("text", "ws-doc:deadbeef", "ws-doc:deadbeef")
	if strings.HasPrefix(forge.identity, "ws-doc:") {
		t.Fatalf("literal text forged the whitespace fingerprint kind: %q", forge.identity)
	}
}

// wsDocFingerprintForSource returns the "ws-doc:" fingerprint identity that
// normalizedHTMLLines now emits unconditionally for every document, failing the
// test if none is present (it must always exist).
func wsDocFingerprintForSource(t *testing.T, src string) string {
	t.Helper()
	lines, ok := normalizedHTMLLines(src)
	if !ok {
		t.Fatalf("normalizedHTMLLines rejected %q", src)
	}
	for _, l := range lines {
		if strings.HasPrefix(l.identity, "ws-doc:") {
			return l.identity
		}
	}
	t.Fatalf("no unconditional whitespace fingerprint for %q", src)
	return ""
}

// assertNotDoubleBlind fails if a before/after pair with a real (potentially
// visible) whitespace-layout difference collapses to an empty diff. A single
// bounded synthetic whitespace record is the accepted surfacing; the digest must
// never leak.
func assertNotDoubleBlind(t *testing.T, before, after string) *VersionDiff {
	t.Helper()
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatalf("buildVersionDiff returned error (413?): %v", err)
	}
	if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
		t.Fatalf("whitespace-layout change vanished (double-blind) for %q vs %q", before, after)
	}
	assertNoLeakedInternals(t, result.CodeHunks)
	return result
}

// TestBuildVersionDiffNoCSSBlockSiblingNewlineIsNotDoubleBlind pins P0-1: even
// with no <style>/stylesheet/inline layout style anywhere, a newline inserted
// between two block-level siblings must NOT double-blind. The fingerprint is
// unconditional, so an unknown future stylesheet that renders those blocks
// inline can never hide the change. It surfaces as a single bounded whitespace
// record with no digest leaked.
func TestBuildVersionDiffNoCSSBlockSiblingNewlineIsNotDoubleBlind(t *testing.T) {
	before := `<div><p>a</p><p>b</p></div>`
	after := `<div><p>a</p>` + "\n" + `<p>b</p></div>`
	// Sanity: the source is genuinely CSS-free so this exercises the universal path.
	if hasLayoutAffectingCSS(before) || hasLayoutAffectingCSS(after) {
		t.Fatalf("test inputs unexpectedly contain layout-affecting CSS")
	}
	result := assertNotDoubleBlind(t, before, after)
	body := hunksBody(result.CodeHunks)
	if !strings.Contains(body, "[formatting whitespace changed]") {
		t.Fatalf("expected comprehensible whitespace marker; hunks:\n%s", body)
	}
	total := 0
	for _, h := range result.CodeHunks {
		total += len(h.Lines)
	}
	if total > reindentMaxHunkLines {
		t.Fatalf("no-CSS block newline produced %d hunk lines; want a small bounded hunk", total)
	}
}

// TestBuildVersionDiffDynamicStyleScenariosAreNotDoubleBlind pins P0-1 for
// external/dynamic styling the conservative CSS scan cannot see: a <link
// rel="preload" as="style">, and a script that injects a stylesheet at runtime.
// Because the fingerprint is universal (not gated on hasLayoutAffectingCSS), a
// block-sibling whitespace change under either scenario still surfaces and never
// double-blinds.
func TestBuildVersionDiffDynamicStyleScenariosAreNotDoubleBlind(t *testing.T) {
	cases := []struct {
		name          string
		before, after string
	}{
		{
			"preload-as-style",
			`<link rel="preload" as="style" href="a.css"><section>a</section><section>b</section>`,
			`<link rel="preload" as="style" href="a.css"><section>a</section>` + "\n" + `<section>b</section>`,
		},
		{
			"script-injected-stylesheet",
			`<script>document.head.appendChild(document.createElement('x'))</script><div>a</div><div>b</div>`,
			`<script>document.head.appendChild(document.createElement('x'))</script><div>a</div>` + "\n" + `<div>b</div>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := assertNotDoubleBlind(t, tc.before, tc.after)
			if !strings.Contains(hunksBody(result.CodeHunks), "[formatting whitespace changed]") {
				t.Fatalf("dynamic-style whitespace change lacked the marker; hunks:\n%s", hunksBody(result.CodeHunks))
			}
			total := 0
			for _, h := range result.CodeHunks {
				total += len(h.Lines)
			}
			if total > reindentMaxHunkLines {
				t.Fatalf("%s produced %d hunk lines; want a small bounded hunk", tc.name, total)
			}
		})
	}
}

// TestWhitespaceMoveAcrossRepeatedTagsSurfacesViaBuildDiff pins the accepted P1
// trade-off. The v3 fingerprint is the ordered sequence of non-empty whitespace
// events (each event's exact bytes) with NO boundary/occurrence anchor. Moving
// the same VISIBLE run to a different boundary between otherwise-identical
// repeated inline tags does NOT change that ordered sequence (one space event,
// same bytes, same index), so the fingerprint deliberately collides — this is
// what kills the structural-insert anchor false positive. The visible move must
// still surface, though: the overall build diff must be non-empty via the
// per-slot inline source lines (the folded whitespace prefix moves from one line
// to another). A silent empty diff for a visible move would be a regression; a
// fingerprint marker here would be the false positive we removed.
//
// (A newline move purely between BLOCK boundaries is provably ignorable reindent
// noise — it renders identically — so an empty diff there is correct, not a
// regression; this test uses an inline context where the space is genuinely
// visible.)
func TestWhitespaceMoveAcrossRepeatedTagsSurfacesViaBuildDiff(t *testing.T) {
	// Inline <span> siblings: the single space is visible, so moving it across a
	// repeated boundary is a real visible change the per-slot lines must surface.
	css := `<style>span{display:inline}</style>`
	atBoundary1 := css + `<div><span>x</span> <span>x</span><span>x</span></div>`
	atBoundary2 := css + `<div><span>x</span><span>x</span> <span>x</span></div>`
	// Same bytes at another boundary must change the single positioned digest.
	if fp1, fp2 := wsDocFingerprintForSource(t, atBoundary1), wsDocFingerprintForSource(t, atBoundary2); fp1 == fp2 {
		t.Fatalf("same-bytes move left the positioned fingerprint unchanged: %q", fp1)
	}
	// The build diff must still be non-empty (the moved space surfaces through the
	// per-slot source lines and the document marker).
	result, err := buildVersionDiff(1, 2, atBoundary1, atBoundary2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
		t.Fatalf("moved-space build diff was empty for %q vs %q", atBoundary1, atBoundary2)
	}
	if !whitespaceMarkerChanged(result.CodeHunks) {
		t.Fatalf("same-bytes move lacked the whitespace marker:\n%s", hunksBody(result.CodeHunks))
	}
	assertNoLeakedInternals(t, result.CodeHunks)
}

// TestWhitespaceFingerprintIgnoresStructuralOnlyEdits covers structural edits
// that do not move an event's boundary position. Positioned events deliberately
// permit one bounded marker when an element insertion does shift that position.
func TestWhitespaceFingerprintIgnoresStructuralOnlyEdits(t *testing.T) {
	fp := func(src string) string { return wsDocFingerprintForSource(t, src) }

	// --- Structural-only edits: fingerprint MUST stay identical (no marker). ---
	// Same-type inserts are exercised before, after, AND between whitespace events
	// to prove no position-relative anchor survives.
	inert := []struct{ name, before, after string }{
		{"element-insert-empty", `<a></a><b></b>`, `<a></a><b></b><c></c>`},
		{"element-insert-content-mid", `<a>x</a><b>y</b>`, `<a>x</a><c>z</c><b>y</b>`},
		{"same-type-insert-after-ws", `<p>x</p> <p>x</p>`, `<p>x</p> <p>x</p><p>x</p>`},
		{"comment-add-before-ws", `<p>a</p> <p>b</p>`, `<p>a</p><!--n--> <p>b</p>`},
		{"comment-remove-before-ws", `<p>a</p><!--n--> <p>b</p>`, `<p>a</p> <p>b</p>`},
		{"raw-script-add-before-ws", `<p>a</p> <p>b</p>`, `<p>a</p><script>var x=1;</script> <p>b</p>`},
		{"pi-add-before-ws", `<p>a</p> <p>b</p>`, `<p>a</p><?x?> <p>b</p>`},
		{"doctype-add-before-ws", `<p>a</p> <p>b</p>`, `<p>a</p><!doctype html> <p>b</p>`},
		{"attr-change", `<p class="x">a</p> <p>b</p>`, `<p class="y">a</p> <p>b</p>`},
		{"text-change-before-ws", `<p>a</p> <p>b</p>`, `<p>ZZZ</p> <p>b</p>`},
	}
	for _, tc := range inert {
		t.Run(tc.name, func(t *testing.T) {
			if before, after := fp(tc.before), fp(tc.after); before != after {
				t.Fatalf("structural-only edit perturbed the whitespace fingerprint:\n before=%q -> %s\n after =%q -> %s", tc.before, before, tc.after, after)
			}
		})
	}

	// --- Whitespace edits: fingerprint MUST change (a marker is warranted). ---
	// These alter the ordered whitespace-event sequence (presence, exact bytes,
	// or count).
	changing := []struct{ name, before, after string }{
		{"present-vs-absent", `<a></a> <b></b>`, `<a></a><b></b>`},
		{"bytes-differ", `<a></a> <b></b>`, `<a></a>` + "\n" + `<b></b>`},
		{"extra-event", `<p>a</p> <p>b</p><p>c</p>`, `<p>a</p> <p>b</p> <p>c</p>`},
	}
	for _, tc := range changing {
		t.Run(tc.name, func(t *testing.T) {
			if before, after := fp(tc.before), fp(tc.after); before == after {
				t.Fatalf("whitespace-layout change left the fingerprint unchanged:\n before=%q\n after =%q\n digest=%s", tc.before, tc.after, before)
			}
		})
	}

	// Identical whitespace layout compares equal and is deterministic.
	if s1, s2 := fp(`<a></a> <b></b><c></c>`), fp(`<a></a> <b></b><c></c>`); s1 != s2 {
		t.Fatalf("identical layout mismatched: %q != %q", s1, s2)
	}
}

// TestBuildVersionDiffIdenticalDocIsEmptyUniversal pins that an identical
// document (whitespace fingerprint now unconditional) still yields no changes and
// no hunks via the identical-source fast path and the equal fingerprint.
func TestBuildVersionDiffIdenticalDocIsEmptyUniversal(t *testing.T) {
	docs := []string{
		`<div><p>a</p><p>b</p></div>`,
		`<div><p>a</p>` + "\n" + `<p>b</p></div>`,
		`<ul><li>x</li> <li>y</li></ul>`,
		`<a></a><b></b><c></c>`,
	}
	for _, doc := range docs {
		result, err := buildVersionDiff(1, 2, doc, doc)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Changes) != 0 || len(result.CodeHunks) != 0 {
			t.Fatalf("identical doc produced a diff: changes=%d hunks=%d for %q", len(result.Changes), len(result.CodeHunks), doc)
		}
	}
}

// TestBuildVersionDiffSameLayoutAcrossContentEditFingerprintMatches pins that a
// non-whitespace edit that preserves the exact inter-tag whitespace layout does
// not spuriously produce a whitespace record: the fingerprint is content-
// independent, so the ws-doc line is identical on both sides and only the real
// text change surfaces.
func TestBuildVersionDiffSameLayoutAcrossContentEditFingerprintMatches(t *testing.T) {
	before := `<div><p>alpha</p> <p>beta</p></div>`
	after := `<div><p>ALPHA</p> <p>beta</p></div>`
	fpBefore := wsDocFingerprintForSource(t, before)
	fpAfter := wsDocFingerprintForSource(t, after)
	if fpBefore != fpAfter {
		t.Fatalf("content-only edit changed the whitespace fingerprint: %q != %q", fpBefore, fpAfter)
	}
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(hunksBody(result.CodeHunks), "[formatting whitespace changed]") {
		t.Fatalf("content-only edit spuriously surfaced a whitespace record:\n%s", hunksBody(result.CodeHunks))
	}
	assertNoLeakedInternals(t, result.CodeHunks)
}

// TestBuildVersionDiffPlainReindentLargeStaysBoundedNoCSS pins the P0 no-413
// contract for the universal fingerprint: a large CSS-free newline reindent
// (500 and 1500 elements) never 413s and stays a small bounded hunk that does
// not grow with the element count.
func TestBuildVersionDiffPlainReindentLargeStaysBoundedNoCSS(t *testing.T) {
	for _, count := range []int{500, 1500} {
		t.Run("n-"+strconv.Itoa(count), func(t *testing.T) {
			var compact, pretty strings.Builder
			compact.WriteString("<main>")
			pretty.WriteString("<main>\n")
			for range count {
				compact.WriteString("<p>x</p>")
				pretty.WriteString("  <p>x</p>\n")
			}
			compact.WriteString("</main>")
			pretty.WriteString("</main>")
			if hasLayoutAffectingCSS(compact.String()) || hasLayoutAffectingCSS(pretty.String()) {
				t.Fatalf("test inputs unexpectedly contain layout-affecting CSS")
			}
			result, err := buildVersionDiff(1, 2, compact.String(), pretty.String())
			if err != nil {
				t.Fatalf("count=%d: plain reindent overflowed (413?): %v", count, err)
			}
			if len(result.Changes) != 0 {
				t.Fatalf("count=%d: plain reindent produced structural changes: %d", count, len(result.Changes))
			}
			total := 0
			for _, h := range result.CodeHunks {
				total += len(h.Lines)
			}
			if total > reindentMaxHunkLines {
				t.Fatalf("count=%d: plain reindent produced %d hunk lines; want a small bounded hunk", count, total)
			}
			assertNoLeakedInternals(t, result.CodeHunks)
		})
	}
}

// whitespaceMarkerChanged reports whether any hunk carries the whitespace record
// as an ADDED or REMOVED line (unified prefix '+'/'-'), i.e. a real
// whitespace-layout change. An unchanged marker that merely appears as context
// (leading ' ') is not a change and must not count — the fingerprint only
// surfaces a whitespace hunk when the layout actually differs.
func whitespaceMarkerChanged(hunks []CodeHunk) bool {
	for _, h := range hunks {
		for _, line := range h.Lines {
			if len(line) == 0 {
				continue
			}
			if (line[0] == '+' || line[0] == '-') && strings.Contains(line, "[formatting whitespace changed]") {
				return true
			}
		}
	}
	return false
}

// TestBuildVersionDiffStructuralOnlyEditEmitsNoWhitespaceHunk is the end-to-end
// guard for the final P1: a pure element/comment/raw/text edit must never
// surface a "[formatting whitespace changed]" change line. The structural
// change may (and should) surface through the normal line diff, but the
// whitespace record must stay silent because no inter-token whitespace moved.
func TestBuildVersionDiffStructuralOnlyEditEmitsNoWhitespaceHunk(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  string
	}{
		{"element-insert-empty", `<a></a><b></b>`, `<a></a><c></c><b></b>`},
		{"element-insert-content", `<a>x</a><b>y</b>`, `<a>x</a><c>z</c><b>y</b>`},
		{"element-remove-empty", `<a></a><c></c><b></b>`, `<a></a><b></b>`},
		{"comment-insert", `<p>a</p><p>b</p>`, `<p>a</p><!--c--><p>b</p>`},
		{"comment-remove", `<p>a</p><!--c--><p>b</p>`, `<p>a</p><p>b</p>`},
		{"raw-script-insert", `<p>a</p><p>b</p>`, `<p>a</p><script>x()</script><p>b</p>`},
		{"raw-script-remove", `<p>a</p><script>x()</script><p>b</p>`, `<p>a</p><p>b</p>`},
		{"attr-change", `<p class="x">a</p><p>b</p>`, `<p class="y">a</p><p>b</p>`},
		{"text-change", `<p>a</p><p>b</p>`, `<p>zzz</p><p>b</p>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, tc.before, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			if whitespaceMarkerChanged(result.CodeHunks) {
				t.Fatalf("structural-only edit produced a spurious whitespace change:\n%s", hunksBody(result.CodeHunks))
			}
			assertNoLeakedInternals(t, result.CodeHunks)
		})
	}
}

// TestBuildVersionDiffWhitespaceEditEmitsWhitespaceHunk is the end-to-end
// converse: an actual inter-token whitespace add/remove/byte change must surface
// the single bounded "[formatting whitespace changed]" change marker. A
// same-bytes move across a repeated boundary is NOT here — it does not alter the
// ordered whitespace-event sequence, so it surfaces via the per-slot/structural
// diff instead (see TestWhitespaceMoveAcrossRepeatedTagsSurfacesViaBuildDiff).
func TestBuildVersionDiffWhitespaceEditEmitsWhitespaceHunk(t *testing.T) {
	cases := []struct {
		name   string
		before string
		after  string
	}{
		{"whitespace-add", `<p>a</p><p>b</p>`, `<p>a</p> <p>b</p>`},
		{"whitespace-remove", `<p>a</p> <p>b</p>`, `<p>a</p><p>b</p>`},
		{"whitespace-bytes-differ", `<p>a</p> <p>b</p>`, `<p>a</p>` + "\n" + `<p>b</p>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, tc.before, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			if !whitespaceMarkerChanged(result.CodeHunks) {
				t.Fatalf("whitespace-layout change did not surface a whitespace change:\n%s", hunksBody(result.CodeHunks))
			}
			assertNoLeakedInternals(t, result.CodeHunks)
		})
	}
}

// A structural insert before a run may shift the positioned fingerprint, but
// output must remain bounded to the single document marker plus the real edit.
func TestWhitespaceFingerprintStructuralInsertBeforeRunStaysBounded(t *testing.T) {
	before := `<p>x</p> <p>x</p>`
	after := `<p>x</p><p>x</p> <p>x</p>`
	fpBefore := wsDocFingerprintForSource(t, before)
	fpAfter := wsDocFingerprintForSource(t, after)
	if fpBefore == fpAfter {
		t.Fatalf("positioned fingerprint ignored a shifted whitespace run: %q", fpBefore)
	}
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	// The structural insert itself must still surface (a real added element).
	if len(result.Changes) == 0 && len(result.CodeHunks) == 0 {
		t.Fatalf("structural <p> insert vanished entirely: changes=0 hunks=0")
	}
	total := 0
	for _, hunk := range result.CodeHunks {
		total += len(hunk.Lines)
	}
	if total > 20 {
		t.Fatalf("structural insert produced %d hunk lines; want bounded output", total)
	}
	assertNoLeakedInternals(t, result.CodeHunks)
}

// TestWhitespaceFingerprintCommentInsertBeforeRunIsTransparent pins the final
// sign-off P1 repro B: inserting a whitespace-free comment between "</p>" and an
// existing inter-tag whitespace run must NOT change the ordered whitespace-event
// sequence. Comments are transparent for whitespace (they add no event), so the
// run stays a single event and no spurious whitespace hunk appears.
func TestWhitespaceFingerprintCommentInsertBeforeRunIsTransparent(t *testing.T) {
	before := `<p>a</p> <p>b</p>`
	after := `<p>a</p><!--c--> <p>b</p>`
	fpBefore := wsDocFingerprintForSource(t, before)
	fpAfter := wsDocFingerprintForSource(t, after)
	if fpBefore != fpAfter {
		t.Fatalf("comment insert before a whitespace run shifted the fingerprint: %q != %q", fpBefore, fpAfter)
	}
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if whitespaceMarkerChanged(result.CodeHunks) {
		t.Fatalf("comment insert fabricated a whitespace change:\n%s", hunksBody(result.CodeHunks))
	}
	assertNoLeakedInternals(t, result.CodeHunks)
}

// TestWhitespaceFingerprintRawInsertBeforeRunIsTransparent pins that a
// whitespace-free raw-text (script/style) block inserted before an existing
// inter-tag whitespace run is transparent too: it adds no whitespace event, so
// the ordered sequence is unchanged and no spurious whitespace hunk appears,
// while a real whitespace run adjacent to a raw block is still detected.
func TestWhitespaceFingerprintRawInsertBeforeRunIsTransparent(t *testing.T) {
	before := `<p>a</p> <p>b</p>`
	after := `<p>a</p><script>x()</script> <p>b</p>`
	fpBefore := wsDocFingerprintForSource(t, before)
	fpAfter := wsDocFingerprintForSource(t, after)
	if fpBefore != fpAfter {
		t.Fatalf("raw-text insert before a whitespace run shifted the fingerprint: %q != %q", fpBefore, fpAfter)
	}
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if whitespaceMarkerChanged(result.CodeHunks) {
		t.Fatalf("raw-text insert fabricated a whitespace change:\n%s", hunksBody(result.CodeHunks))
	}
	assertNoLeakedInternals(t, result.CodeHunks)

	// A genuine whitespace run added adjacent to a raw block must still surface:
	// transparency applies to the block, never to real whitespace bytes.
	wsBefore := `<p>a</p><script>x()</script><p>b</p>`
	wsAfter := `<p>a</p><script>x()</script> <p>b</p>`
	wsResult, err := buildVersionDiff(1, 2, wsBefore, wsAfter)
	if err != nil {
		t.Fatal(err)
	}
	if !whitespaceMarkerChanged(wsResult.CodeHunks) {
		t.Fatalf("real whitespace beside a raw block was swallowed:\n%s", hunksBody(wsResult.CodeHunks))
	}
	assertNoLeakedInternals(t, wsResult.CodeHunks)
}

// Preformatted whitespace must surface for nested <pre>/<textarea>, not only when
// the element is the document root: real documents always wrap them.
func TestBuildVersionDiffNestedPreformattedWhitespaceIsVisible(t *testing.T) {
	preserved := []struct {
		name          string
		before, after string
	}{
		{"html_body_pre", "<html><body><pre>a b</pre></body></html>", "<html><body><pre>a  b</pre></body></html>"},
		{"div_pre", "<div><pre>a b</pre></div>", "<div><pre>a  b</pre></div>"},
		{"section_div_pre", "<section><div><pre>a b</pre></div></section>", "<section><div><pre>a  b</pre></div></section>"},
		{"body_pre_code_reindent", "<html><body><pre><code>if x:\n  y\n</code></pre></body></html>", "<html><body><pre><code>if x:\n    y\n</code></pre></body></html>"},
		{"body_textarea", "<html><body><textarea>a b</textarea></body></html>", "<html><body><textarea>a  b</textarea></body></html>"},
		{"form_textarea", "<form><textarea>a b</textarea></form>", "<form><textarea>a  b</textarea></form>"},
		{"body_style_pre", `<html><body><div style="white-space: pre">a b</div></body></html>`, `<html><body><div style="white-space: pre">a  b</div></body></html>`},
		{"pre_descendant_inherits", "<pre><span>a b</span></pre>", "<pre><span>a  b</span></pre>"},
	}
	for _, test := range preserved {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 || result.Summary.Modified == 0 {
				t.Fatalf("nested preformatted whitespace change missing from structural diff: %+v", result)
			}
		})
	}
	collapsed := []struct {
		name          string
		before, after string
	}{
		{"html_body_div", "<html><body><div>a b</div></body></html>", "<html><body><div>a  b</div></body></html>"},
		{"div_div", "<div><div>a b</div></div>", "<div><div>a  b</div></div>"},
	}
	for _, test := range collapsed {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) != 0 || result.Summary.Modified != 0 || len(result.CodeHunks) != 0 {
				t.Fatalf("collapsible whitespace under a wrapper became a change: %+v", result)
			}
		})
	}
}

// white-space: pre-line preserves forced line breaks, so a newline edit under it
// must surface in both output fields; spaces under it still collapse.
func TestBuildVersionDiffPreLineKeepsNewlines(t *testing.T) {
	result, err := buildVersionDiff(1, 2,
		`<div style="white-space: pre-line">a`+"\n"+`b</div>`,
		`<div style="white-space: pre-line">a`+"\n\n"+`b</div>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) == 0 || result.Summary.Modified == 0 {
		t.Fatalf("pre-line newline edit missing from structural diff: %+v", result)
	}
	if len(result.CodeHunks) == 0 {
		t.Fatalf("pre-line newline edit missing from code hunks: %+v", result)
	}
	spaces, err := buildVersionDiff(1, 2,
		`<div style="white-space: pre-line">a b</div>`,
		`<div style="white-space: pre-line">a  b</div>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces.Changes) != 0 || spaces.Summary.Modified != 0 {
		t.Fatalf("pre-line space run became structural: %+v", spaces)
	}
}

// Loose text directly inside html/head/body has no child element to carry it, so
// a wrapper's own text change must be reported instead of skipped.
func TestBuildVersionDiffWrapperOwnTextChangeIsReported(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after string
	}{
		{"body_text", "<html><body>hello</body></html>", "<html><body>world</body></html>"},
		{"html_text", "<html>hello</html>", "<html>world</html>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 || result.Summary.Modified == 0 {
				t.Fatalf("wrapper text replacement missing from structural diff: %+v", result)
			}
		})
	}
	// Reindenting a wrapper around block children is still not a change.
	result, err := buildVersionDiff(1, 2,
		"<html><body><p>x</p></body></html>",
		"<html><body>\n  <p>x</p>\n</body></html>")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range result.Changes {
		if change.DOMPath == "/html[1]/body[1]" {
			t.Fatalf("wrapper reindent reported as a change: %+v", result)
		}
	}
}

// CSS-wide keywords are unspecified, so a <pre> keeps its UA preserving default;
// invalid or comment-bearing declarations must not erase a valid earlier one.
func TestInlineWhiteSpaceModeCSSGrammar(t *testing.T) {
	for _, test := range []struct {
		name          string
		before, after string
	}{
		{"revert_on_pre", `<pre style="white-space: revert">a b</pre>`, `<pre style="white-space: revert">a  b</pre>`},
		{"revert_layer_on_pre", `<pre style="white-space: revert-layer">a b</pre>`, `<pre style="white-space: revert-layer">a  b</pre>`},
		{"inherit_on_pre", `<pre style="white-space: inherit">a b</pre>`, `<pre style="white-space: inherit">a  b</pre>`},
		{"bang_space_important", `<div style="white-space: pre ! important">a b</div>`, `<div style="white-space: pre ! important">a  b</div>`},
		{"invalid_later_declaration_dropped", `<div style="white-space: pre; white-space: garbage">a b</div>`, `<div style="white-space: pre; white-space: garbage">a  b</div>`},
		{"invalid_important_dropped", `<div style="white-space: pre; white-space: garbage !important">a b</div>`, `<div style="white-space: pre; white-space: garbage !important">a  b</div>`},
		{"css_comment", `<div style="white-space:/**/pre">a b</div>`, `<div style="white-space:/**/pre">a  b</div>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 || result.Summary.Modified == 0 {
				t.Fatalf("preserved whitespace edit missing from structural diff: %+v", result)
			}
		})
	}
	// Unclosed comment leaves nothing valid, so the tag default applies.
	result, err := buildVersionDiff(1, 2, `<div style="white-space: /*pre">a b</div>`, `<div style="white-space: /*pre">a  b</div>`)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 0 || result.Summary.Modified != 0 {
		t.Fatalf("unparsable style became preserving: %+v", result)
	}
}

// Quotes are attribute delimiters only in tags and DOCTYPEs. An apostrophe in a
// comment must not swallow the rest of the document into one synthetic tag line,
// which previously cut the real edit out of the hunks.
func TestDiffCommentQuoteDoesNotSwallowSourceEdit(t *testing.T) {
	document := func(marker, target string) string {
		return `<html><body><!-- ` + marker + ` --><p>lead paragraph of ordinary prose</p><p>` +
			target + `</p><p>tail paragraph of ordinary prose</p></body></html>`
	}
	for _, marker := range []string{"don't edit below", `say "hi"`, "author's note"} {
		t.Run(marker, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, document(marker, "TARGET-OLD"), document(marker, "TARGET-NEW"))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Changes) == 0 || result.Summary.Modified == 0 {
				t.Fatalf("structural change missing: %+v", result)
			}
			var sawOld, sawNew bool
			for _, hunk := range result.CodeHunks {
				for _, line := range hunk.Lines {
					if strings.Contains(line, "TARGET-OLD") {
						sawOld = true
					}
					if strings.Contains(line, "TARGET-NEW") {
						sawNew = true
					}
				}
			}
			if !sawOld || !sawNew {
				t.Fatalf("edited text missing from code hunks (old=%v new=%v): %+v", sawOld, sawNew, result.CodeHunks)
			}
		})
	}
}

// EOF closes an open comment in both parser views, so source-only edits remain
// code-only while one-sided recovery still returns a usable diff.
func TestDiffUnterminatedCommentRecoversAtEOF(t *testing.T) {
	result, err := buildVersionDiff(1, 2,
		`<html><body><!-- TODO<p>old</p></body></html>`,
		`<html><body><!-- TODO<p>new</p></body></html>`)
	if err != nil || len(result.Changes) != 0 || len(result.CodeHunks) == 0 {
		t.Fatalf("EOF comment recovery = (%+v, %v)", result, err)
	}
	result, err = buildVersionDiff(1, 2,
		`<html><body><p>old</p></body></html>`,
		`<html><body><!-- TODO<p>new</p></body></html>`)
	if err != nil || len(result.Changes) == 0 {
		t.Fatalf("one-sided EOF comment recovery = (%+v, %v)", result, err)
	}
	// A properly closed comment stays a normal, fully reported diff.
	result, err = buildVersionDiff(1, 2,
		`<html><body><!-- TODO --><p>old</p></body></html>`,
		`<html><body><!-- TODO --><p>new</p></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Modified != 1 {
		t.Fatalf("closed comment document: Modified = %d, want 1: %+v", result.Summary.Modified, result)
	}
	// Bogus declarations and PIs still degrade to a first-'>' terminator.
	for _, source := range []string{`<html><body><!bogus><p>x</p></body></html>`, `<html><body><?pi><p>x</p></body></html>`} {
		if _, err := buildVersionDiff(1, 2, source, strings.Replace(source, "x", "y", 1)); err != nil {
			t.Fatalf("buildVersionDiff(%q) unexpected err = %v", source, err)
		}
	}
}

func TestDiffServiceAcceptsUnterminatedComment(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	docs := NewDocService(store, store, NewCommentService(store, sluglock.NewMemory()), sluglock.NewMemory(), "", 5<<20)
	sources := []string{
		`<html><body><!-- TODO<p>old</p></body></html>`,
		`<html><body><!-- TODO<p>new</p></body></html>`,
	}
	for version, value := range sources {
		if _, err := store.PutDoc(ctx, "bad-comment", version+1, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutMeta(ctx, "bad-comment", storage.DocMeta{Slug: "bad-comment", Versions: []storage.VersionRef{{N: 1}, {N: 2}}}); err != nil {
		t.Fatal(err)
	}
	result, err := docs.Diff(ctx, "bad-comment", 1, 2)
	if err != nil || len(result.CodeHunks) == 0 {
		t.Fatalf("service diff = (%+v, %v)", result, err)
	}
}

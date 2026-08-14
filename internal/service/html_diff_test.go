package service

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// diffFor runs the real entry point so every test exercises both layers.
func diffFor(t *testing.T, before, after string) *VersionDiff {
	t.Helper()
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatalf("buildVersionDiff: %v", err)
	}
	return result
}

func changeByPath(result *VersionDiff, kind, path string) *ElementChange {
	for i := range result.Changes {
		if result.Changes[i].Kind == kind && result.Changes[i].DOMPath == path {
			return &result.Changes[i]
		}
	}
	return nil
}

func hunkText(result *VersionDiff) string {
	var b strings.Builder
	for _, hunk := range result.CodeHunks {
		for _, line := range hunk.Lines {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestDiffIdenticalDocumentsReportNothing(t *testing.T) {
	doc := `<html><head><title>T</title></head><body><p>hello</p></body></html>`
	result := diffFor(t, doc, doc)
	if len(result.Changes) != 0 || len(result.CodeHunks) != 0 {
		t.Fatalf("identical documents produced changes=%v hunks=%v", result.Changes, result.CodeHunks)
	}
	if result.Summary != (DiffSummary{}) {
		t.Fatalf("summary = %+v; want zero", result.Summary)
	}
}

func TestDiffReportsTextEditOnTheOwningElementOnly(t *testing.T) {
	before := `<html><body><div><p>old</p></div></body></html>`
	after := `<html><body><div><p>new</p></div></body></html>`
	result := diffFor(t, before, after)
	if result.Summary != (DiffSummary{Modified: 1}) {
		t.Fatalf("summary = %+v; want exactly one modified", result.Summary)
	}
	change := changeByPath(result, "modified", "html[1]>body[1]>div[1]>p[1]")
	if change == nil {
		t.Fatalf("no modified change on the <p>; changes = %+v", result.Changes)
	}
	if !strings.Contains(change.BeforeHTML, "old") || !strings.Contains(change.AfterHTML, "new") {
		t.Fatalf("snippets = %q / %q", change.BeforeHTML, change.AfterHTML)
	}
}

func TestDiffReportsAddedAndRemovedElements(t *testing.T) {
	before := `<html><body><p>a</p><p>b</p></body></html>`
	after := `<html><body><p>a</p><span>c</span></body></html>`
	result := diffFor(t, before, after)
	if result.Summary.Added != 1 || result.Summary.Removed != 1 {
		t.Fatalf("summary = %+v; want one added and one removed", result.Summary)
	}
	if changeByPath(result, "removed", "html[1]>body[1]>p[2]") == nil {
		t.Fatalf("missing removed <p>; changes = %+v", result.Changes)
	}
	if changeByPath(result, "added", "html[1]>body[1]>span[1]") == nil {
		t.Fatalf("missing added <span>; changes = %+v", result.Changes)
	}
}

func TestDiffAttributeChangeIsModification(t *testing.T) {
	before := `<html><body><a href="/one">go</a></body></html>`
	after := `<html><body><a href="/two">go</a></body></html>`
	result := diffFor(t, before, after)
	if result.Summary != (DiffSummary{Modified: 1}) {
		t.Fatalf("summary = %+v; want one modified", result.Summary)
	}
}

// An unquoted attribute value ending in a slash was a real false-equality bug in
// the hand-written scanner: the tokenizer is the only thing that decides where a
// value ends.
func TestDiffUnquotedAttributeValueWithSlashIsNotSplit(t *testing.T) {
	before := `<html><body><a href=docs/v1/intro>x</a></body></html>`
	after := `<html><body><a href=docs/intro/v1>x</a></body></html>`
	result := diffFor(t, before, after)
	if result.Summary.Modified != 1 {
		t.Fatalf("summary = %+v; want the href reordering reported", result.Summary)
	}
}

// The AID is a content hash, so it survives a move and identifies the element
// across it. It must not appear in the signature, or every edit is double counted.
func TestDiffMatchesByAIDAcrossReorder(t *testing.T) {
	before := `<html><body><p data-odoc-aid="a1">one</p><p data-odoc-aid="a2">two</p></body></html>`
	after := `<html><body><p data-odoc-aid="a2">two</p><p data-odoc-aid="a1">one</p></body></html>`
	result := diffFor(t, before, after)
	if result.Summary.Added != 0 || result.Summary.Removed != 0 {
		t.Fatalf("summary = %+v; a reorder must not add or remove elements", result.Summary)
	}
	for _, change := range result.Changes {
		if change.BeforeAID != change.AfterAID {
			t.Fatalf("AID match crossed elements: %+v", change)
		}
		if change.BeforePath == change.AfterPath {
			t.Fatalf("a move must report both positions: %+v", change)
		}
	}
}

// Deliberate limit of the two-tier rule: an element whose content is unchanged
// but which both moved and was re-stamped has neither identity, and is reported
// as a removal plus an addition. Recovering it would need similarity scoring,
// which makes the whole output unstable across unrelated edits.
func TestDiffMovedAndRestampedElementIsAddRemove(t *testing.T) {
	before := `<html><body><p data-odoc-aid="a1">one</p><p data-odoc-aid="a2">two</p></body></html>`
	after := `<html><body><p data-odoc-aid="a2">two</p><span data-odoc-aid="a9">one</span></body></html>`
	result := diffFor(t, before, after)
	if result.Summary.Added != 1 || result.Summary.Removed != 1 {
		t.Fatalf("summary = %+v; want one added and one removed", result.Summary)
	}
}

func TestDiffAIDChangeAloneIsNotAContentChange(t *testing.T) {
	before := `<html><body><p data-odoc-aid="old">same</p></body></html>`
	after := `<html><body><p data-odoc-aid="new">same</p></body></html>`
	result := diffFor(t, before, after)
	for _, change := range result.Changes {
		if change.DOMPath == "html[1]>body[1]>p[1]" {
			t.Fatalf("a re-stamped AID with identical content was reported: %+v", change)
		}
	}
}

// Markup indentation is not content: the structural layer ignores it, and the
// source layer reports it. Neither layer is asked to agree with the other.
func TestDiffLayersSplitReindentation(t *testing.T) {
	before := "<html><body><p>text</p></body></html>"
	after := "<html>\n  <body>\n    <p>text</p>\n  </body>\n</html>"
	result := diffFor(t, before, after)
	if len(result.Changes) != 0 {
		t.Fatalf("structural layer reported a reflow: %+v", result.Changes)
	}
	if len(result.CodeHunks) == 0 {
		t.Fatalf("source layer swallowed a reflow")
	}
}

func TestDiffSourceLayerReportsRawTextWhitespace(t *testing.T) {
	cases := map[string][2]string{
		"script": {"<script>let a= 1;</script>", "<script>let a=\t1;</script>"},
		"style":  {"<style>a { color: red }</style>", "<style>a {  color: red }</style>"},
		"pre":    {"<pre>x y</pre>", "<pre>x\ty</pre>"},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			before := "<html><body>" + pair[0] + "</body></html>"
			after := "<html><body>" + pair[1] + "</body></html>"
			result := diffFor(t, before, after)
			if len(result.CodeHunks) == 0 {
				t.Fatalf("whitespace-only edit inside <%s> was swallowed by the source layer", name)
			}
			if result.Summary.Modified == 0 {
				t.Fatalf("whitespace inside <%s> is content and must be a structural change too", name)
			}
		})
	}
}

// A raw-text element's content is text, not markup: a literal "<b>" inside
// <script> must not become an element.
func TestDiffRawTextContentIsNotParsedAsMarkup(t *testing.T) {
	doc := `<html><body><script>if (a<b) {}</script></body></html>`
	elements, err := parseDiffElements(doc)
	if err != nil {
		t.Fatalf("parseDiffElements: %v", err)
	}
	for _, element := range elements {
		if element.path == "html[1]>body[1]>script[1]>b[1]" {
			t.Fatalf("script content produced an element node")
		}
	}
}

func TestDiffCommentEditIsReported(t *testing.T) {
	before := `<html><body><div><!-- don't edit --></div></body></html>`
	after := `<html><body><div><!-- edited --></div></body></html>`
	result := diffFor(t, before, after)
	if result.Summary.Modified != 1 {
		t.Fatalf("summary = %+v; want the comment edit reported", result.Summary)
	}
	if !strings.Contains(hunkText(result), "edited") {
		t.Fatalf("comment edit missing from code hunks:\n%s", hunkText(result))
	}
}

// An unterminated comment swallows the rest of the document. The tree builder
// recovers from it, so the diff succeeds instead of failing the whole request.
func TestDiffUnterminatedCommentDoesNotFailTheRequest(t *testing.T) {
	before := `<html><body><p>a</p><!-- open`
	after := `<html><body><p>b</p><!-- open`
	result := diffFor(t, before, after)
	if result.Summary.Modified != 1 {
		t.Fatalf("summary = %+v; want the <p> edit reported", result.Summary)
	}
}

func TestDiffMalformedInputRecoversLikeABrowser(t *testing.T) {
	cases := map[string][2]string{
		"unclosed tag":  {"<html><body><div><p>a</body></html>", "<html><body><div><p>b</body></html>"},
		"stray end tag": {"<html><body></span><p>a</p></body></html>", "<html><body></span><p>b</p></body></html>"},
		"bad doctype":   {`<!DOCTYPE html SYSTEM "a>b"><html><body><p>a</p></body></html>`, `<!DOCTYPE html SYSTEM "a>b"><html><body><p>b</p></body></html>`},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			result := diffFor(t, pair[0], pair[1])
			if result.Summary.Modified == 0 {
				t.Fatalf("summary = %+v; want the edit reported", result.Summary)
			}
		})
	}
}

// A trailing slash does not close a non-void HTML element; the tree builder
// ignores it, so the following siblings are children.
func TestDiffSelfClosingSlashFollowsHTMLRules(t *testing.T) {
	elements, err := parseDiffElements(`<html><body><div/><p>a</p></body></html>`)
	if err != nil {
		t.Fatalf("parseDiffElements: %v", err)
	}
	found := false
	for _, element := range elements {
		if element.path == "html[1]>body[1]>div[1]>p[1]" {
			found = true
		}
	}
	if !found {
		t.Fatalf("<div/> was treated as self-closing; paths = %v", elementPaths(elements))
	}
}

// Inside SVG the slash does close the element, and the parser applies that rule.
func TestDiffForeignContentSelfCloses(t *testing.T) {
	elements, err := parseDiffElements(`<html><body><svg><rect/><circle/></svg></body></html>`)
	if err != nil {
		t.Fatalf("parseDiffElements: %v", err)
	}
	for _, element := range elements {
		if strings.Contains(element.path, "rect[1]>") {
			t.Fatalf("<rect/> inside svg did not self-close; paths = %v", elementPaths(elements))
		}
	}
}

func TestDiffAmbiguousPathsFallThroughInsteadOfMismatching(t *testing.T) {
	// Identical siblings: paths are unique, so position decides, deterministically.
	before := `<html><body><li>a</li><li>b</li><li>c</li></body></html>`
	after := `<html><body><li>a</li><li>B</li><li>c</li></body></html>`
	result := diffFor(t, before, after)
	if result.Summary != (DiffSummary{Modified: 1}) {
		t.Fatalf("summary = %+v; want only the middle item modified", result.Summary)
	}
	if changeByPath(result, "modified", "html[1]>body[1]>li[2]") == nil {
		t.Fatalf("wrong element matched; changes = %+v", result.Changes)
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	before := `<html><body><div><p>a</p><p>b</p><span>c</span></div></body></html>`
	after := `<html><body><div><p>a</p><span>c</span><p>d</p></div></body></html>`
	first := diffFor(t, before, after)
	for i := 0; i < 20; i++ {
		next := diffFor(t, before, after)
		if len(next.Changes) != len(first.Changes) || next.Summary != first.Summary {
			t.Fatalf("run %d differs: %+v vs %+v", i, next.Summary, first.Summary)
		}
		for j := range next.Changes {
			if next.Changes[j] != first.Changes[j] {
				t.Fatalf("run %d change %d differs: %+v vs %+v", i, j, next.Changes[j], first.Changes[j])
			}
		}
	}
}

func TestDiffCodeHunksCarrySignsAndLineCounts(t *testing.T) {
	before := "<html>\n<body>\n<p>a</p>\n</body>\n</html>\n"
	after := "<html>\n<body>\n<p>b</p>\n</body>\n</html>\n"
	result := diffFor(t, before, after)
	if len(result.CodeHunks) != 1 {
		t.Fatalf("hunks = %d; want 1", len(result.CodeHunks))
	}
	hunk := result.CodeHunks[0]
	if hunk.OldLines == 0 || hunk.NewLines == 0 {
		t.Fatalf("hunk = %+v; want both sides counted", hunk)
	}
	text := hunkText(result)
	if !strings.Contains(text, "-<p>a</p>") || !strings.Contains(text, "+<p>b</p>") {
		t.Fatalf("hunk lines missing signs:\n%s", text)
	}
}

func TestDiffTooManyNodesFailsClosed(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i <= maxDiffNodes; i++ {
		b.WriteString("<p>x</p>")
	}
	b.WriteString("</body></html>")
	if _, err := buildVersionDiff(1, 2, b.String(), b.String()); err == nil {
		t.Fatalf("oversized document was accepted")
	}
}

func TestDiffTooManySourceBytesFailsClosed(t *testing.T) {
	doc := "<html><body>" + strings.Repeat("<p>x</p>\n", maxDiffSourceBytes/8) + "</body></html>"
	if _, _, err := diffSourceLines(doc, doc); err == nil {
		t.Fatalf("oversized source was accepted")
	}
}

func TestDiffOutputIsTrimmedToBudget(t *testing.T) {
	filler := strings.Repeat("y", 4096)
	var before, after strings.Builder
	before.WriteString("<html><body>")
	after.WriteString("<html><body>")
	for i := 0; i < 400; i++ {
		before.WriteString("<p>" + filler + "a</p>")
		after.WriteString("<p>" + filler + "b</p>")
	}
	before.WriteString("</body></html>")
	after.WriteString("</body></html>")
	result := diffFor(t, before.String(), after.String())
	if size := diffOutputSize(result); size > maxDiffOutputBytes {
		t.Fatalf("output size = %d; want <= %d", size, maxDiffOutputBytes)
	}
	if !result.Truncated {
		t.Fatalf("trimmed response did not set truncated")
	}
}

func TestDiffCarriesRequestedVersions(t *testing.T) {
	result, err := buildVersionDiff(3, 7, "<html><body><p>a</p></body></html>", "<html><body><p>b</p></body></html>")
	if err != nil {
		t.Fatalf("buildVersionDiff: %v", err)
	}
	if result.From != 3 || result.To != 7 {
		t.Fatalf("versions = %d/%d; want 3/7", result.From, result.To)
	}
}

// The Myers layer must be bounded in memory, not just in line count. The
// previous library retained one frontier array per iteration (O(D*(N+M))) and
// allocated ~4 GB on two 8000-line documents with no line in common; this
// bounds the same shape well under a gigabyte.
func TestDiffSourceLayerIsMemoryBounded(t *testing.T) {
	const lines = 8000
	var before, after strings.Builder
	before.WriteString("<html><body><pre>")
	after.WriteString("<html><body><pre>")
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&before, "alpha line %d\n", i)
		fmt.Fprintf(&after, "beta line %d\n", i)
	}
	before.WriteString("</pre></body></html>")
	after.WriteString("</pre></body></html>")

	var start, end runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&start)
	result, err := buildVersionDiff(1, 2, before.String(), after.String())
	runtime.ReadMemStats(&end)
	if err != nil {
		t.Fatalf("two 8000-line documents were rejected: %v", err)
	}
	allocated := end.TotalAlloc - start.TotalAlloc
	if allocated > 1<<30 {
		t.Fatalf("allocated %.2f GB for %d lines; the line diff is not bounded", float64(allocated)/(1<<30), lines)
	}
	// A fully rewritten document must still return usable hunks: burning the
	// budget and answering with an empty code_hunks is the failure mode this
	// layer was rebuilt to avoid.
	if len(result.CodeHunks) == 0 {
		t.Fatalf("no code hunks for a fully rewritten document")
	}
	if !result.Truncated {
		t.Fatalf("an over-budget run was not flagged as truncated")
	}
}

// A pathological pair must answer instead of running to completion: the library
// deadline degrades fidelity, not liveness.
func TestDiffSourceLayerHonoursDeadline(t *testing.T) {
	var before, after strings.Builder
	before.WriteString("<html><body><pre>")
	after.WriteString("<html><body><pre>")
	for i := 0; i < 40000; i++ {
		before.WriteString("aaaa\n")
		after.WriteString("bbbb\n")
	}
	before.WriteString("</pre></body></html>")
	after.WriteString("</pre></body></html>")
	start := time.Now()
	if _, err := buildVersionDiff(1, 2, before.String(), after.String()); err != nil {
		t.Fatalf("buildVersionDiff: %v", err)
	}
	// diffTimeout is 2s and applies to each of the two layers.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("elapsed %v; the deadline did not bail", elapsed)
	}
}

// An insertion must not re-pair every following sibling. Only stamped tags carry
// an AID, so prose relies entirely on the sequence tier.
func TestDiffInsertionDoesNotShiftFollowingSiblings(t *testing.T) {
	for _, n := range []int{3, 1200} {
		var before, after strings.Builder
		before.WriteString("<html><body><ul>")
		after.WriteString("<html><body><ul><li>NEW</li>")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&before, "<li>item %d</li>", i)
			fmt.Fprintf(&after, "<li>item %d</li>", i)
		}
		before.WriteString("</ul></body></html>")
		after.WriteString("</ul></body></html>")
		result := diffFor(t, before.String(), after.String())
		if result.Summary.Added != 1 {
			t.Fatalf("n=%d summary = %+v; want exactly one addition", n, result.Summary)
		}
		// The <ul> itself changes (its child tag list grows), nothing else should.
		if result.Summary.Modified > 1 {
			t.Fatalf("n=%d summary = %+v; an insertion amplified into modifications", n, result.Summary)
		}
		if result.Summary.Removed != 0 {
			t.Fatalf("n=%d summary = %+v; an insertion removed nothing", n, result.Summary)
		}
	}
}

// Truncation must keep the highest-signal changes. Adds and removes are what a
// reader is looking for; modifications are the ones a shift can mass-produce.
func TestDiffTruncationKeepsAddsAndRemoves(t *testing.T) {
	var before, after strings.Builder
	before.WriteString("<html><body>")
	after.WriteString("<html><body>")
	for i := 0; i < maxDiffChanges+400; i++ {
		fmt.Fprintf(&before, "<p>old %d</p>", i)
		fmt.Fprintf(&after, "<p>new %d</p>", i)
	}
	after.WriteString("<section>added</section>")
	before.WriteString("</body></html>")
	after.WriteString("</body></html>")

	result := diffFor(t, before.String(), after.String())
	if !result.Truncated {
		t.Fatalf("expected truncation with %d changes", len(result.Changes))
	}
	if len(result.Changes) > maxDiffChanges {
		t.Fatalf("changes = %d; want at most %d", len(result.Changes), maxDiffChanges)
	}
	found := false
	for _, change := range result.Changes {
		if change.Kind == "added" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the only added element was truncated away in favour of modifications")
	}
}

// Changes stay in document order after the by-kind merge, so a reader can walk
// the response top to bottom.
func TestDiffChangesAreInDocumentOrder(t *testing.T) {
	before := `<html><body><p>a</p><p>b</p><p>c</p></body></html>`
	after := `<html><body><p>a</p><span>x</span><p>C</p></body></html>`
	result := diffFor(t, before, after)
	previous := ""
	for _, change := range result.Changes {
		key := change.AfterPath
		if key == "" {
			key = change.BeforePath
		}
		if previous != "" && key < previous {
			t.Fatalf("changes are out of document order: %q after %q", key, previous)
		}
		previous = key
	}
}

// A signature is length-prefixed, so an attribute value cannot forge another
// element's signature by embedding the delimiters.
func TestDiffSignatureResistsDelimiterInjection(t *testing.T) {
	before := `<html><body><div a="1|:b=2"></div></body></html>`
	after := `<html><body><div a="1" b="2"></div></body></html>`
	result := diffFor(t, before, after)
	if result.Summary.Modified != 1 {
		t.Fatalf("summary = %+v; a real attribute change was swallowed by a signature collision", result.Summary)
	}
}

// Space next to an inline sibling is rendered content, not indentation.
func TestDiffReportsWhitespaceNextToInlineSibling(t *testing.T) {
	before := `<html><body><p>Hello <em>x</em></p></body></html>`
	after := `<html><body><p>Hello<em>x</em></p></body></html>`
	result := diffFor(t, before, after)
	if result.Summary.Modified == 0 {
		t.Fatalf("summary = %+v; a rendered space change was reported as no change", result.Summary)
	}
}

// The output budget is measured on encoded JSON, since the encoder escapes HTML
// and this payload is mostly HTML.
func TestDiffOutputBudgetCountsJSONEscaping(t *testing.T) {
	var before, after strings.Builder
	before.WriteString("<html><body>")
	after.WriteString("<html><body>")
	// Markup-dense but well under the node cap, so the budget is what bites.
	filler := strings.Repeat("<b>tag</b>", 12) + strings.Repeat("text ", 400)
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&before, "<p>%s a%d</p>", filler, i)
		fmt.Fprintf(&after, "<p>%s b%d</p>", filler, i)
	}
	before.WriteString("</body></html>")
	after.WriteString("</body></html>")
	result := diffFor(t, before.String(), after.String())
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// The budget covers the change and hunk payload, not the whole envelope, so
	// allow modest slack for field names and structure.
	if len(encoded) > maxDiffOutputBytes*2 {
		t.Fatalf("encoded response = %d bytes; budget is %d", len(encoded), maxDiffOutputBytes)
	}
}

// Hunk line numbers must address the real files, including after a hunk is
// dropped for exceeding the per-hunk budget.
func TestDiffHunkLineNumbersAddressTheSource(t *testing.T) {
	before := "<html>\n<body>\n<p>a</p>\n<p>keep</p>\n<p>b</p>\n</body>\n</html>\n"
	after := "<html>\n<body>\n<p>A</p>\n<p>keep</p>\n<p>B</p>\n</body>\n</html>\n"
	result := diffFor(t, before, after)
	beforeLines := strings.Split(before, "\n")
	for _, hunk := range result.CodeHunks {
		if hunk.OldStart < 1 || hunk.OldStart > len(beforeLines) {
			t.Fatalf("hunk OldStart = %d outside 1..%d", hunk.OldStart, len(beforeLines))
		}
		offset := 0
		for _, line := range hunk.Lines {
			if !strings.HasPrefix(line, "+") {
				want := beforeLines[hunk.OldStart-1+offset]
				if line[1:] != want {
					t.Fatalf("hunk line %q does not match source line %d (%q)", line, hunk.OldStart+offset, want)
				}
				offset++
			}
		}
	}
}

func elementPaths(elements []diffElement) []string {
	paths := make([]string, 0, len(elements))
	for _, element := range elements {
		paths = append(paths, element.path)
	}
	return paths
}

func FuzzBuildVersionDiff(f *testing.F) {
	f.Add("<html><body><p>a</p></body></html>", "<html><body><p>b</p></body></html>")
	f.Add("<div><!-- c", "<div><!-- d")
	f.Add("<script>a<b</script>", "<script>a>b</script>")
	f.Add("<svg><rect/></svg>", "<svg><rect></svg>")
	f.Fuzz(func(t *testing.T, before, after string) {
		result, err := buildVersionDiff(1, 2, before, after)
		if err != nil {
			return
		}
		if size := diffOutputSize(result); size > maxDiffOutputBytes {
			t.Fatalf("output size = %d exceeds budget", size)
		}
		if before == after && (len(result.Changes) != 0 || len(result.CodeHunks) != 0) {
			t.Fatalf("identical input produced a diff: %+v", result)
		}
	})
}

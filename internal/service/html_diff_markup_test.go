package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
	xhtml "golang.org/x/net/html"
)

// diffProbe runs a before/after pair through both scanning layers and the public
// entry point, so a matrix row cannot pass by agreeing at one layer only.
type diffProbe struct {
	structuralErr  error
	sourceLinesOK  bool
	diffErr        error
	changePaths    []string
	modified       int
	hunkText       string
	structuralNode int
}

func probeDiff(t *testing.T, before, after string) diffProbe {
	t.Helper()
	probe := diffProbe{}
	nodes, err := parseDiffHTML(before)
	probe.structuralErr = err
	probe.structuralNode = len(nodes)
	_, probe.sourceLinesOK = normalizedHTMLLines(before)
	result, err := buildVersionDiff(1, 2, before, after)
	probe.diffErr = err
	if err != nil {
		return probe
	}
	probe.modified = result.Summary.Modified
	for _, change := range result.Changes {
		path := change.DOMPath
		if path == "" {
			path = change.BeforePath
		}
		probe.changePaths = append(probe.changePaths, path)
	}
	var hunks strings.Builder
	for _, hunk := range result.CodeHunks {
		for _, line := range hunk.Lines {
			hunks.WriteString(line)
			hunks.WriteByte('\n')
		}
	}
	probe.hunkText = hunks.String()
	return probe
}

func (p diffProbe) hasPath(want string) bool {
	for _, path := range p.changePaths {
		if path == want {
			return true
		}
	}
	return false
}

// Comment termination must follow the tokenizer states that core.commentEnd
// already implements: "-->" and "--!>" both close (whichever comes EARLIER, so a
// following real element is not swallowed), "<!-->" / "<!--->" close abruptly, and
// only a comment with no terminator at all runs to EOF and fails closed.
func TestDiffCommentTerminatorMatrix(t *testing.T) {
	document := func(marker, target string) string {
		return `<html><body><div>` + marker + `<p>` + target + `</p></div></body></html>`
	}
	terminated := []struct {
		name   string
		marker string
	}{
		{"comment_end", "<!-- x -->"},
		{"comment_end_bang", "<!-- x --!>"},
		{"abrupt_empty", "<!-->"},
		{"abrupt_empty_dash", "<!--->"},
		{"nested_dashes", "<!-- a -- b -->"},
	}
	for _, test := range terminated {
		t.Run(test.name, func(t *testing.T) {
			probe := probeDiff(t, document(test.marker, "TARGET-OLD"), document(test.marker, "TARGET-NEW"))
			if probe.structuralErr != nil {
				t.Fatalf("parseDiffHTML err = %v, want nil", probe.structuralErr)
			}
			if !probe.sourceLinesOK {
				t.Fatal("normalizedHTMLLines ok = false, want true")
			}
			if probe.diffErr != nil {
				t.Fatalf("buildVersionDiff err = %v, want nil", probe.diffErr)
			}
			if !probe.hasPath("/html[1]/body[1]/div[1]/p[1]") {
				t.Fatalf("edited element missing from changes: paths=%v", probe.changePaths)
			}
			if probe.modified != 1 {
				t.Fatalf("Summary.Modified = %d, want 1", probe.modified)
			}
			if !strings.Contains(probe.hunkText, "TARGET-OLD") || !strings.Contains(probe.hunkText, "TARGET-NEW") {
				t.Fatalf("edited text missing from code hunks:\n%s", probe.hunkText)
			}
		})
	}
	// A closed comment must not over-scan to a LATER terminator and swallow the
	// real elements in between.
	swallow := func(target string) string {
		return `<html><body><div><!-- c --!><p>` + target + `</p><!-- later --><p>tail</p></div></body></html>`
	}
	probe := probeDiff(t, swallow("TARGET-OLD"), swallow("TARGET-NEW"))
	if probe.diffErr != nil {
		t.Fatalf("swallow case err = %v, want nil", probe.diffErr)
	}
	if !probe.hasPath("/html[1]/body[1]/div[1]/p[1]") {
		t.Fatalf("element after --!> was swallowed: paths=%v nodes=%d", probe.changePaths, probe.structuralNode)
	}
	if probe.modified != 1 {
		t.Fatalf("swallow case Summary.Modified = %d, want 1", probe.modified)
	}
	// The tokenizer and tree builder both recover an open comment at EOF.
	for _, marker := range []string{"<!-- TODO", "<!--!>", "<!-- x --"} {
		unterminated := func(target string) string {
			return `<html><body><div>` + marker + `<p>` + target + `</p></div></body></html>`
		}
		probe = probeDiff(t, unterminated("TARGET-OLD"), unterminated("TARGET-NEW"))
		if !probe.sourceLinesOK {
			t.Fatalf("%q normalizedHTMLLines ok = false, want true", marker)
		}
		if probe.diffErr != nil {
			t.Fatalf("%q buildVersionDiff err = %v, want nil", marker, probe.diffErr)
		}
	}
}

// Every element parsed by the generic raw-text / RCDATA algorithms holds text,
// not markup: a literal "<!--" inside one is not a comment, and its content must
// not become DOM nodes. A trailing '/' on the start tag is a parse error the
// tokenizer ignores, so the element still opens as raw text.
func TestDiffRawTextContextMatrix(t *testing.T) {
	for _, tag := range []string{"script", "style", "textarea", "title", "iframe", "noembed", "noframes", "xmp", "noscript"} {
		t.Run(tag, func(t *testing.T) {
			for _, open := range []string{"<" + tag + ">", "<" + tag + "/>", "<" + tag + ` src="a"/>`} {
				document := func(target string) string {
					return `<html><body>` + open + `<!--</` + tag + `><p>` + target + `</p></body></html>`
				}
				probe := probeDiff(t, document("TARGET-OLD"), document("TARGET-NEW"))
				if probe.structuralErr != nil {
					t.Fatalf("%s: parseDiffHTML err = %v, want nil", open, probe.structuralErr)
				}
				if !probe.sourceLinesOK {
					t.Fatalf("%s: normalizedHTMLLines ok = false, want true", open)
				}
				if probe.diffErr != nil {
					t.Fatalf("%s: buildVersionDiff err = %v, want nil", open, probe.diffErr)
				}
				if !probe.hasPath("/html[1]/body[1]/p[1]") {
					t.Fatalf("%s: element after raw text missing from changes: paths=%v", open, probe.changePaths)
				}
				if probe.modified != 1 {
					t.Fatalf("%s: Summary.Modified = %d, want 1", open, probe.modified)
				}
				if !strings.Contains(probe.hunkText, "TARGET-OLD") || !strings.Contains(probe.hunkText, "TARGET-NEW") {
					t.Fatalf("%s: edited text missing from code hunks:\n%s", open, probe.hunkText)
				}
			}
			// Raw-text content is text: it must not produce child nodes.
			fallback := `<html><body><` + tag + `><p>inner</p></` + tag + `><p>outer-OLD</p></body></html>`
			nodes, err := parseDiffHTML(fallback)
			if err != nil {
				t.Fatalf("fallback parse err = %v", err)
			}
			for _, node := range nodes {
				if strings.Contains(node.path, "/"+tag+"[1]/") {
					t.Fatalf("raw-text content produced a child node: %q", node.path)
				}
			}
		})
	}
	// Both layers must treat a self-closed raw-text open tag the same way; a
	// disagreement here previously produced a 413 from one layer and acceptance
	// from the other.
	for _, source := range []string{
		"<iframe/><!--", "<xmp/><!--", "<noscript/><!--", "<noembed/><!--",
		"<noframes/><!--", "<script/><!--", "<style/><!--", "<textarea/><!--", "<title/><!--",
	} {
		_, structuralErr := parseDiffHTML(source)
		_, linesOK := normalizedHTMLLines(source)
		if structuralErr != nil {
			t.Fatalf("%q: parseDiffHTML err = %v, want nil (the \"<!--\" is raw text)", source, structuralErr)
		}
		if !linesOK {
			t.Fatalf("%q: normalizedHTMLLines ok = false, want true", source)
		}
	}
	// A trailing '/' on a non-raw-text element keeps its existing meaning: void
	// elements take no content, ordinary ones still open (see
	// TestDiffSelfClosingFlagFollowsNamespace).
	for _, test := range []struct{ name, source, wantPath string }{
		{"div", `<html><body><div/><p>x</p></div></body></html>`, "/html[1]/body[1]/div[1]/p[1]"},
		{"section", `<html><body><section/><p>x</p></section></body></html>`, "/html[1]/body[1]/section[1]/p[1]"},
		{"void_br", `<html><body><br/><p>x</p></body></html>`, "/html[1]/body[1]/p[1]"},
		{"void_img", `<html><body><img src="a"/><p>x</p></body></html>`, "/html[1]/body[1]/p[1]"},
	} {
		t.Run("self_close_"+test.name, func(t *testing.T) {
			nodes, err := parseDiffHTML(test.source)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			var found bool
			for _, node := range nodes {
				if node.path == test.wantPath {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s missing from the tree", test.wantPath)
			}
		})
	}
}

// A DOCTYPE ends at the first '>' like any other declaration: in the DOCTYPE
// system-identifier states a '>' is an abrupt-doctype parse error whose action is
// to emit the DOCTYPE, quoted or not.
func TestDiffDoctypeMatrix(t *testing.T) {
	for _, test := range []struct {
		name     string
		doctype  string
		wantPath string
	}{
		{"plain", `<!DOCTYPE html>`, "/html[1]/body[1]/p[1]"},
		{"mixed_case", `<!dOcTyPe HtMl>`, "/html[1]/body[1]/p[1]"},
		{"quoted_system_id", `<!DOCTYPE html SYSTEM "about:legacy-compat">`, "/html[1]/body[1]/p[1]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := func(target string) string {
				return test.doctype + `<html><body><p>` + target + `</p></body></html>`
			}
			probe := probeDiff(t, document("TARGET-OLD"), document("TARGET-NEW"))
			if probe.diffErr != nil {
				t.Fatalf("err = %v, want nil", probe.diffErr)
			}
			if !probe.hasPath(test.wantPath) {
				t.Fatalf("paths=%v, want %s", probe.changePaths, test.wantPath)
			}
		})
	}
	// A '>' inside a quoted system identifier still ends the DOCTYPE, so the text
	// after it is character data and the following element is real.
	quotedGT := func(target string) string {
		return `<!DOCTYPE html SYSTEM "about:legacy->compat"><html><body><p>` + target + `</p></body></html>`
	}
	probe := probeDiff(t, quotedGT("TARGET-OLD"), quotedGT("TARGET-NEW"))
	if probe.diffErr != nil {
		t.Fatalf("quoted '>' err = %v, want nil", probe.diffErr)
	}
	if !probe.hasPath("/html[1]/body[1]/p[1]") {
		t.Fatalf("quoted '>' paths=%v, want the real <p>", probe.changePaths)
	}
	// An unbalanced quote must not truncate the tree: the <span> after the bogus
	// DOCTYPE is a real element, reported at its own path.
	unbalanced := func(target string) string {
		return `<html><body><div><!doctype x "><span>` + target + `</span></div></body></html>`
	}
	probe = probeDiff(t, unbalanced("TARGET-OLD"), unbalanced("TARGET-NEW"))
	if probe.diffErr != nil {
		t.Fatalf("unbalanced quote err = %v, want nil", probe.diffErr)
	}
	if !probe.hasPath("/html[1]/body[1]/div[1]/span[1]") {
		t.Fatalf("unbalanced quote paths=%v, want the real <span>", probe.changePaths)
	}
}

// Both layers must agree on where every construct ends, and only a genuinely
// unterminated comment may fail closed. A disagreement is what let the two layers
// drift apart before both views shared the standard tokenizer front-end.
func TestDiffMarkupScannerInvariants(t *testing.T) {
	seeds := []string{
		"<!--",
		"<!-->",
		"<!--->",
		"<!-- x -->",
		"<!-- x --!>",
		"<!-- x --!><p>x</p><!-- later -->",
		`<!doctype x ">`,
		`<!doctype x "a>b">`,
		"<iframe><!--</iframe>",
		"<noscript><!--</noscript>",
		`<tag title="a>b">`,
		"<!bogus>",
		"<?php echo \">\"; ?>",
		"<html><body><p>plain</p></body></html>",
	}
	for _, seed := range seeds {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %q: %v", seed, r)
				}
			}()
			_, structuralErr := parseDiffHTML(seed)
			if structuralErr != nil {
				t.Fatalf("structural layer rejected %q: %v", seed, structuralErr)
			}
			_, linesOK := normalizedHTMLLines(seed)
			if !linesOK {
				t.Fatalf("source layer rejected bounded recovered markup %q", seed)
			}
			_, diffErr := buildVersionDiff(1, 2, seed, seed+"x")
			if diffErr != nil {
				t.Fatalf("buildVersionDiff(%q) err = %v", seed, diffErr)
			}
		}()
	}
}

// isUnterminatedDiffComment reports whether source contains a comment opener that
// reaches EOF without any terminator, computed independently of the scanner under
// test. It checks EVERY opener, since an earlier comment can close and a later one
// run off the end. Deliberately conservative: an opener inside raw text or inside
// another comment's content also counts, which can only make the assertions that
// use it weaker, never wrong.
func isUnterminatedDiffComment(source string) bool {
	for index := 0; index+4 <= len(source); index++ {
		if !strings.HasPrefix(source[index:], "<!--") {
			continue
		}
		if !referenceCommentTerminated(source, index) {
			return true
		}
	}
	return false
}

// The three forms that terminate a comment must not reach the caller as the 413
// reserved for a diff that cannot be completed.
func TestDiffTerminatedCommentIsNotPayloadTooLarge(t *testing.T) {
	ctx := context.Background()
	for _, marker := range []string{"<!-- x --!>", "<!-->", "<!--->"} {
		store := memory.New()
		docs := NewDocService(store, store, NewCommentService(store, sluglock.NewMemory()), sluglock.NewMemory(), "", 5<<20)
		for version, target := range []string{"TARGET-OLD", "TARGET-NEW"} {
			source := `<html><body><div>` + marker + `<p>` + target + `</p></div></body></html>`
			if _, err := store.PutDoc(ctx, "marker", version+1, source); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.PutMeta(ctx, "marker", storage.DocMeta{Slug: "marker", Versions: []storage.VersionRef{{N: 1}, {N: 2}}}); err != nil {
			t.Fatal(err)
		}
		result, err := docs.Diff(ctx, "marker", 1, 2)
		var appErr *apperr.Error
		if errors.As(err, &appErr) {
			t.Fatalf("marker %q: error = %#v; want a successful diff", marker, appErr)
		}
		if err != nil {
			t.Fatalf("marker %q: err = %v", marker, err)
		}
		if result.Summary.Modified != 1 {
			t.Fatalf("marker %q: Modified = %d, want 1", marker, result.Summary.Modified)
		}
	}
}

// referenceCommentEnd is core.commentEnd's rule written the way core writes it
// (two independent searches, earlier terminator wins). This pins the standard
// tokenizer's emitted boundary against the behaviour core already ships.
func referenceCommentEnd(s string, lt int) int {
	n := len(s)
	if lt+3 >= n || s[lt+1] != '!' || s[lt+2] != '-' || s[lt+3] != '-' {
		return -1
	}
	p := lt + 4
	if p < n && s[p] == '>' {
		return p + 1
	}
	if p+1 < n && s[p] == '-' && s[p+1] == '>' {
		return p + 2
	}
	dash := strings.Index(s[p:], "-->")
	bang := strings.Index(s[p:], "--!>")
	switch {
	case dash < 0 && bang < 0:
		return n
	case bang < 0 || (dash >= 0 && dash <= bang):
		return p + dash + len("-->")
	default:
		return p + bang + len("--!>")
	}
}

// referenceCommentTerminated reports whether the comment at lt actually closes,
// independently of any index arithmetic, so the equivalence check below cannot
// confuse an abrupt closing at EOF with running off the end.
func referenceCommentTerminated(s string, lt int) bool {
	p := lt + 4
	if p < len(s) && s[p] == '>' {
		return true
	}
	if p+1 < len(s) && s[p] == '-' && s[p+1] == '>' {
		return true
	}
	rest := s[p:]
	return strings.Contains(rest, "-->") || strings.Contains(rest, "--!>")
}

func TestDiffScannerCommentBoundariesMatchCoreRule(t *testing.T) {
	bodies := []string{
		"", ">", "->", "-->", "--!>", "x", "x-->", "x--!>", "-", "--", "---", "----",
		"--->", "---!>", "x--y-->", "x--!y-->", "x-->y--!>", "x--!>y-->",
		"a--!", "a--", "!>", "-!>", "--x!>", "<!--", "x<!--y-->", "> -->", "-- >",
	}
	check := func(source string, start int) {
		t.Helper()
		wantTerminated := referenceCommentTerminated(source, start)
		var gotRaw string
		gotEnd := -1
		err := scanDiffHTML(source, func(token diffHTMLToken) error {
			if token.type_ == xhtml.CommentToken && token.start == start {
				gotRaw = token.raw
				gotEnd = token.end
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%q: scanner err = %v", source, err)
		}
		if !wantTerminated {
			if gotEnd != len(source) || gotRaw != source[start:] {
				t.Fatalf("%q: EOF recovery raw=%q end=%d", source, gotRaw, gotEnd)
			}
			return
		}
		wantEnd := referenceCommentEnd(source, start)
		if gotEnd != wantEnd {
			t.Fatalf("%q: scanner end = %d, reference end = %d (raw %q)", source, gotEnd, wantEnd, gotRaw)
		}
		if gotRaw != source[start:wantEnd] {
			t.Fatalf("%q: scanner raw = %q, want %q", source, gotRaw, source[start:wantEnd])
		}
	}
	for _, body := range bodies {
		check("<!--"+body, 0)
		embedded := "<div>text<!--" + body + "</div>"
		check(embedded, strings.Index(embedded, "<!--"))
	}
}

// FuzzDiffMarkupScanner holds the invariants the two scanning layers must keep on
// arbitrary input: no panic, bounded parser recovery, and only documented
// resource limits may fail closed.
func FuzzDiffMarkupScanner(f *testing.F) {
	seeds := []string{
		"<!--",
		"<!-->",
		"<!--->",
		"<!-- x -->",
		"<!-- x --!>",
		"<!-- x --!><p>x</p><!-- later -->",
		`<!doctype x ">`,
		`<!doctype x "a>b">`,
		"<iframe><!--</iframe>",
		"<noscript><!--</noscript>",
		`<tag title="a>b">`,
		"<!bogus>",
		`<?php echo ">"; ?>`,
		"<html><body><p>plain</p></body></html>",
		"<pre>a  b</pre>",
		"<xmp></xmp",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 1<<16 {
			t.Skip()
		}
		_, structuralErr := parseDiffHTML(source)
		if structuralErr != nil && !errors.Is(structuralErr, errDiffLimit) {
			t.Fatalf("parseDiffHTML err = %v, want nil or errDiffLimit", structuralErr)
		}
		_, linesOK := normalizedHTMLLines(source)
		// Layer agreement only holds for the fail-closed rule; the source layer may
		// also reject on its own line budget, which the structural layer does not share.
		if errors.Is(structuralErr, errDiffLimit) && isUnterminatedDiffComment(source) && linesOK {
			t.Fatalf("structural layer failed closed but source layer accepted: %q", source)
		}
		if errors.Is(structuralErr, errDiffLimit) && !isUnterminatedDiffComment(source) {
			// Every other rejection must come from a documented bound, never from
			// misreading a terminated construct.
			if len(source) < maxDiffTagBytes {
				t.Fatalf("small input failed closed without an unterminated comment: %q", source)
			}
		}
		if _, err := buildVersionDiff(1, 2, source, source+"x"); err != nil && !errors.Is(err, errDiffLimit) {
			t.Fatalf("buildVersionDiff err = %v", err)
		}
	})
}

func FuzzDiffSelfClosedRawText(f *testing.F) {
	for _, seed := range []string{
		"<iframe/><!--", "<script/><!--", `<iframe src="a"/><p>x</p>`,
		"<xmp/>", "<noscript/><p>x</p>", "<div/><p>x</p>", "<br/><p>x</p>",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source string) {
		if len(source) > 1<<14 {
			t.Skip()
		}
		_, structuralErr := parseDiffHTML(source)
		_, linesOK := normalizedHTMLLines(source)
		if errors.Is(structuralErr, errDiffLimit) && linesOK && isUnterminatedDiffComment(source) {
			t.Fatalf("structural failed closed, source accepted: %q", source)
		}
	})
}

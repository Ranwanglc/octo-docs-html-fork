package service

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sergi/go-diff/diffmatchpatch"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var errDiffLimit = errors.New("diff complexity limit exceeded")

const (
	maxDiffNodes        = 8000
	maxDiffChanges      = 1000
	maxDiffSourceBytes  = 8 << 20
	maxDiffHunkLines    = 2000
	maxDiffSnippetBytes = 8 << 10
	maxDiffPathBytes    = 1 << 10
	maxDiffOutputBytes  = 512 << 10
	diffContextLines    = 3

	// diffTimeout bounds the line-level search. On expiry the library falls back
	// to a whole-file replace, so the request still answers.
	diffTimeout = 2 * time.Second

	aidAttr = "data-odoc-aid"
)

// VersionDiff compares two published versions in two independent layers:
// Changes is DOM-structural, CodeHunks is raw source lines. The two answer
// different questions and are deliberately not reconciled with each other.
type VersionDiff struct {
	From      int             `json:"from"`
	To        int             `json:"to"`
	Summary   DiffSummary     `json:"summary"`
	Changes   []ElementChange `json:"changes"`
	CodeHunks []CodeHunk      `json:"code_hunks"`
	Truncated bool            `json:"truncated,omitempty"`
}

// DiffSummary counts element-level changes by kind.
type DiffSummary struct {
	Added    int `json:"added"`
	Removed  int `json:"removed"`
	Modified int `json:"modified"`
}

// ElementChange describes one added, removed, or modified element.
type ElementChange struct {
	Kind       string `json:"kind"`
	BeforeAID  string `json:"before_aid,omitempty"`
	AfterAID   string `json:"after_aid,omitempty"`
	DOMPath    string `json:"dom_path"`
	BeforePath string `json:"before_path,omitempty"`
	AfterPath  string `json:"after_path,omitempty"`
	BeforeHTML string `json:"before_html,omitempty"`
	AfterHTML  string `json:"after_html,omitempty"`
}

// CodeHunk is a unified-style hunk over the raw published bytes.
type CodeHunk struct {
	OldStart int      `json:"old_start"`
	OldLines int      `json:"old_lines"`
	NewStart int      `json:"new_start"`
	NewLines int      `json:"new_lines"`
	Lines    []string `json:"lines"`
}

// diffElement is one element node flattened in document order.
type diffElement struct {
	node      *xhtml.Node
	aid       string
	path      string
	signature string
}

// buildVersionDiff runs both layers over the two stored documents.
func buildVersionDiff(fromVersion, toVersion int, before, after string) (*VersionDiff, error) {
	beforeElements, err := parseDiffElements(before)
	if err != nil {
		return nil, err
	}
	afterElements, err := parseDiffElements(after)
	if err != nil {
		return nil, err
	}
	changes, summary, truncated := diffElements(beforeElements, afterElements)
	hunks, hunksTruncated, err := diffSourceLines(before, after)
	if err != nil {
		return nil, err
	}
	result := &VersionDiff{
		From:      fromVersion,
		To:        toVersion,
		Summary:   summary,
		Changes:   changes,
		CodeHunks: hunks,
		Truncated: truncated || hunksTruncated,
	}
	trimDiffOutput(result)
	return result, nil
}

// parseDiffElements parses with the standard tree builder and flattens every
// element in document order. Malformed input is recovered by the parser exactly
// as a browser would, so this layer has no error-recovery rules of its own.
func parseDiffElements(source string) ([]diffElement, error) {
	root, err := xhtml.Parse(strings.NewReader(source))
	if err != nil {
		return nil, err
	}
	elements := make([]diffElement, 0, 64)
	var walk func(node *xhtml.Node, path string) error
	walk = func(node *xhtml.Node, path string) error {
		counts := map[string]int{}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != xhtml.ElementNode {
				continue
			}
			name := diffTagName(child)
			counts[name]++
			childPath := path + ">" + name + "[" + strconv.Itoa(counts[name]) + "]"
			if len(childPath) > maxDiffPathBytes {
				return errDiffLimit
			}
			if len(elements) >= maxDiffNodes {
				return errDiffLimit
			}
			elements = append(elements, diffElement{
				node:      child,
				aid:       diffAttr(child, aidAttr),
				path:      strings.TrimPrefix(childPath, ">"),
				signature: diffSignature(child),
			})
			if err := walk(child, childPath); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return nil, err
	}
	return elements, nil
}

// diffElements matches the two element lists and classifies the result.
//
// Matching is deterministic and two-tiered on purpose: a unique AID is an exact
// identity, and a DOM path is a positional identity. No similarity scoring —
// a heuristic third tier makes the output unstable across unrelated edits.
func diffElements(before, after []diffElement) ([]ElementChange, DiffSummary, bool) {
	matched := map[int]int{}
	takenAfter := map[int]bool{}
	// Tier 1: a unique AID is an exact identity that survives a move.
	matchDiffBy(before, after, matched, takenAfter, func(e diffElement) string {
		if e.aid == "" {
			return ""
		}
		return "aid:" + e.aid
	})
	// Tier 2: an unchanged element in document order. Only stamped elements carry
	// an AID (core.isStampableTag skips p, li, div, h*, span), so without this
	// tier prose falls straight to position and a single insertion re-pairs every
	// following sibling, reporting the whole tail as modified.
	matchDiffBySequence(before, after, matched, takenAfter)
	// Tier 3: position, for elements that genuinely changed content.
	matchDiffBy(before, after, matched, takenAfter, func(e diffElement) string {
		return "path:" + e.path
	})

	// Structural changes are collected by kind, then merged under the cap with
	// adds and removes first: an insertion or deletion is the highest-signal
	// change, and a single positional shift can produce enough modifications to
	// crowd it out of the response entirely.
	var modifications, insertions, deletions []ElementChange
	summary := DiffSummary{}

	for beforeIndex, element := range before {
		afterIndex, ok := matched[beforeIndex]
		if !ok {
			summary.Removed++
			deletions = append(deletions, ElementChange{
				Kind:       "removed",
				BeforeAID:  element.aid,
				DOMPath:    element.path,
				BeforePath: element.path,
				BeforeHTML: diffOuterHTML(element.node),
			})
			continue
		}
		counterpart := after[afterIndex]
		// Only a signature change is a content change. A matched element whose
		// signature is identical did not change; its DOM path may still differ
		// because a sibling was inserted or removed earlier in the document, and
		// reporting that as "modified" turns one insertion into a modification per
		// following sibling. The insertion itself is already reported, and the
		// shift is derivable from before_path/after_path on the changes that
		// remain.
		if element.signature == counterpart.signature {
			continue
		}
		summary.Modified++
		modifications = append(modifications, ElementChange{
			Kind:       "modified",
			BeforeAID:  element.aid,
			AfterAID:   counterpart.aid,
			DOMPath:    counterpart.path,
			BeforePath: element.path,
			AfterPath:  counterpart.path,
			BeforeHTML: diffOuterHTML(element.node),
			AfterHTML:  diffOuterHTML(counterpart.node),
		})
	}
	for afterIndex, element := range after {
		if takenAfter[afterIndex] {
			continue
		}
		summary.Added++
		insertions = append(insertions, ElementChange{
			Kind:      "added",
			AfterAID:  element.aid,
			DOMPath:   element.path,
			AfterPath: element.path,
			AfterHTML: diffOuterHTML(element.node),
		})
	}
	changes, truncated := mergeDiffChanges(deletions, insertions, modifications)
	return changes, summary, truncated
}

// mergeDiffChanges fills the change cap with adds and removes before
// modifications, then restores document order so the response still reads
// top-to-bottom.
func mergeDiffChanges(deletions, insertions, modifications []ElementChange) ([]ElementChange, bool) {
	capacity := maxDiffChanges
	total := len(deletions) + len(insertions) + len(modifications)
	changes := make([]ElementChange, 0, min(total, capacity))
	take := func(source []ElementChange) {
		for _, change := range source {
			if len(changes) >= capacity {
				return
			}
			changes = append(changes, change)
		}
	}
	take(deletions)
	take(insertions)
	take(modifications)
	slices.SortStableFunc(changes, func(a, b ElementChange) int {
		return strings.Compare(diffChangeOrderKey(a), diffChangeOrderKey(b))
	})
	return changes, total > len(changes)
}

// diffChangeOrderKey sorts on the path the change is anchored at: the new
// position when there is one, otherwise the old.
func diffChangeOrderKey(change ElementChange) string {
	if change.AfterPath != "" {
		return change.AfterPath
	}
	return change.BeforePath
}

// matchDiffBy pairs still-unmatched elements whose key is unique on both sides.
// A key occurring more than once is ambiguous and is left to the next tier.
func matchDiffBy(before, after []diffElement, matched map[int]int, takenAfter map[int]bool, key func(diffElement) string) {
	beforeIndex := uniqueDiffKeys(before, key, func(i int) bool { _, ok := matched[i]; return ok })
	afterIndex := uniqueDiffKeys(after, key, func(i int) bool { return takenAfter[i] })
	for k, bi := range beforeIndex {
		ai, ok := afterIndex[k]
		if !ok {
			continue
		}
		matched[bi] = ai
		takenAfter[ai] = true
	}
}

// matchDiffBySequence pairs still-unmatched elements by an exact signature match
// in document order, using the same hashed-line LCS the source layer uses. This
// is deterministic — it is an order-preserving match on exact equality, not a
// similarity score — so an insertion shifts nothing after it.
func matchDiffBySequence(before, after []diffElement, matched map[int]int, takenAfter map[int]bool) {
	beforeOpen := openDiffIndices(len(before), func(i int) bool { _, ok := matched[i]; return ok })
	afterOpen := openDiffIndices(len(after), func(i int) bool { return takenAfter[i] })
	if len(beforeOpen) == 0 || len(afterOpen) == 0 {
		return
	}
	symbols := map[string]rune{}
	encode := func(elements []diffElement, open []int) string {
		var b strings.Builder
		for _, index := range open {
			key := elements[index].signature
			symbol, ok := symbols[key]
			if !ok {
				// Stay inside the BMP and skip surrogates so every symbol is one rune.
				next := rune(0xE000 + len(symbols))
				if next > 0xF8FF {
					// Signature alphabet exhausted; leave the rest to the positional tier.
					return b.String()
				}
				symbol = next
				symbols[key] = symbol
			}
			b.WriteRune(symbol)
		}
		return b.String()
	}
	beforeText := encode(before, beforeOpen)
	afterText := encode(after, afterOpen)
	matcher := diffmatchpatch.New()
	matcher.DiffTimeout = diffTimeout
	beforeCursor, afterCursor := 0, 0
	for _, diff := range matcher.DiffMain(beforeText, afterText, false) {
		count := len([]rune(diff.Text))
		switch diff.Type {
		case diffmatchpatch.DiffEqual:
			for i := 0; i < count; i++ {
				if beforeCursor >= len(beforeOpen) || afterCursor >= len(afterOpen) {
					break
				}
				beforeIndex, afterIndex := beforeOpen[beforeCursor], afterOpen[afterCursor]
				matched[beforeIndex] = afterIndex
				takenAfter[afterIndex] = true
				beforeCursor++
				afterCursor++
			}
		case diffmatchpatch.DiffDelete:
			beforeCursor += count
		case diffmatchpatch.DiffInsert:
			afterCursor += count
		}
	}
}

func openDiffIndices(length int, taken func(int) bool) []int {
	open := make([]int, 0, length)
	for i := 0; i < length; i++ {
		if !taken(i) {
			open = append(open, i)
		}
	}
	return open
}

func uniqueDiffKeys(elements []diffElement, key func(diffElement) string, skip func(int) bool) map[string]int {
	index := map[string]int{}
	duplicated := map[string]bool{}
	for i, element := range elements {
		if skip(i) {
			continue
		}
		k := key(element)
		if k == "" {
			continue
		}
		if _, seen := index[k]; seen {
			duplicated[k] = true
			continue
		}
		index[k] = i
	}
	for k := range duplicated {
		delete(index, k)
	}
	return index
}

// diffSignature identifies an element by tag, attributes, own text and child tag
// order — everything the element itself owns, without its descendants' content,
// so one deep edit reports one change instead of one per ancestor.
func diffSignature(node *xhtml.Node) string {
	var builder strings.Builder
	// Every part is length-prefixed. Joining with plain delimiters let an
	// attribute value containing the delimiters forge a different element's
	// signature, which silently reported a real change as no change.
	writeDiffPart(&builder, 'n', node.Namespace)
	writeDiffPart(&builder, 't', diffTagName(node))
	names := make([]string, 0, len(node.Attr))
	values := map[string]string{}
	for _, attr := range node.Attr {
		if attr.Key == aidAttr {
			// The AID is a content hash: it changes with the content it identifies,
			// so including it would report every edited element twice.
			continue
		}
		name := attr.Namespace + ":" + attr.Key
		names = append(names, name)
		values[name] = attr.Val
	}
	slices.Sort(names)
	for _, name := range names {
		writeDiffPart(&builder, 'a', name)
		writeDiffPart(&builder, 'v', values[name])
	}
	literal := isLiteralTextTag(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		switch child.Type {
		case xhtml.ElementNode:
			writeDiffPart(&builder, 'e', diffTagName(child))
		case xhtml.TextNode:
			text := child.Data
			if !literal {
				// Two different things look alike here. A whitespace-ONLY node is
				// markup indentation between block children and is dropped, so a
				// reflow of the source stays out of this layer. A node with real
				// text keeps its leading and trailing space, because that space
				// separates it from an inline sibling: "Hello <em>x</em>" and
				// "Hello<em>x</em>" render differently. Known limit of that rule:
				// whitespace that is the node's ENTIRE content is dropped even
				// between two inline siblings, because it is indistinguishable
				// from indentation without resolving layout. The source layer
				// reports it.
				text = collapseASCIIWhitespace(text)
				if strings.TrimSpace(text) == "" {
					continue
				}
			}
			writeDiffPart(&builder, '#', text)
		case xhtml.CommentNode:
			writeDiffPart(&builder, '!', child.Data)
		case xhtml.DoctypeNode, xhtml.DocumentNode, xhtml.ErrorNode, xhtml.RawNode:
		}
	}
	return builder.String()
}

// writeDiffPart appends one length-prefixed, kind-tagged field.
func writeDiffPart(builder *strings.Builder, kind byte, value string) {
	builder.WriteByte(kind)
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
}

// isLiteralTextTag reports whether the element's text children are source bytes
// rather than prose: pre and textarea preserve whitespace, script/style/title
// are parsed as raw or escapable text. Whitespace there is meaningful.
//
// Out of scope by design: a CSS `white-space` rule can make any element
// preserving. Emulating the cascade here would put a stylesheet interpreter in
// the diff path; the source layer already reports those bytes exactly.
func isLiteralTextTag(node *xhtml.Node) bool {
	if node.Namespace != "" {
		return false
	}
	switch node.DataAtom {
	case atom.Pre, atom.Textarea, atom.Script, atom.Style, atom.Title:
		return true
	default:
		return false
	}
}

// collapseASCIIWhitespace reduces every whitespace run to a single space and
// keeps leading and trailing runs as one space each.
//
// The edges matter: this text node's neighbours may be inline elements, where
// "Hello <em>x</em>" and "Hello<em>x</em>" render differently. Trimming the
// edges made that edit invisible to the structural layer. A node that is
// entirely whitespace collapses to a single space and its caller drops it only
// when it has no siblings to separate.
func collapseASCIIWhitespace(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	space := false
	for i := 0; i < len(value); i++ {
		if isASCIIWhitespaceByte(value[i]) {
			space = true
			continue
		}
		if space {
			builder.WriteByte(' ')
		}
		space = false
		builder.WriteByte(value[i])
	}
	if space {
		builder.WriteByte(' ')
	}
	return builder.String()
}

func isASCIIWhitespaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\f' || b == '\r'
}

func diffTagName(node *xhtml.Node) string {
	if node.Namespace != "" {
		return node.Namespace + ":" + node.Data
	}
	return node.Data
}

func diffAttr(node *xhtml.Node, name string) string {
	for _, attr := range node.Attr {
		if attr.Namespace == "" && attr.Key == name {
			return attr.Val
		}
	}
	return ""
}

// diffOuterHTML re-renders the element for display. Render output is normalized
// markup, not the original bytes — the source layer is where bytes are shown.
func diffOuterHTML(node *xhtml.Node) string {
	var builder strings.Builder
	if err := xhtml.Render(&builder, node); err != nil {
		return ""
	}
	return truncateUTF8(builder.String(), maxDiffSnippetBytes)
}

// diffSourceLines compares the published bytes line by line, with no
// normalization at all: an indentation-only edit is a real edit here.
//
// The line-level compare runs on hashed lines (DiffLinesToChars), so the O(ND)
// search sees one symbol per line instead of one per byte, and diffBisect keeps
// only two rolling frontier arrays — memory is O(N+M) in lines regardless of how
// different the two documents are. diffTimeout bounds the search itself: on
// expiry the library returns a whole-file replace rather than running to
// completion, so a pathological pair degrades in fidelity, not in liveness.
func diffSourceLines(before, after string) ([]CodeHunk, bool, error) {
	if len(before)+len(after) > maxDiffSourceBytes {
		return nil, false, errDiffLimit
	}
	ops := sourceLineOps(before, after)
	return groupSourceHunks(ops)
}

// sourceLineOp is one line tagged with its edit kind, in output order.
type sourceLineOp struct {
	kind    diffLineKind
	content string
}

type diffLineKind int

const (
	diffLineEqual diffLineKind = iota
	diffLineDelete
	diffLineInsert
)

func sourceLineOps(before, after string) []sourceLineOp {
	matcher := diffmatchpatch.New()
	matcher.DiffTimeout = diffTimeout
	beforeChars, afterChars, lines := matcher.DiffLinesToChars(before, after)
	diffs := matcher.DiffCharsToLines(matcher.DiffMain(beforeChars, afterChars, false), lines)
	ops := make([]sourceLineOp, 0, 64)
	for _, diff := range diffs {
		var kind diffLineKind
		switch diff.Type {
		case diffmatchpatch.DiffDelete:
			kind = diffLineDelete
		case diffmatchpatch.DiffInsert:
			kind = diffLineInsert
		case diffmatchpatch.DiffEqual:
			kind = diffLineEqual
		}
		for _, line := range splitKeepingLines(diff.Text) {
			ops = append(ops, sourceLineOp{kind: kind, content: line})
		}
	}
	return ops
}

// splitKeepingLines splits on "\n" without inventing a trailing empty line for
// text that ends in a newline, so line numbering matches the file.
func splitKeepingLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// groupSourceHunks turns the flat op list into unified hunks with at most
// diffContextLines of context on each side, coalescing runs that would overlap.
//
// The hunk-line budget is applied while a hunk is being built, so an oversized
// run is abandoned as it is found rather than after the whole hunk is
// materialized.
func groupSourceHunks(ops []sourceLineOp) ([]CodeHunk, bool, error) {
	hunks := make([]CodeHunk, 0, 8)
	truncated := false
	oldLine, newLine := 1, 1
	index := 0
	for index < len(ops) {
		if ops[index].kind == diffLineEqual {
			oldLine++
			newLine++
			index++
			continue
		}
		// Back up over the leading context, then emit until the trailing context
		// of the last change in this cluster.
		start := index
		contextBefore := 0
		for start > 0 && ops[start-1].kind == diffLineEqual && contextBefore < diffContextLines {
			start--
			contextBefore++
		}
		hunk := CodeHunk{OldStart: oldLine - contextBefore, NewStart: newLine - contextBefore}
		if hunk.OldStart < 1 {
			hunk.OldStart = 1
		}
		if hunk.NewStart < 1 {
			hunk.NewStart = 1
		}
		cursor := start
		trailing := 0
		overflow := false
		for cursor < len(ops) {
			op := ops[cursor]
			if op.kind == diffLineEqual {
				trailing++
				// Two changes closer than 2*context share one hunk; beyond that
				// the run is over.
				if trailing > diffContextLines && !changeWithin(ops, cursor, diffContextLines+1) {
					break
				}
			} else {
				trailing = 0
			}
			if len(hunk.Lines) >= maxDiffHunkLines {
				overflow = true
				break
			}
			switch op.kind {
			case diffLineDelete:
				hunk.OldLines++
				hunk.Lines = append(hunk.Lines, "-"+op.content)
			case diffLineInsert:
				hunk.NewLines++
				hunk.Lines = append(hunk.Lines, "+"+op.content)
			case diffLineEqual:
				hunk.OldLines++
				hunk.NewLines++
				hunk.Lines = append(hunk.Lines, " "+op.content)
			}
			cursor++
		}
		// Advance the running line numbers across everything this hunk consumed.
		for i := start; i < cursor; i++ {
			switch ops[i].kind {
			case diffLineDelete:
				oldLine++
			case diffLineInsert:
				newLine++
			case diffLineEqual:
				oldLine++
				newLine++
			}
		}
		if overflow {
			truncated = true
			// Keep the capped prefix rather than dropping the hunk: a document
			// rewritten end to end is one enormous run, and discarding it returned
			// an empty code_hunks for the most drastic possible edit. The emitted
			// lines are contiguous from OldStart/NewStart and OldLines/NewLines
			// count only what was emitted, so this hunk is self-consistent; the
			// running counters below skip the remainder, so later hunks stay
			// correctly numbered.
			if len(hunk.Lines) > 0 {
				hunks = append(hunks, hunk)
			}
			for cursor < len(ops) && ops[cursor].kind != diffLineEqual {
				switch ops[cursor].kind {
				case diffLineDelete:
					oldLine++
				case diffLineInsert:
					newLine++
				case diffLineEqual:
				}
				cursor++
			}
		} else if len(hunk.Lines) > 0 {
			hunks = append(hunks, hunk)
		}
		if cursor == index {
			// Defensive: never fail to advance, a stalled loop would hang the request.
			cursor++
		}
		index = cursor
	}
	return hunks, truncated, nil
}

// changeWithin reports whether a non-equal op occurs within limit ops of from.
func changeWithin(ops []sourceLineOp, from, limit int) bool {
	for i := from; i < len(ops) && i < from+limit; i++ {
		if ops[i].kind != diffLineEqual {
			return true
		}
	}
	return false
}

// trimDiffOutput drops trailing detail until the response fits the wire budget.
// Snippets go first: a change without its HTML is still navigable by DOM path.
//
// Sizes are computed once into running totals and decremented as items are
// dropped. Re-measuring the whole result after every single pop made this
// quadratic on exactly the large payloads that reach it.
func trimDiffOutput(result *VersionDiff) {
	changeSizes := make([]int, len(result.Changes))
	total := 0
	for i, change := range result.Changes {
		changeSizes[i] = diffChangeSize(change)
		total += changeSizes[i]
	}
	hunkSizes := make([]int, len(result.CodeHunks))
	for i, hunk := range result.CodeHunks {
		hunkSizes[i] = diffHunkSize(hunk)
		total += hunkSizes[i]
	}
	if total <= maxDiffOutputBytes {
		return
	}
	result.Truncated = true
	for i := range result.Changes {
		total -= diffJSONSize(result.Changes[i].BeforeHTML) + diffJSONSize(result.Changes[i].AfterHTML)
		changeSizes[i] = diffChangeSize(result.Changes[i]) -
			diffJSONSize(result.Changes[i].BeforeHTML) - diffJSONSize(result.Changes[i].AfterHTML)
		result.Changes[i].BeforeHTML = ""
		result.Changes[i].AfterHTML = ""
	}
	for total > maxDiffOutputBytes && len(result.CodeHunks) > 0 {
		last := len(result.CodeHunks) - 1
		total -= hunkSizes[last]
		result.CodeHunks = result.CodeHunks[:last]
		hunkSizes = hunkSizes[:last]
	}
	for total > maxDiffOutputBytes && len(result.Changes) > 0 {
		last := len(result.Changes) - 1
		total -= changeSizes[last]
		result.Changes = result.Changes[:last]
		changeSizes = changeSizes[:last]
	}
}

func diffOutputSize(result *VersionDiff) int {
	size := 0
	for _, change := range result.Changes {
		size += diffChangeSize(change)
	}
	for _, hunk := range result.CodeHunks {
		size += diffHunkSize(hunk)
	}
	return size
}

func diffChangeSize(change ElementChange) int {
	return diffJSONSize(change.Kind) + diffJSONSize(change.BeforeAID) + diffJSONSize(change.AfterAID) +
		diffJSONSize(change.DOMPath) + diffJSONSize(change.BeforePath) + diffJSONSize(change.AfterPath) +
		diffJSONSize(change.BeforeHTML) + diffJSONSize(change.AfterHTML)
}

func diffHunkSize(hunk CodeHunk) int {
	size := 0
	for _, line := range hunk.Lines {
		size += diffJSONSize(line)
	}
	return size
}

// diffJSONSize is the encoded length of a string field, not its raw length.
// The response encoder escapes HTML (`<` becomes `\u003c`), and this payload is
// mostly HTML, so measuring raw bytes let the wire response run several times
// past the budget.
func diffJSONSize(value string) int {
	size := 0
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '<', '>', '&':
			size += 6
		case '"', '\\', '\n', '\r', '	':
			size += 2
		default:
			if value[i] < 0x20 {
				size += 6
			} else {
				size++
			}
		}
	}
	return size
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && value[cut]&0xC0 == 0x80 {
		cut--
	}
	return value[:cut]
}

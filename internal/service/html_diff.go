package service

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var errDiffLimit = errors.New("diff complexity limit exceeded")

const (
	maxDiffNodes        = 8000
	maxDiffChanges      = 1000
	maxDiffSourceLines  = 20000
	maxDiffHunkLines    = 2000
	maxDiffSnippetBytes = 8 << 10
	maxDiffPathBytes    = 1 << 10
	maxDiffOutputBytes  = 512 << 10
	diffContextLines    = 3

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
	matchDiffBy(before, after, matched, takenAfter, func(e diffElement) string {
		if e.aid == "" {
			return ""
		}
		return "aid:" + e.aid
	})
	matchDiffBy(before, after, matched, takenAfter, func(e diffElement) string {
		return "path:" + e.path
	})

	changes := make([]ElementChange, 0, 16)
	summary := DiffSummary{}
	truncated := false
	appendChange := func(change ElementChange) {
		if len(changes) >= maxDiffChanges {
			truncated = true
			return
		}
		changes = append(changes, change)
	}

	for beforeIndex, element := range before {
		afterIndex, ok := matched[beforeIndex]
		if !ok {
			summary.Removed++
			appendChange(ElementChange{
				Kind:       "removed",
				BeforeAID:  element.aid,
				DOMPath:    element.path,
				BeforePath: element.path,
				BeforeHTML: diffOuterHTML(element.node),
			})
			continue
		}
		counterpart := after[afterIndex]
		if element.signature == counterpart.signature && element.path == counterpart.path {
			continue
		}
		summary.Modified++
		appendChange(ElementChange{
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
		appendChange(ElementChange{
			Kind:      "added",
			AfterAID:  element.aid,
			DOMPath:   element.path,
			AfterPath: element.path,
			AfterHTML: diffOuterHTML(element.node),
		})
	}
	return changes, summary, truncated
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
	builder.WriteString(node.Namespace)
	builder.WriteByte('|')
	builder.WriteString(diffTagName(node))
	names := make([]string, 0, len(node.Attr))
	values := map[string]string{}
	for _, attr := range node.Attr {
		name := attr.Namespace + ":" + attr.Key
		if attr.Key == aidAttr {
			// The AID is a content hash: it changes with the content it identifies,
			// so including it would report every edited element twice.
			continue
		}
		names = append(names, name)
		values[name] = attr.Val
	}
	slices.Sort(names)
	for _, name := range names {
		builder.WriteByte('|')
		builder.WriteString(name)
		builder.WriteByte('=')
		builder.WriteString(values[name])
	}
	literal := isLiteralTextTag(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		switch child.Type {
		case xhtml.ElementNode:
			builder.WriteString("|<")
			builder.WriteString(diffTagName(child))
		case xhtml.TextNode:
			text := child.Data
			if !literal {
				// Markup indentation is not content. Collapsing it keeps a reflow
				// of the source out of the structural layer; the source layer still
				// reports it byte for byte.
				text = collapseASCIIWhitespace(text)
				if text == "" {
					continue
				}
			}
			builder.WriteString("|#")
			builder.WriteString(text)
		case xhtml.CommentNode:
			builder.WriteString("|!")
			builder.WriteString(child.Data)
		case xhtml.DoctypeNode, xhtml.DocumentNode, xhtml.ErrorNode, xhtml.RawNode:
		}
	}
	return builder.String()
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

func collapseASCIIWhitespace(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	space := false
	for i := 0; i < len(value); i++ {
		if isASCIIWhitespaceByte(value[i]) {
			space = true
			continue
		}
		if space && builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		space = false
		builder.WriteByte(value[i])
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
func diffSourceLines(before, after string) ([]CodeHunk, bool, error) {
	beforeLines := strings.Count(before, "\n") + 1
	afterLines := strings.Count(after, "\n") + 1
	if beforeLines > maxDiffSourceLines || afterLines > maxDiffSourceLines {
		return nil, false, errDiffLimit
	}
	edits := myers.ComputeEdits(span.URIFromPath("before"), before, after)
	unified := gotextdiff.ToUnified("before", "after", before, edits)
	hunks := make([]CodeHunk, 0, len(unified.Hunks))
	truncated := false
	for _, hunk := range unified.Hunks {
		converted, ok := toCodeHunk(hunk)
		if !ok {
			truncated = true
			continue
		}
		hunks = append(hunks, converted)
	}
	return hunks, truncated, nil
}

func toCodeHunk(hunk *gotextdiff.Hunk) (CodeHunk, bool) {
	if len(hunk.Lines) > maxDiffHunkLines {
		return CodeHunk{}, false
	}
	result := CodeHunk{OldStart: hunk.FromLine, NewStart: hunk.ToLine, Lines: make([]string, 0, len(hunk.Lines))}
	for _, line := range hunk.Lines {
		content := strings.TrimSuffix(line.Content, "\n")
		switch line.Kind {
		case gotextdiff.Delete:
			result.OldLines++
			result.Lines = append(result.Lines, "-"+content)
		case gotextdiff.Insert:
			result.NewLines++
			result.Lines = append(result.Lines, "+"+content)
		default:
			result.OldLines++
			result.NewLines++
			result.Lines = append(result.Lines, " "+content)
		}
	}
	return result, true
}

// trimDiffOutput drops trailing detail until the response fits the wire budget.
// Snippets go first: a change without its HTML is still navigable by DOM path.
func trimDiffOutput(result *VersionDiff) {
	if diffOutputSize(result) <= maxDiffOutputBytes {
		return
	}
	for i := range result.Changes {
		result.Changes[i].BeforeHTML = ""
		result.Changes[i].AfterHTML = ""
	}
	result.Truncated = true
	for diffOutputSize(result) > maxDiffOutputBytes && len(result.CodeHunks) > 0 {
		result.CodeHunks = result.CodeHunks[:len(result.CodeHunks)-1]
	}
	for diffOutputSize(result) > maxDiffOutputBytes && len(result.Changes) > 0 {
		result.Changes = result.Changes[:len(result.Changes)-1]
	}
}

func diffOutputSize(result *VersionDiff) int {
	size := 0
	for _, change := range result.Changes {
		size += len(change.Kind) + len(change.BeforeAID) + len(change.AfterAID) +
			len(change.DOMPath) + len(change.BeforePath) + len(change.AfterPath) +
			len(change.BeforeHTML) + len(change.AfterHTML)
	}
	for _, hunk := range result.CodeHunks {
		for _, line := range hunk.Lines {
			size += len(line)
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

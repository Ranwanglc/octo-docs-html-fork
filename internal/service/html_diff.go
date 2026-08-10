package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"html"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
	xhtml "golang.org/x/net/html"
)

var errDiffLimit = errors.New("diff complexity limit exceeded")

const (
	maxDiffNodes        = 4000
	maxDiffDepth        = 256
	maxDiffComparisons  = 200000
	maxDiffCompareBytes = 16 << 20
	maxDiffCompareText  = 4096
	maxDiffRawScanBytes = 16 << 20
	maxDiffChanges      = 1000
	maxDiffInputLines   = 12000
	maxDiffHunkLines    = 2000
	maxDiffTagBytes     = 256
	maxDiffPathBytes    = 16 << 10
	maxDiffPathsBytes   = 2 << 20
	maxDiffSnippetBytes = 8 << 10
	maxDiffOpeningBytes = 1024
	maxDiffOutputBytes  = 512 << 10
	diffContextLines    = 3
)

// VersionDiff is a bounded structural and source-level comparison of two HTML versions.
type VersionDiff struct {
	From      int             `json:"from"`
	To        int             `json:"to"`
	Summary   DiffSummary     `json:"summary"`
	Changes   []ElementChange `json:"changes"`
	CodeHunks []CodeHunk      `json:"code_hunks"`
}

// DiffSummary counts bounded element-level changes by kind.
type DiffSummary struct {
	Added    int `json:"added"`
	Removed  int `json:"removed"`
	Modified int `json:"modified"`
}

// ElementChange describes one matched, added, or removed local DOM subtree.
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

// CodeHunk is a normalized unified-style HTML source hunk.
type CodeHunk struct {
	OldStart int      `json:"old_start"`
	OldLines int      `json:"old_lines"`
	NewStart int      `json:"new_start"`
	NewLines int      `json:"new_lines"`
	Lines    []string `json:"lines"`
}

type htmlDiffNode struct {
	tag         string
	aid         string
	attrs       map[string]string
	text        string
	textParts   []string
	textBounds  []int
	textDigest  string
	compareText string
	signature   string
	path        string
	element     *xhtml.Node
	literalText bool
	parent      int
	children    []int
	childTags   []string
	siblingPos  int
	order       int
}

type diffHTMLToken struct {
	type_      xhtml.TokenType
	raw        string
	text       string
	start, end int
	tag        string
	attrs      map[string]string
	rawTextTag string
	namespace  string
}

type diffNamespaceEntry struct {
	tag, namespace string
	integration    bool
}

// scanDiffHTML tokenizes original bytes for code hunks; it does not infer DOM structure.
func scanDiffHTML(source string, visit func(diffHTMLToken) error) error {
	z := xhtml.NewTokenizer(strings.NewReader(source))
	z.SetMaxBuf(maxDiffRawScanBytes)
	offset := 0
	stack := make([]diffNamespaceEntry, 0, 32)
	for {
		z.AllowCDATA(len(stack) > 0 && stack[len(stack)-1].namespace != "")
		type_ := z.Next()
		if type_ == xhtml.ErrorToken {
			if err := z.Err(); err != nil && !errors.Is(err, io.EOF) {
				return errDiffLimit
			}
			if tailBytes := z.Raw(); len(tailBytes) != 0 {
				if len(tailBytes) > len(source)-offset {
					return errDiffLimit
				}
				tail := source[offset : offset+len(tailBytes)]
				if err := visit(diffHTMLToken{type_: xhtml.TextToken, raw: tail, text: string(z.Text()), start: offset, end: offset + len(tail)}); err != nil {
					return err
				}
				offset += len(tail)
			}
			break
		}
		rawBytes := z.Raw()
		if len(rawBytes) > len(source)-offset {
			return errDiffLimit
		}
		raw := source[offset : offset+len(rawBytes)]
		token := diffHTMLToken{type_: type_, raw: raw, start: offset, end: offset + len(raw)}
		offset = token.end
		if type_ == xhtml.TextToken {
			token.text = string(z.Text())
		}
		if type_ == xhtml.StartTagToken || type_ == xhtml.SelfClosingTagToken || type_ == xhtml.EndTagToken {
			name, more := z.TagName()
			token.tag = string(name)
			token.attrs = map[string]string{}
			for more {
				key, value, next := z.TagAttr()
				if _, exists := token.attrs[string(key)]; !exists {
					token.attrs[string(key)] = string(value)
				}
				more = next
			}
		}
		if type_ == xhtml.TextToken && len(stack) > 0 && stack[len(stack)-1].namespace == "" && isDiffRawTextTag(stack[len(stack)-1].tag) {
			token.rawTextTag = stack[len(stack)-1].tag
		}
		if type_ == xhtml.StartTagToken || type_ == xhtml.SelfClosingTagToken {
			foreign := diffTokenInForeignContent(stack, type_, token.tag)
			if foreign && diffForeignBreakout(token.tag, token.attrs) {
				for len(stack) > 0 {
					top := stack[len(stack)-1]
					if top.namespace == "" || top.integration {
						break
					}
					stack = stack[:len(stack)-1]
				}
				foreign = false
			}
			namespace := ""
			if foreign && len(stack) > 0 {
				namespace = stack[len(stack)-1].namespace
			}
			if namespace == "" {
				switch token.tag {
				case "svg":
					namespace = "svg"
				case "math":
					namespace = "math"
				}
			}
			token.namespace = namespace
			entry := diffNamespaceEntry{tag: token.tag, namespace: namespace}
			entry.integration = diffHTMLIntegrationPoint(entry, token.attrs)
			selfClosing := namespace != "" && type_ == xhtml.SelfClosingTagToken || namespace == "" && isDiffVoidTag(token.tag)
			if foreign {
				z.NextIsNotRawText()
			}
			if !selfClosing {
				stack = append(stack, entry)
			}
		} else if type_ == xhtml.EndTagToken {
			for index := len(stack) - 1; index >= 0; index-- {
				if stack[index].tag == token.tag {
					token.namespace = stack[index].namespace
					stack = stack[:index]
					break
				}
			}
		}
		if err := visit(token); err != nil {
			return err
		}
	}
	if offset != len(source) {
		return errDiffLimit
	}
	return nil
}

func buildVersionDiff(fromVersion, toVersion int, before, after string) (*VersionDiff, error) {
	if before == after {
		return &VersionDiff{From: fromVersion, To: toVersion, Changes: []ElementChange{}, CodeHunks: []CodeHunk{}}, nil
	}
	beforeNodes, beforeLines, err := scanDiffVersion(before)
	if err != nil {
		return nil, err
	}
	afterNodes, afterLines, err := scanDiffVersion(after)
	if err != nil {
		return nil, err
	}
	matches, err := matchDiffNodes(beforeNodes, afterNodes)
	if err != nil {
		return nil, err
	}
	matchedAfter := make(map[int]int, len(matches))
	changes := make([]ElementChange, 0)
	for beforeIndex, afterIndex := range matches {
		matchedAfter[afterIndex] = beforeIndex
		beforeNode, afterNode := beforeNodes[beforeIndex], afterNodes[afterIndex]
		if diffNodeSignature(beforeNode) == diffNodeSignature(afterNode) {
			continue
		}
		changes = append(changes, ElementChange{
			Kind: "modified", BeforeAID: beforeNode.aid, AfterAID: afterNode.aid,
			DOMPath: afterNode.path, BeforePath: beforeNode.path, AfterPath: afterNode.path,
			BeforeHTML: diffNodeSnippet(beforeNode), AfterHTML: diffNodeSnippet(afterNode),
		})
	}
	for index, node := range beforeNodes {
		if _, ok := matches[index]; !ok {
			if isDiffWrapper(node.tag) {
				continue
			}
			if node.parent >= 0 {
				if _, parentMatched := matches[node.parent]; !parentMatched {
					continue
				}
			}
			changes = append(changes, ElementChange{Kind: "removed", BeforeAID: node.aid, DOMPath: node.path, BeforePath: node.path, BeforeHTML: diffNodeSnippet(node)})
		}
	}
	for index, node := range afterNodes {
		if _, ok := matchedAfter[index]; !ok {
			if isDiffWrapper(node.tag) {
				continue
			}
			if node.parent >= 0 {
				if _, parentMatched := matchedAfter[node.parent]; !parentMatched {
					continue
				}
			}
			changes = append(changes, ElementChange{Kind: "added", AfterAID: node.aid, DOMPath: node.path, AfterPath: node.path, AfterHTML: diffNodeSnippet(node)})
		}
	}
	if len(changes) > maxDiffChanges {
		return nil, errDiffLimit
	}
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].DOMPath == changes[j].DOMPath {
			return changes[i].Kind < changes[j].Kind
		}
		return changes[i].DOMPath < changes[j].DOMPath
	})
	hunks, err := diffCodeHunksLines(beforeLines, afterLines)
	if err != nil {
		return nil, err
	}
	result := &VersionDiff{From: fromVersion, To: toVersion, Changes: changes, CodeHunks: hunks}
	for _, change := range changes {
		switch change.Kind {
		case "added":
			result.Summary.Added++
		case "removed":
			result.Summary.Removed++
		case "modified":
			result.Summary.Modified++
		}
	}
	if diffOutputSize(result) > maxDiffOutputBytes {
		return nil, errDiffLimit
	}
	return result, nil
}

// scanDiffVersion keeps source-byte and parsed-DOM views independent.
func scanDiffVersion(source string) ([]htmlDiffNode, []diffSourceLine, error) {
	lines, err := scanDiffLines(source)
	if err != nil {
		return nil, nil, err
	}
	nodes, err := parseDiffHTML(source)
	if err != nil {
		return nil, nil, err
	}
	return nodes, lines, nil
}

func scanDiffLines(source string) ([]diffSourceLine, error) {
	consumer := newDiffLineConsumer(source)
	if err := scanDiffHTML(source, func(token diffHTMLToken) error {
		if !consumer.consume(token) {
			return errDiffLimit
		}
		return nil
	}); err != nil {
		return nil, err
	}
	lines, ok := consumer.finish()
	if !ok {
		return nil, errDiffLimit
	}
	return lines, nil
}

// parseDiffHTML uses the tree builder as the sole authority for document structure.
func parseDiffHTML(source string) ([]htmlDiffNode, error) {
	root, err := xhtml.Parse(strings.NewReader(source))
	if err != nil {
		return nil, errDiffLimit
	}
	builder := diffTreeBuilder{nodes: make([]htmlDiffNode, 0, 128), rootCounts: map[string]int{}, childCounts: map[int]map[string]int{}}
	if err := builder.appendChildren(root, -1, 0); err != nil {
		return nil, err
	}
	finalizeDiffNodes(builder.nodes)
	return builder.nodes, nil
}

// diffTreeBuilder flattens the parsed tree while enforcing path budgets.
type diffTreeBuilder struct {
	nodes       []htmlDiffNode
	rootCounts  map[string]int
	childCounts map[int]map[string]int
	pathBytes   int
}

// appendChildren skips non-structural nodes without merging adjacent text nodes.
func (builder *diffTreeBuilder) appendChildren(parentElement *xhtml.Node, parent, depth int) error {
	for child := parentElement.FirstChild; child != nil; child = child.NextSibling {
		switch child.Type {
		case xhtml.TextNode:
			if parent >= 0 {
				appendDiffNodeText(&builder.nodes[parent], child.Data)
			}
		case xhtml.ElementNode:
			index, err := builder.appendElement(child, parent, depth)
			if err != nil {
				return err
			}
			if err := builder.appendChildren(child, index, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (builder *diffTreeBuilder) appendElement(element *xhtml.Node, parent, depth int) (int, error) {
	if len(builder.nodes) >= maxDiffNodes || depth >= maxDiffDepth {
		return 0, errDiffLimit
	}
	tag := element.Data
	if len(tag) > maxDiffTagBytes {
		return 0, errDiffLimit
	}
	pathTag := diffPathTag(tag)
	counts := builder.rootCounts
	if parent >= 0 {
		counts = builder.childCounts[parent]
		if counts == nil {
			counts = map[string]int{}
			builder.childCounts[parent] = counts
		}
	}
	counts[pathTag]++
	segment := "/" + pathTag + fmt.Sprintf("[%d]", counts[pathTag])
	pathLen := len(segment)
	if parent >= 0 {
		pathLen += len(builder.nodes[parent].path)
	}
	if pathLen > maxDiffPathBytes || builder.pathBytes > maxDiffPathsBytes-pathLen {
		return 0, errDiffLimit
	}
	path := segment
	if parent >= 0 {
		path = builder.nodes[parent].path + segment
	}
	builder.pathBytes += pathLen
	siblingPos := counts[pathTag] - 1
	if parent >= 0 {
		siblingPos = len(builder.nodes[parent].children)
	}
	attrs := diffElementAttrs(element)
	node := htmlDiffNode{
		tag: tag, aid: attrs["data-odoc-aid"], attrs: attrs, path: path,
		element: element, literalText: element.Namespace == "" && isDiffLiteralRawTextTag(tag),
		parent: parent, siblingPos: siblingPos, order: len(builder.nodes),
	}
	builder.nodes = append(builder.nodes, node)
	index := len(builder.nodes) - 1
	if parent >= 0 {
		builder.nodes[parent].textBounds = append(builder.nodes[parent].textBounds, len(builder.nodes[parent].textParts))
		builder.nodes[parent].children = append(builder.nodes[parent].children, index)
		builder.nodes[parent].childTags = append(builder.nodes[parent].childTags, tag)
	}
	return index, nil
}

// diffElementAttrs keeps namespaces so xlink:href and href remain distinct.
func diffElementAttrs(element *xhtml.Node) map[string]string {
	attrs := make(map[string]string, len(element.Attr))
	for _, attr := range element.Attr {
		key := attr.Key
		if attr.Namespace != "" {
			key = attr.Namespace + ":" + attr.Key
		}
		if _, exists := attrs[key]; !exists {
			attrs[key] = attr.Val
		}
	}
	return attrs
}

func finalizeDiffNodes(nodes []htmlDiffNode) {
	preformattedNode := make([]whiteSpaceMode, len(nodes))
	for index := range nodes {
		mode := whiteSpaceCollapse
		if parent := nodes[index].parent; parent >= 0 {
			mode = preformattedNode[parent]
		}
		if nodes[index].tag == "pre" || nodes[index].tag == "textarea" {
			mode = whiteSpacePreserve
		}
		if styleMode, specified := inlineWhiteSpaceMode(nodes[index].attrs["style"]); specified {
			mode = styleMode
		}
		preformattedNode[index] = mode
	}
	for index := range nodes {
		literalRawText := nodes[index].literalText
		whiteSpace := preformattedNode[index]
		preserveWhitespace := whiteSpace == whiteSpacePreserve
		fullText := ""
		textDigest := sha256.New()
		if literalRawText {
			fullText = strings.Join(nodes[index].textParts, "")
			writeDiffFrame(textDigest, fullText)
		} else {
			parts := append([]string(nil), nodes[index].textParts...)
			bounds := append(append([]int(nil), nodes[index].textBounds...), len(parts))
			start := 0
			var fullTextBuilder strings.Builder
			for slot, end := range bounds {
				segment := strings.Join(parts[start:end], "")
				start = end
				fullTextBuilder.WriteString(segment)
				normalizedSegment := segment
				switch whiteSpace {
				case whiteSpacePreserve:
				case whiteSpacePreserveLines:
					normalizedSegment = normalizeDiffTextSegmentKeepLines(segment)
				default:
					normalizedSegment = normalizeDiffTextSegment(segment)
				}
				if normalizedSegment == "" || !preserveWhitespace && isOnlyHTMLASCIIWhitespace(segment) && !strings.Contains(normalizedSegment, "\n") && !diffSlotWhitespaceVisible(nodes[index], slot) {
					continue
				}
				writeDiffFrame(textDigest, strconv.Itoa(slot))
				writeDiffFrame(textDigest, normalizedSegment)
			}
			fullText = fullTextBuilder.String()
			switch whiteSpace {
			case whiteSpacePreserve:
			case whiteSpacePreserveLines:
				fullText = collapseHTMLASCIIWhitespaceKeepLines(fullText)
			default:
				fullText = collapseHTMLASCIIWhitespace(fullText)
			}
		}
		nodes[index].textParts = nil
		nodes[index].textBounds = nil
		nodes[index].childTags = nil
		nodes[index].compareText = normalizeCompareText(fullText)
		nodes[index].textDigest = fmt.Sprintf("%x", textDigest.Sum(nil))
		nodes[index].text = fullText
		if len(nodes[index].text) > maxDiffCompareText {
			nodes[index].text = truncateUTF8(nodes[index].text, maxDiffCompareText)
		}
		nodes[index].signature = computeDiffNodeSignature(nodes[index])
	}
}

func appendDiffNodeText(node *htmlDiffNode, text string) {
	if text == "" {
		return
	}
	// Keep chunks separate so entity decoding cannot cross comments or tags.
	// Finalization joins once, avoiding quadratic repeated concatenation.
	node.textParts = append(node.textParts, text)
}

// isBlockLevelDiffTag reports whether tag renders as a block-like box whose
// surrounding ASCII whitespace collapses away visually (block, table, list,
// section, form, headings). Unknown/custom elements are conservatively treated
// as inline (returns false) so their surrounding whitespace stays significant.
func isBlockLevelDiffTag(tag string) bool {
	switch tag {
	case "address", "article", "aside", "blockquote", "details", "dialog", "dd", "div",
		"dl", "dt", "fieldset", "figcaption", "figure", "footer", "form", "h1", "h2",
		"h3", "h4", "h5", "h6", "header", "hgroup", "hr", "li", "main", "menu", "nav",
		"ol", "p", "pre", "search", "section", "summary", "ul",
		"table", "caption", "colgroup", "thead", "tbody", "tfoot", "tr", "td", "th",
		"optgroup", "option", "html", "head", "body":
		return true
	default:
		return false
	}
}

// boundaryInlineAttrs reports whether an element boundary is inline-level for
// inter-tag whitespace visibility in the source-hunk view: a non-block tag
// (default-inline, or unknown/custom treated as inline for safety) or a
// default-block tag carrying an inline display override. It shares
// isBlockLevelDiffTag with the structural-digest side so both layers classify
// boundaries identically. It does NOT consult document-global CSS: a
// document-wide <style>/stylesheet is not a per-boundary inline signal (that
// global gate would force every reindented block boundary visible and overflow
// the hunk budget); the global case is handled for newline-free runs in
// whitespaceRunVisible. attrs contains the tokenizer-decoded attributes.

// whitespaceRunVisible reports whether an inter-tag whitespace run can render as
// a visible space and therefore must surface in the source hunk. A run is
// visible when either neighbouring boundary is inline, regardless of the run's
// bytes. When only global layout CSS is present (a stylesheet could flip a
// block boundary inline) a run is visible only if it carries no newline: a
// deliberately inserted space between otherwise-block tags is significant,
// whereas a newline-bearing reindent run is the common pretty-print no-op and
// stays ignorable so large reindents do not overflow the hunk budget.
func whitespaceRunVisible(run string, prevInline, nextInline, layoutCSS bool) bool {
	if prevInline || nextInline {
		return true
	}
	if layoutCSS && !strings.ContainsAny(run, "\r\n") {
		return true
	}
	return false
}

// styleHasInlineDisplay reports whether an inline style sets display to an
// inline-level value (inline, inline-block, ...). It never parses full CSS; it
// scans display declarations conservatively.
func styleHasInlineDisplay(style string) bool {
	if style == "" {
		return false
	}
	style = strings.ToLower(style)
	for idx := strings.Index(style, "display"); idx >= 0; {
		value := style[idx+len("display"):]
		if colon := strings.IndexByte(value, ':'); colon >= 0 {
			value = value[colon+1:]
			if semi := strings.IndexByte(value, ';'); semi >= 0 {
				value = value[:semi]
			}
			if strings.Contains(value, "inline") {
				return true
			}
		}
		next := strings.Index(style[idx+len("display"):], "display")
		if next < 0 {
			break
		}
		idx += len("display") + next
	}
	return false
}

// hasLayoutAffectingCSS is a bounded, conservative scan for anything that could
// make inter-tag whitespace visible: a <style> element, a stylesheet <link>, or
// an inline style declaring display/white-space. It never parses CSS; it only
// decides whether newline-free block-boundary whitespace must stay provable
// rather than assumed safe. False positives merely keep such whitespace in the
// source hunk; a false negative would risk an empty diff, so the checks stay
// generous.
func hasLayoutAffectingCSS(source string) bool {
	scan := source
	if len(scan) > maxDiffRawScanBytes {
		scan = scan[:maxDiffRawScanBytes]
	}
	lower := strings.ToLower(scan)
	if strings.Contains(lower, "<style") || strings.Contains(lower, "stylesheet") {
		return true
	}
	for idx := strings.Index(lower, "style="); idx >= 0; {
		tail := lower[idx+len("style="):]
		value := tail
		if len(tail) > 0 && (tail[0] == '"' || tail[0] == '\'') {
			if end := strings.IndexByte(tail[1:], tail[0]); end >= 0 {
				value = tail[1 : 1+end]
			}
		} else if end := strings.IndexAny(tail, " \t\r\n>"); end >= 0 {
			value = tail[:end]
		}
		if strings.Contains(value, "display") || strings.Contains(value, "white-space") {
			return true
		}
		next := strings.Index(tail, "style=")
		if next < 0 {
			break
		}
		idx += len("style=") + next
	}
	return false
}

// diffSlotWhitespaceVisible reports whether a pure-whitespace text segment at
// boundary slot renders as a visible inline space. Slot s sits between child
// s-1 (left) and child s (right); a missing side is the parent's own edge,
// which is a block context. Whitespace is invisible reindent/pretty-print noise
// only when both neighbors are block-level, so it must not enter the digest.
// Any inline (or unknown/custom, treated inline) or void neighbor keeps it.
func diffSlotWhitespaceVisible(node htmlDiffNode, slot int) bool {
	return !diffBoundarySideIsBlock(node, slot-1) || !diffBoundarySideIsBlock(node, slot)
}

// diffBoundarySideIsBlock reports whether the child at childPos is block-level.
// An out-of-range position denotes the parent's own edge, so an inline or
// unknown/custom parent's edge counts as inline.
func diffBoundarySideIsBlock(node htmlDiffNode, childPos int) bool {
	if childPos < 0 || childPos >= len(node.children) {
		return isBlockLevelDiffTag(node.tag)
	}
	return isBlockLevelDiffTag(node.childTags[childPos])
}

func normalizeDiffTextSegment(value string) string {
	if value == "" {
		return ""
	}
	// HTML only folds ASCII whitespace; a leading/trailing ASCII-whitespace
	// boundary is preserved as a single space so adjacent segments do not merge.
	leading := isHTMLASCIIWhitespace(value[0])
	trailing := isHTMLASCIIWhitespace(value[len(value)-1])
	value = collapseHTMLASCIIWhitespace(value)
	if leading {
		value = " " + value
	}
	if trailing {
		value += " "
	}
	return value
}

// isHTMLASCIIWhitespace reports whether b is one of the HTML collapsible ASCII
// whitespace bytes (tab, LF, FF, CR, space). Visible Unicode whitespace such as
// NBSP (U+00A0) is intentionally excluded.
func isHTMLASCIIWhitespace(b byte) bool {
	switch b {
	case '\t', '\n', '\f', '\r', ' ':
		return true
	default:
		return false
	}
}

// collapseHTMLASCIIWhitespace folds runs of HTML ASCII whitespace to a single
// space and trims leading/trailing runs. It is linear and UTF-8 safe: multibyte
// runes (whose continuation bytes never match an ASCII whitespace byte) pass
// through untouched, so a literal U+00A0 never equals a plain space.
func collapseHTMLASCIIWhitespace(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	pendingSpace := false
	for i := 0; i < len(value); i++ {
		if isHTMLASCIIWhitespace(value[i]) {
			pendingSpace = builder.Len() > 0
			continue
		}
		if pendingSpace {
			builder.WriteByte(' ')
			pendingSpace = false
		}
		builder.WriteByte(value[i])
	}
	return builder.String()
}

// collapseHTMLASCIIWhitespaceKeepLines implements white-space: pre-line: spaces
// collapse, forced line breaks are preserved (including consecutive ones).
func collapseHTMLASCIIWhitespaceKeepLines(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	pending := false
	newlines := 0
	flush := func() {
		if !pending {
			return
		}
		switch {
		case newlines > 0:
			builder.WriteString(strings.Repeat("\n", newlines))
		case builder.Len() > 0:
			builder.WriteByte(' ')
		}
		pending, newlines = false, 0
	}
	for i := 0; i < len(value); i++ {
		if isHTMLASCIIWhitespace(value[i]) {
			// CRLF counts once.
			if value[i] == '\n' || (value[i] == '\r' && (i+1 == len(value) || value[i+1] != '\n')) {
				newlines++
			}
			pending = true
			continue
		}
		flush()
		builder.WriteByte(value[i])
	}
	flush()
	return builder.String()
}

// normalizeDiffTextSegmentKeepLines is normalizeDiffTextSegment for pre-line:
// boundary whitespace stays representable, newlines are not collapsed away.
func normalizeDiffTextSegmentKeepLines(value string) string {
	if value == "" {
		return ""
	}
	leading := isHTMLASCIIWhitespace(value[0])
	trailing := isHTMLASCIIWhitespace(value[len(value)-1])
	collapsed := collapseHTMLASCIIWhitespaceKeepLines(value)
	if leading && !strings.HasPrefix(collapsed, "\n") {
		collapsed = " " + collapsed
	}
	if trailing && !strings.HasSuffix(collapsed, "\n") {
		collapsed += " "
	}
	return collapsed
}

func writeDiffFrame(builder interface{ Write([]byte) (int, error) }, value string) {
	length := strconv.AppendInt(nil, int64(len(value)), 10)
	_, _ = builder.Write(length)
	_, _ = builder.Write([]byte{':'})
	_, _ = builder.Write([]byte(value))
}

func diffPathTag(tag string) string {
	cut := len(tag)
	folded := false
	for index := 0; index < len(tag); index++ {
		char := tag[index]
		if char >= 'A' && char <= 'Z' {
			char += 'a' - 'A'
			folded = true
		}
		letter := char >= 'a' && char <= 'z'
		validSuffix := index > 0 && (char == '-' || char == ':' || char >= '0' && char <= '9')
		if !letter && !validSuffix {
			cut = index
			break
		}
	}
	if !folded {
		return tag[:cut]
	}
	// Tree-builder-adjusted foreign names must not change path casing.
	return strings.ToLower(tag[:cut])
}

func diffTokenInForeignContent(stack []diffNamespaceEntry, type_ xhtml.TokenType, tag string) bool {
	if len(stack) == 0 || stack[len(stack)-1].namespace == "" {
		return false
	}
	top := stack[len(stack)-1]
	if top.namespace == "math" && diffMathTextIntegrationTag(top.tag) {
		return type_ != xhtml.TextToken && (type_ != xhtml.StartTagToken && type_ != xhtml.SelfClosingTagToken || tag == "mglyph" || tag == "malignmark")
	}
	if top.namespace == "math" && top.tag == "annotation-xml" && (type_ == xhtml.StartTagToken || type_ == xhtml.SelfClosingTagToken) && tag == "svg" {
		return false
	}
	if top.integration && (type_ == xhtml.StartTagToken || type_ == xhtml.SelfClosingTagToken || type_ == xhtml.TextToken) {
		return false
	}
	return type_ != xhtml.ErrorToken
}

func diffMathTextIntegrationTag(tag string) bool {
	switch tag {
	case "mi", "mo", "mn", "ms", "mtext":
		return true
	default:
		return false
	}
}

func diffForeignBreakout(tag string, attrs map[string]string) bool {
	switch tag {
	case "b", "big", "blockquote", "body", "br", "center", "code", "dd", "div", "dl", "dt", "em", "embed",
		"h1", "h2", "h3", "h4", "h5", "h6", "head", "hr", "i", "img", "li", "listing", "menu", "meta",
		"nobr", "ol", "p", "pre", "ruby", "s", "small", "span", "strong", "strike", "sub", "sup", "table", "tt", "u", "ul", "var":
		return true
	case "font":
		_, color := attrs["color"]
		_, face := attrs["face"]
		_, size := attrs["size"]
		return color || face || size
	default:
		return false
	}
}

func diffHTMLIntegrationPoint(entry diffNamespaceEntry, attrs map[string]string) bool {
	if entry.namespace == "svg" {
		return entry.tag == "foreignobject" || entry.tag == "desc" || entry.tag == "title"
	}
	if entry.namespace == "math" {
		switch entry.tag {
		case "mi", "mo", "mn", "ms", "mtext":
			return true
		case "annotation-xml":
			encoding := attrs["encoding"]
			return strings.EqualFold(encoding, "text/html") || strings.EqualFold(encoding, "application/xhtml+xml")
		}
	}
	return false
}

func isDiffVoidTag(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

// isDiffRawTextTag covers the elements whose content the tokenizer reads as text
// rather than markup (RAWTEXT and RCDATA): '<' inside them is literal and only the
// matching end tag closes them. noscript is included because scripting is enabled
// in any normal browser. plaintext is NOT here: it runs to EOF rather than to a
// matching close tag, which this path cannot express.
func isDiffRawTextTag(tag string) bool {
	switch tag {
	case "script", "style", "textarea", "title",
		"iframe", "noembed", "noframes", "xmp", "noscript":
		return true
	default:
		return false
	}
}

// isDiffLiteralRawTextTag covers the raw-text elements that do NOT decode
// character references. Derived from isDiffRawTextTag so the two cannot drift:
// every raw-text element is literal except the two RCDATA ones.
func isDiffLiteralRawTextTag(tag string) bool {
	return isDiffRawTextTag(tag) && tag != "textarea" && tag != "title"
}

func isDiffWrapper(tag string) bool {
	return tag == "html" || tag == "head" || tag == "body"
}

func matchDiffNodes(before, after []htmlDiffNode) (map[int]int, error) {
	matches := map[int]int{}
	used := map[int]bool{}
	budget := diffMatchBudget{}
	matchRootNodes(before, after, matches, used)
	for changed := true; changed; {
		changed = false
		if !budget.addComparisons(len(before) + len(after)) {
			return nil, errDiffLimit
		}
		bounds := newSiblingOrderBounds(before, after, matches)
		for _, pair := range sortedDiffMatches(matches) {
			beforeParent, afterParent := pair[0], pair[1]
			if matchAIDChildren(before, after, beforeParent, afterParent, matches, used) {
				changed = true
				// New AID anchors under this parent must be visible to the exact-match
				// pass below (and to later parents) so no match crosses them.
				bounds.refreshParent(before, beforeParent, matches)
			}
			exactChanged, err := matchExactChildSequence(before, after, beforeParent, afterParent, matches, used, bounds, &budget)
			if err != nil {
				return nil, err
			}
			if exactChanged {
				changed = true
				bounds.refreshParent(before, beforeParent, matches)
			}
		}
	}
	if !budget.addComparisons(len(before) + len(after)) {
		return nil, errDiffLimit
	}
	bounds := newSiblingOrderBounds(before, after, matches)
	for beforeIndex, beforeNode := range before {
		if _, ok := matches[beforeIndex]; ok {
			continue
		}
		for afterIndex, afterNode := range after {
			if used[afterIndex] || beforeNode.path != afterNode.path || beforeNode.tag != afterNode.tag || !parentsMatch(beforeNode, afterNode, matches) {
				continue
			}
			matches[beforeIndex] = afterIndex
			used[afterIndex] = true
			bounds.refreshParent(before, beforeNode.parent, matches)
			break
		}
	}
	for beforeIndex, beforeNode := range before {
		if _, ok := matches[beforeIndex]; ok {
			continue
		}
		bestIndex, bestScore := -1, 0.0
		for afterIndex, afterNode := range after {
			if used[afterIndex] || beforeNode.tag != afterNode.tag || !parentsMatch(beforeNode, afterNode, matches) {
				continue
			}
			compatible := bounds.compatible(before, after, beforeIndex, afterIndex)
			if !compatible {
				continue
			}
			if !budget.add(len(beforeNode.compareText) + len(afterNode.compareText)) {
				return nil, errDiffLimit
			}
			score := diffNodeSimilarity(beforeNode, afterNode)
			if score > bestScore {
				bestIndex, bestScore = afterIndex, score
			}
		}
		if bestIndex >= 0 && bestScore >= 0.55 {
			matches[beforeIndex] = bestIndex
			used[bestIndex] = true
			bounds.refreshParent(before, beforeNode.parent, matches)
		}
	}
	return matches, nil
}

func sortedDiffMatches(matches map[int]int) [][2]int {
	pairs := make([][2]int, 0, len(matches))
	for beforeIndex, afterIndex := range matches {
		pairs = append(pairs, [2]int{beforeIndex, afterIndex})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] == pairs[j][0] {
			return pairs[i][1] < pairs[j][1]
		}
		return pairs[i][0] < pairs[j][0]
	})
	return pairs
}

func matchAIDChildren(before, after []htmlDiffNode, beforeParent, afterParent int, matches map[int]int, used map[int]bool) bool {
	afterByAID := map[string][]int{}
	for _, afterIndex := range after[afterParent].children {
		if !used[afterIndex] && after[afterIndex].aid != "" {
			afterByAID[after[afterIndex].aid] = append(afterByAID[after[afterIndex].aid], afterIndex)
		}
	}
	changed := false
	for _, beforeIndex := range before[beforeParent].children {
		if _, ok := matches[beforeIndex]; ok || before[beforeIndex].aid == "" {
			continue
		}
		candidates := afterByAID[before[beforeIndex].aid]
		if len(candidates) != 1 || used[candidates[0]] || before[beforeIndex].tag != after[candidates[0]].tag {
			continue
		}
		matches[beforeIndex] = candidates[0]
		used[candidates[0]] = true
		changed = true
	}
	return changed
}

type diffMatchBudget struct {
	comparisons int
	bytes       int
}

func (budget *diffMatchBudget) add(bytes int) bool {
	return budget.addComparisonsAndBytes(1, bytes)
}

func (budget *diffMatchBudget) addComparisons(comparisons int) bool {
	return budget.addComparisonsAndBytes(comparisons, 0)
}

func (budget *diffMatchBudget) addComparisonsAndBytes(comparisons, bytes int) bool {
	budget.comparisons += comparisons
	budget.bytes += bytes
	return budget.comparisons <= maxDiffComparisons && budget.bytes <= maxDiffCompareBytes
}

func matchRootNodes(before, after []htmlDiffNode, matches map[int]int, used map[int]bool) {
	for beforeIndex, beforeNode := range before {
		if beforeNode.parent >= 0 {
			continue
		}
		for afterIndex, afterNode := range after {
			if !used[afterIndex] && afterNode.parent < 0 && beforeNode.tag == afterNode.tag {
				matches[beforeIndex] = afterIndex
				used[afterIndex] = true
				break
			}
		}
	}
}

func matchExactChildSequence(before, after []htmlDiffNode, beforeParent, afterParent int, matches map[int]int, used map[int]bool, bounds siblingOrderBounds, budget *diffMatchBudget) (bool, error) {
	beforeChildren := unmatchedDiffChildren(before[beforeParent].children, matches, nil)
	afterChildren := unmatchedDiffChildren(after[afterParent].children, nil, used)
	if len(beforeChildren) == 0 || len(afterChildren) == 0 {
		return false, nil
	}
	changed := false
	beforeStart, afterStart := 0, 0
	for beforeStart < len(beforeChildren) && afterStart < len(afterChildren) {
		equal, err := diffSignaturesEqual(before[beforeChildren[beforeStart]], after[afterChildren[afterStart]], budget)
		if err != nil {
			return false, err
		}
		compatible := bounds.compatible(before, after, beforeChildren[beforeStart], afterChildren[afterStart])
		if !equal || !compatible {
			break
		}
		matches[beforeChildren[beforeStart]] = afterChildren[afterStart]
		used[afterChildren[afterStart]] = true
		changed = true
		beforeStart++
		afterStart++
	}
	beforeEnd, afterEnd := len(beforeChildren), len(afterChildren)
	for beforeEnd > beforeStart && afterEnd > afterStart {
		beforeIndex, afterIndex := beforeChildren[beforeEnd-1], afterChildren[afterEnd-1]
		equal, err := diffSignaturesEqual(before[beforeIndex], after[afterIndex], budget)
		if err != nil {
			return false, err
		}
		compatible := bounds.compatible(before, after, beforeIndex, afterIndex)
		if !equal || !compatible {
			break
		}
		beforeEnd--
		afterEnd--
		matches[beforeIndex] = afterIndex
		used[afterIndex] = true
		changed = true
	}
	beforeChildren = beforeChildren[beforeStart:beforeEnd]
	afterChildren = afterChildren[afterStart:afterEnd]
	if len(beforeChildren) == 0 || len(afterChildren) == 0 {
		return changed, nil
	}
	if len(beforeChildren) > (maxDiffComparisons-budget.comparisons)/len(afterChildren) {
		if isDiffWrapper(before[beforeParent].tag) && isDiffWrapper(after[afterParent].tag) {
			return matchUniqueChildSignatures(before, after, beforeChildren, afterChildren, matches, used), nil
		}
		return false, errDiffLimit
	}
	width := len(afterChildren) + 1
	cells := make([]uint16, (len(beforeChildren)+1)*width)
	for beforePos := len(beforeChildren) - 1; beforePos >= 0; beforePos-- {
		for afterPos := len(afterChildren) - 1; afterPos >= 0; afterPos-- {
			beforeIndex, afterIndex := beforeChildren[beforePos], afterChildren[afterPos]
			equal, err := diffSignaturesEqual(before[beforeIndex], after[afterIndex], budget)
			if err != nil {
				return false, err
			}
			cell := beforePos*width + afterPos
			compatible := bounds.compatible(before, after, beforeIndex, afterIndex)
			if equal && compatible {
				cells[cell] = cells[(beforePos+1)*width+afterPos+1] + 1
			} else {
				skipBefore := cells[(beforePos+1)*width+afterPos]
				skipAfter := cells[beforePos*width+afterPos+1]
				if skipBefore >= skipAfter {
					cells[cell] = skipBefore
				} else {
					cells[cell] = skipAfter
				}
			}
		}
	}
	for beforePos, afterPos := 0, 0; beforePos < len(beforeChildren) && afterPos < len(afterChildren); {
		beforeIndex, afterIndex := beforeChildren[beforePos], afterChildren[afterPos]
		compatible := bounds.compatible(before, after, beforeIndex, afterIndex)
		if diffNodeSignature(before[beforeIndex]) == diffNodeSignature(after[afterIndex]) && compatible && cells[beforePos*width+afterPos] == cells[(beforePos+1)*width+afterPos+1]+1 {
			matches[beforeIndex] = afterIndex
			used[afterIndex] = true
			changed = true
			beforePos++
			afterPos++
		} else if cells[(beforePos+1)*width+afterPos] >= cells[beforePos*width+afterPos+1] {
			beforePos++
		} else {
			afterPos++
		}
	}
	return changed, nil
}

// matchUniqueChildSignatures avoids quadratic LCS work created only by parser
// wrappers while leaving ambiguous repetitions to the bounded matcher.
func matchUniqueChildSignatures(before, after []htmlDiffNode, beforeChildren, afterChildren []int, matches map[int]int, used map[int]bool) bool {
	type signatureEntry struct {
		index int
		count int
	}
	afterBySignature := make(map[string]signatureEntry, len(afterChildren))
	for _, index := range afterChildren {
		signature := diffNodeSignature(after[index])
		entry := afterBySignature[signature]
		entry.index = index
		entry.count++
		afterBySignature[signature] = entry
	}
	beforeCounts := make(map[string]int, len(beforeChildren))
	for _, index := range beforeChildren {
		beforeCounts[diffNodeSignature(before[index])]++
	}
	changed := false
	for _, beforeIndex := range beforeChildren {
		signature := diffNodeSignature(before[beforeIndex])
		entry := afterBySignature[signature]
		if beforeCounts[signature] != 1 || entry.count != 1 || used[entry.index] {
			continue
		}
		matches[beforeIndex] = entry.index
		used[entry.index] = true
		changed = true
	}
	return changed
}

func diffSignaturesEqual(before, after htmlDiffNode, budget *diffMatchBudget) (bool, error) {
	beforeSignature, afterSignature := diffNodeSignature(before), diffNodeSignature(after)
	if !budget.add(0) {
		return false, errDiffLimit
	}
	return beforeSignature == afterSignature, nil
}

func unmatchedDiffChildren(children []int, matches map[int]int, used map[int]bool) []int {
	result := make([]int, 0, len(children))
	for _, index := range children {
		if matches != nil {
			if _, ok := matches[index]; ok {
				continue
			}
		}
		if used != nil && used[index] {
			continue
		}
		result = append(result, index)
	}
	return result
}

func parentsMatch(before, after htmlDiffNode, matches map[int]int) bool {
	if before.parent < 0 || after.parent < 0 {
		return before.parent == after.parent
	}
	matchedParent, ok := matches[before.parent]
	return ok && matchedParent == after.parent
}

type siblingOrderBounds struct {
	lower []int
	upper []int
}

func newSiblingOrderBounds(before, after []htmlDiffNode, matches map[int]int) siblingOrderBounds {
	bounds := siblingOrderBounds{lower: make([]int, len(before)), upper: make([]int, len(before))}
	for index := range bounds.lower {
		bounds.lower[index] = -1
		bounds.upper[index] = len(after)
	}
	for parentIndex := range before {
		bounds.refreshParent(before, parentIndex, matches)
	}
	return bounds
}

// refreshParent recomputes the sibling-order anchor bounds for one parent's
// children from the current matches. It is O(children) and touches only that
// parent, so it can be called after new matches are committed under a parent
// without the O(nodes×parents) cost of rebuilding the whole document. Keeping the
// bounds current is required for correctness: a stale snapshot could let a later
// match cross an anchor established earlier in the same fixpoint pass.
func (bounds siblingOrderBounds) refreshParent(before []htmlDiffNode, parentIndex int, matches map[int]int) {
	if parentIndex < 0 || parentIndex >= len(before) {
		return
	}
	children := before[parentIndex].children
	anchor := -1
	for _, child := range children {
		bounds.lower[child] = anchor
		if matched, ok := matches[child]; ok {
			anchor = matched
		}
	}
	anchor = -1
	for pos := len(children) - 1; pos >= 0; pos-- {
		child := children[pos]
		bounds.upper[child] = anchor
		if matched, ok := matches[child]; ok {
			anchor = matched
		}
	}
}

func (bounds siblingOrderBounds) compatible(before, after []htmlDiffNode, beforeIndex, afterIndex int) bool {
	beforeNode, afterNode := before[beforeIndex], after[afterIndex]
	if beforeNode.parent < 0 || afterNode.parent < 0 {
		return true
	}
	if anchor := bounds.lower[beforeIndex]; anchor >= 0 && after[anchor].parent == afterNode.parent && after[anchor].siblingPos >= afterNode.siblingPos {
		return false
	}
	if anchor := bounds.upper[beforeIndex]; anchor >= 0 && after[anchor].parent == afterNode.parent && after[anchor].siblingPos <= afterNode.siblingPos {
		return false
	}
	return true
}

func diffNodeSimilarity(before, after htmlDiffNode) float64 {
	score := 0.25
	if before.parent >= 0 && after.parent >= 0 {
		beforeParent := before.path[:strings.LastIndex(before.path, "/")]
		afterParent := after.path[:strings.LastIndex(after.path, "/")]
		if beforeParent == afterParent {
			score += 0.25
		}
	}
	if before.order == after.order {
		score += 0.1
	}
	score += 0.25 * stringSimilarity(before.compareText, after.compareText)
	score += 0.15 * attrSimilarity(before.attrs, after.attrs)
	return score
}

func stringSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	if a == "" || b == "" {
		return 0
	}
	shorter, longer := a, b
	if len(shorter) > len(longer) {
		shorter, longer = longer, shorter
	}
	if strings.Contains(longer, shorter) {
		return float64(len(shorter)) / float64(len(longer))
	}
	common := 0
	seen := map[string]bool{}
	for _, word := range strings.Fields(shorter) {
		seen[word] = true
	}
	for _, word := range strings.Fields(longer) {
		if seen[word] {
			common++
		}
	}
	return float64(2*common) / float64(len(strings.Fields(a))+len(strings.Fields(b)))
}

func attrSimilarity(a, b map[string]string) float64 {
	union, same := 0, 0
	seen := map[string]bool{}
	for key, value := range a {
		if key == "data-odoc-aid" {
			continue
		}
		seen[key] = true
		union++
		if b[key] == value {
			same++
		}
	}
	for key := range b {
		if key != "data-odoc-aid" && !seen[key] {
			union++
		}
	}
	if union == 0 {
		return 1
	}
	return float64(same) / float64(union)
}

func diffNodeSignature(node htmlDiffNode) string {
	if node.signature != "" {
		return node.signature
	}
	return computeDiffNodeSignature(node)
}

func computeDiffNodeSignature(node htmlDiffNode) string {
	keys := make([]string, 0, len(node.attrs))
	for key := range node.attrs {
		if key != "data-odoc-aid" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var builder strings.Builder
	writeDiffFrame(&builder, node.tag)
	for _, key := range keys {
		writeDiffFrame(&builder, key)
		writeDiffFrame(&builder, node.attrs[key])
	}
	writeDiffFrame(&builder, node.compareText)
	writeDiffFrame(&builder, node.textDigest)
	return builder.String()
}

func normalizeCompareText(value string) string {
	if len(value) > maxDiffCompareText {
		value = truncateUTF8(value, maxDiffCompareText)
	}
	return strings.ToLower(collapseHTMLASCIIWhitespace(value))
}

// diffNodeSnippet renders a bounded normalized DOM subtree.
func diffNodeSnippet(node htmlDiffNode) string {
	if node.element == nil {
		return ""
	}
	writer := boundedDiffWriter{limit: maxDiffSnippetBytes}
	if err := xhtml.Render(&writer, node.element); err == nil {
		return string(writer.buf)
	}
	opening := diffNodeOpeningTag(node)
	if isDiffVoidTag(node.tag) {
		return opening
	}
	return opening + "<!-- omitted -->" + "</" + node.tag + ">"
}

// diffNodeOpeningTag delegates attribute escaping to the renderer.
func diffNodeOpeningTag(node htmlDiffNode) string {
	shallow := xhtml.Node{Type: xhtml.ElementNode, DataAtom: node.element.DataAtom, Data: node.element.Data, Namespace: node.element.Namespace, Attr: node.element.Attr}
	writer := boundedDiffWriter{limit: maxDiffOpeningBytes}
	if err := xhtml.Render(&writer, &shallow); err != nil {
		return "<" + node.tag + ">"
	}
	return strings.TrimSuffix(string(writer.buf), "</"+node.element.Data+">")
}

// boundedDiffWriter aborts rendering before a subtree exceeds its output limit.
type boundedDiffWriter struct {
	buf   []byte
	limit int
}

func (writer *boundedDiffWriter) Write(bytes []byte) (int, error) {
	if len(writer.buf)+len(bytes) > writer.limit {
		return 0, errDiffLimit
	}
	writer.buf = append(writer.buf, bytes...)
	return len(bytes), nil
}

func (writer *boundedDiffWriter) WriteString(value string) (int, error) {
	if len(writer.buf)+len(value) > writer.limit {
		return 0, errDiffLimit
	}
	writer.buf = append(writer.buf, value...)
	return len(value), nil
}

func (writer *boundedDiffWriter) WriteByte(value byte) error {
	if len(writer.buf) >= writer.limit {
		return errDiffLimit
	}
	writer.buf = append(writer.buf, value)
	return nil
}

func normalizeDiffHTML(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	var quote byte
	pendingSpace := false
	for index := 0; index < len(value); index++ {
		byteValue := value[index]
		if quote != 0 {
			builder.WriteByte(byteValue)
			if byteValue == quote {
				quote = 0
			}
			continue
		}
		if byteValue == '\'' || byteValue == '"' {
			if pendingSpace && builder.Len() > 0 {
				builder.WriteByte(' ')
			}
			pendingSpace = false
			quote = byteValue
			builder.WriteByte(byteValue)
			continue
		}
		if isHTMLASCIIWhitespace(byteValue) {
			pendingSpace = builder.Len() > 0
			continue
		}
		if pendingSpace {
			builder.WriteByte(' ')
			pendingSpace = false
		}
		builder.WriteByte(byteValue)
	}
	return builder.String()
}

func diffCodeHunks(before, after string) ([]CodeHunk, error) {
	oldLines, err := scanDiffLines(before)
	if err != nil {
		return nil, errDiffLimit
	}
	newLines, err := scanDiffLines(after)
	if err != nil {
		return nil, errDiffLimit
	}
	return diffCodeHunksLines(oldLines, newLines)
}

func diffCodeHunksLines(oldLines, newLines []diffSourceLine) ([]CodeHunk, error) {
	ops, ok := diffLineOps(oldLines, newLines)
	if !ok {
		return nil, errDiffLimit
	}
	changeIndexes := make([]int, 0)
	for index, op := range ops {
		if op.kind != ' ' {
			changeIndexes = append(changeIndexes, index)
		}
	}
	if len(changeIndexes) == 0 {
		return []CodeHunk{}, nil
	}
	type hunkRange struct{ start, end int }
	ranges := make([]hunkRange, 0, len(changeIndexes))
	for _, index := range changeIndexes {
		start, end := index-diffContextLines, index+diffContextLines+1
		if start < 0 {
			start = 0
		}
		if end > len(ops) {
			end = len(ops)
		}
		if len(ranges) > 0 && start <= ranges[len(ranges)-1].end {
			ranges[len(ranges)-1].end = end
		} else {
			ranges = append(ranges, hunkRange{start: start, end: end})
		}
	}
	hunks := make([]CodeHunk, 0, len(ranges))
	totalLines := 0
	for _, interval := range ranges {
		totalLines += interval.end - interval.start
	}
	if totalLines > maxDiffHunkLines {
		return nil, errDiffLimit
	}
	for _, interval := range ranges {
		oldStart, newStart := 1, 1
		if interval.start > 0 {
			oldStart = ops[interval.start].oldLine
			newStart = ops[interval.start].newLine
			if oldStart == 0 {
				oldStart = ops[interval.start-1].oldLine + 1
			}
			if newStart == 0 {
				newStart = ops[interval.start-1].newLine + 1
			}
		}
		hunk := CodeHunk{OldStart: oldStart, NewStart: newStart}
		for _, op := range ops[interval.start:interval.end] {
			hunk.Lines = append(hunk.Lines, string(op.kind)+op.line.display)
			if op.kind != '+' {
				hunk.OldLines++
			}
			if op.kind != '-' {
				hunk.NewLines++
			}
		}
		hunks = append(hunks, hunk)
	}
	return hunks, nil
}

type diffLineOp struct {
	kind             byte
	line             diffSourceLine
	oldLine, newLine int
}

type diffSourceLine struct {
	identity string
	display  string
}

// newDiffSourceLine builds a source line whose diff identity is a small
// fixed-width key — the kind tag plus the canonical byte length plus the
// canonical SHA-256 — never the canonical text itself, so line-diff equality and
// LCS memory stay O(lines) with a low constant regardless of line length while
// distinct canonical content never collides. display is the human-readable
// text, separately bounded by displayDiffLine.
func newDiffSourceLine(kind, canonical, display string) diffSourceLine {
	digest := sha256.Sum256([]byte(canonical))
	return diffSourceLine{
		identity: kind + ":" + strconv.Itoa(len(canonical)) + ":" + fmt.Sprintf("%x", digest),
		display:  displayDiffLine(display),
	}
}

func diffLineOps(oldLines, newLines []diffSourceLine) ([]diffLineOp, bool) {
	if len(oldLines) > maxDiffInputLines || len(newLines) > maxDiffInputLines {
		return nil, false
	}
	oldText := diffIdentityText(oldLines)
	newText := diffIdentityText(newLines)
	dmp := diffmatchpatch.New()
	// Wall-clock deadlines can turn scheduler load into different successful diffs.
	dmp.DiffTimeout = 0
	oldTokens, newTokens, tokenLines := dmp.DiffLinesToRunes(oldText, newText)
	diffs := dmp.DiffMainRunes(oldTokens, newTokens, false)

	ops := make([]diffLineOp, 0, len(oldLines)+len(newLines))
	oldIndex, newIndex := 0, 0
	for _, diff := range diffs {
		for _, token := range diff.Text {
			if int(token) >= len(tokenLines) {
				return nil, false
			}
			identity := strings.TrimSuffix(tokenLines[token], "\n")
			switch diff.Type {
			case diffmatchpatch.DiffEqual:
				if oldIndex >= len(oldLines) || newIndex >= len(newLines) || oldLines[oldIndex].identity != identity || newLines[newIndex].identity != identity {
					return nil, false
				}
				ops = append(ops, diffLineOp{kind: ' ', line: oldLines[oldIndex], oldLine: oldIndex + 1, newLine: newIndex + 1})
				oldIndex++
				newIndex++
			case diffmatchpatch.DiffDelete:
				if oldIndex >= len(oldLines) || oldLines[oldIndex].identity != identity {
					return nil, false
				}
				ops = append(ops, diffLineOp{kind: '-', line: oldLines[oldIndex], oldLine: oldIndex + 1, newLine: newIndex + 1})
				oldIndex++
			case diffmatchpatch.DiffInsert:
				if newIndex >= len(newLines) || newLines[newIndex].identity != identity {
					return nil, false
				}
				ops = append(ops, diffLineOp{kind: '+', line: newLines[newIndex], oldLine: oldIndex + 1, newLine: newIndex + 1})
				newIndex++
			default:
				return nil, false
			}
		}
	}
	return ops, oldIndex == len(oldLines) && newIndex == len(newLines)
}

func diffIdentityText(lines []diffSourceLine) string {
	var text strings.Builder
	for _, line := range lines {
		text.WriteString(line.identity)
		text.WriteByte('\n')
	}
	return text.String()
}

// wsFingerprint is a streaming, fixed-memory digest of a document's inter-tag
// whitespace layout. It records only *whitespace events* — inter-tag gaps that
// carry actual whitespace bytes (a whitespace-only run, or a content run with
// leading/trailing whitespace). Gaps with no whitespace at all (two adjacent
// boundaries, or a pure-content run) are NOT recorded and contribute no hash
// bytes and no event index. Each real event is located by the number of element
// boundaries since the previous event, so relocating identical bytes changes
// this single digest without adding per-event source lines.
//
// The digest is exactly the sequence of whitespace events in document order.
// Each event contributes its event index, boundary distance, exact byte length,
// and exact signature bytes. Boundary tags and content are deliberately absent.
// A structural element insertion can shift a distance and therefore cause the
// one bounded formatting marker to change; this accepted false positive is the
// cost of detecting same-byte relocation at any event count.
//
// It is built unconditionally for every document (never gated on a CSS
// heuristic), so no external/dynamic style, preload, or script can double-blind
// a whitespace-layout change. It buffers only the current inter-boundary gap and
// a small running event counter, so memory stays O(1) in run bytes beyond the
// bounded whitespace edges each signature keeps.
//
// Framing sequence, made explicit so the contract is testable:
//   - A fresh framer starts at the document's leading edge.
//   - text(run) accumulates the run into the currently pending inter-boundary
//     gap. Consecutive text runs (rare; the scanner coalesces) concatenate.
//   - boundary() closes the pending gap at an ELEMENT boundary: only if the
//     pending gap carried actual whitespace does it fold one event into the
//     digest (its index, boundary distance, and exact whitespace bytes); it then
//     clears the pending gap.
//   - transparent() marks a comment/PI/doctype/raw-text emit: it is NOT an
//     element boundary, so it neither closes the pending gap nor advances the
//     event counter. Whitespace on either side of it stays in the pending gap
//     and coalesces into the surrounding event, so a real whitespace run next to
//     a comment is still one event while a whitespace-free comment/raw insert is
//     fully invisible to the fingerprint.
//   - sum() closes the final trailing edge (an element boundary) so a trailing
//     whitespace gap before EOF is recorded, then returns the digest.
type wsFingerprint struct {
	hasher  hash.Hash
	frame   [8]byte
	pending strings.Builder
	// events is each whitespace event's document-order index. The boundary count
	// locates that event without emitting a separate diff line per event.
	events               uint64
	boundariesSinceEvent uint64
}

func newWSFingerprint() *wsFingerprint {
	fp := &wsFingerprint{hasher: sha256.New()}
	// Domain-separate the stream so it can never coincide with any other digest.
	// v4: ordered whitespace events including their boundary distance. v3 stored
	// only event bytes, so relocating identical events could collide. v2 anchor-keyed each gap by a
	// surrounding element-boundary tag pair + occurrence index, which structural
	// inserts on the counted side shifted; v1 framed every boundary by absolute
	// slot).
	fp.hasher.Write([]byte("odoc-ws-fingerprint-v4\x00"))
	return fp
}

// text accumulates an inter-tag text run into the currently pending gap. The run
// is kept verbatim until the next boundary reduces it to its
// whitespaceLayoutSignature, so the digest reacts to inter-tag whitespace while
// staying independent of the content bytes (which already surface in the line
// diff).
func (fp *wsFingerprint) text(run string) {
	if run != "" {
		fp.pending.WriteString(run)
	}
}

// boundary closes the pending inter-boundary gap at an ELEMENT boundary. Only a
// gap carrying actual whitespace folds one event into the digest — its
// document-order event index, the exact byte length of its whitespace signature,
// and those exact bytes — after which the pending gap is cleared. The digest
// keeps no anchor, so a structural insert/removal anywhere adds no event and
// leaves the digest identical.
func (fp *wsFingerprint) boundary() {
	run := fp.pending.String()
	fp.pending.Reset()
	sig := whitespaceLayoutSignature(run)
	if !signatureHasWhitespace(sig) {
		fp.boundariesSinceEvent++
		return
	}
	binary.BigEndian.PutUint64(fp.frame[0:8], fp.events)
	_, _ = fp.hasher.Write(fp.frame[:8])
	binary.BigEndian.PutUint64(fp.frame[0:8], fp.boundariesSinceEvent)
	_, _ = fp.hasher.Write(fp.frame[:8])
	fp.events++
	fp.boundariesSinceEvent = 1
	fp.writeField(sig)
}

// transparent marks a comment, processing instruction, doctype, or raw-text
// element emit. Such a node is NOT an element boundary for whitespace layout: it
// does not close the pending gap or advance the event counter. Whitespace
// sitting on either side of it therefore stays in the pending gap and coalesces
// into the surrounding whitespace event, so a genuine whitespace run next to a
// comment is still detected while a whitespace-free comment/raw insert or removal
// leaves the fingerprint identical. Raw-text CONTENT must not be fed into the
// pending gap (it is content, not inter-tag whitespace); callers simply skip
// text() for it and call transparent() for the element as a whole.
func (fp *wsFingerprint) transparent() {}

// writeField hashes one length-prefixed field so adjacent fields can never be
// confused by concatenation (e.g. "a"+"bc" vs "ab"+"c").
func (fp *wsFingerprint) writeField(s string) {
	binary.BigEndian.PutUint64(fp.frame[0:8], uint64(len(s)))
	_, _ = fp.hasher.Write(fp.frame[:8])
	if len(s) > 0 {
		// hash.Hash.Write is documented never to return an error.
		_, _ = io.WriteString(fp.hasher, s)
	}
}

// sum closes the final trailing-edge element boundary (so a trailing whitespace
// gap before EOF is recorded as an event) and returns the hex digest of the
// accumulated ordered whitespace-event sequence.
func (fp *wsFingerprint) sum() string {
	fp.boundary()
	return fmt.Sprintf("%x", fp.hasher.Sum(nil))
}

// whitespaceLayoutSignature reduces an inter-tag text run to just its
// whitespace-layout contribution: the exact leading and trailing whitespace,
// with a one-byte sentinel marking whether any non-whitespace content sat
// between them. This lets the document fingerprint react to inserted/removed
// inter-tag whitespace (and to surrounding whitespace on a content run) while
// staying independent of the content bytes themselves, which already surface in
// the regular line diff. It never allocates more than the two whitespace edges.
func whitespaceLayoutSignature(run string) string {
	lead := run[:len(run)-len(strings.TrimLeft(run, " \t\r\n\f"))]
	if len(lead) == len(run) {
		// Whitespace-only run: no content, no separate trailing edge.
		return lead
	}
	trail := run[len(strings.TrimRight(run, " \t\r\n\f")):]
	return lead + "\x01\x02\x01" + trail
}

// signatureHasWhitespace reports whether a whitespaceLayoutSignature carries any
// actual whitespace — i.e. this run is a whitespace *event* the fingerprint must
// record. A pure-content run signature is exactly the sentinel with no bytes on
// either edge ("\x01\x02\x01"); the empty string is a truly empty run. Both
// carry no whitespace, so the fingerprint skips them and a structural-only edit
// never shifts an occurrence index or perturbs the digest.
func signatureHasWhitespace(sig string) bool {
	return sig != "" && sig != "\x01\x02\x01"
}

// newWhitespaceFingerprintLine builds the single document-level whitespace
// record. The identity uses a dedicated "ws-doc" kind (distinct from the real
// "tag"/"pre"/"ws"/"text" kinds) so no literal user content can forge or collide
// with it; equal whitespace layout compares equal and any change differs. The
// display is a bounded, user-readable marker with no digest or internal token,
// so a changed fingerprint surfaces as exactly one comprehensible hunk line.
func newWhitespaceFingerprintLine(digest string) diffSourceLine {
	return diffSourceLine{
		identity: "ws-doc:" + digest,
		display:  "[formatting whitespace changed]",
	}
}

func normalizedHTMLLines(source string) ([]diffSourceLine, bool) {
	lines, err := scanDiffLines(source)
	return lines, err == nil
}

type diffLineConsumer struct {
	lines       []diffSourceLine
	preStack    []preContext
	layoutCSS   bool
	fingerprint *wsFingerprint
	prevInline  bool
	pendingText *diffHTMLToken
}

func newDiffLineConsumer(source string) *diffLineConsumer {
	return &diffLineConsumer{
		lines:       make([]diffSourceLine, 0, 256),
		preStack:    make([]preContext, 0, 16),
		layoutCSS:   hasLayoutAffectingCSS(source),
		fingerprint: newWSFingerprint(),
	}
}

func (c *diffLineConsumer) consume(token diffHTMLToken) bool {
	if c.pendingText != nil {
		if !c.consumeText(*c.pendingText, &token) {
			return false
		}
		c.pendingText = nil
	}
	if token.type_ == xhtml.TextToken && token.rawTextTag == "" {
		c.pendingText = &token
		return true
	}
	return c.consumeNonText(token)
}

func (c *diffLineConsumer) consumeText(token diffHTMLToken, next *diffHTMLToken) bool {
	if len(c.lines) >= maxDiffInputLines {
		return false
	}
	c.fingerprint.text(token.raw)
	nextInline := false
	if next != nil {
		if next.type_ == xhtml.StartTagToken || next.type_ == xhtml.SelfClosingTagToken || next.type_ == xhtml.EndTagToken {
			nextInline = boundaryInlineAttrs(next.tag, next.attrs)
		} else {
			nextInline = true
		}
	}
	visible := whitespaceRunVisible(token.raw, c.prevInline, nextInline, c.layoutCSS)
	return appendNormalizedDiffText(&c.lines, token.raw, false, visible, c.preformatted())
}

func (c *diffLineConsumer) consumeNonText(token diffHTMLToken) bool {
	if len(c.lines) >= maxDiffInputLines {
		return false
	}
	switch token.type_ {
	case xhtml.TextToken: // RCDATA/RAWTEXT content.
		literal := isDiffLiteralRawTextTag(token.rawTextTag)
		if !appendNormalizedDiffText(&c.lines, token.raw, literal, false, c.preformatted()) {
			return false
		}
		c.fingerprint.transparent()
	case xhtml.CommentToken, xhtml.DoctypeToken:
		canonical := normalizeDiffHTML(token.raw)
		c.lines = append(c.lines, newDiffSourceLine("tag", canonical, canonical))
		c.fingerprint.transparent()
	case xhtml.EndTagToken:
		canonical := normalizeDiffHTML(token.raw)
		c.lines = append(c.lines, newDiffSourceLine("tag", canonical, canonical))
		if token.namespace == "" && isDiffRawTextTag(token.tag) {
			c.fingerprint.transparent()
		} else {
			c.fingerprint.boundary()
		}
		popPreformatted(&c.preStack, token.tag)
		c.prevInline = boundaryInlineAttrs(token.tag, nil)
	case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
		canonical := normalizeDiffHTML(token.raw)
		c.lines = append(c.lines, newDiffSourceLine("tag", canonical, canonical))
		c.prevInline = boundaryInlineAttrs(token.tag, token.attrs)
		rawText := token.namespace == "" && isDiffRawTextTag(token.tag)
		if rawText {
			c.fingerprint.transparent()
		} else {
			c.fingerprint.boundary()
		}
		// HTML ignores a self-closing flag on ordinary/raw-text HTML elements.
		if !isDiffVoidTag(token.tag) && (token.type_ != xhtml.SelfClosingTagToken || token.namespace == "") {
			if preserve, establishes := isPreformattedContextAttrs(token.tag, token.attrs); establishes {
				c.preStack = append(c.preStack, preContext{tag: token.tag, preserve: preserve})
			}
		}
	}
	return len(c.lines) <= maxDiffInputLines
}

func (c *diffLineConsumer) preformatted() bool {
	return len(c.preStack) > 0 && c.preStack[len(c.preStack)-1].preserve
}

func (c *diffLineConsumer) finish() ([]diffSourceLine, bool) {
	if c.pendingText != nil && !c.consumeText(*c.pendingText, nil) {
		return nil, false
	}
	digest := c.fingerprint.sum()
	if len(c.lines)+1 > maxDiffInputLines {
		return nil, false
	}
	c.lines = append(c.lines, newWhitespaceFingerprintLine(digest))
	return c.lines, true
}

func boundaryInlineAttrs(tag string, attrs map[string]string) bool {
	if !isBlockLevelDiffTag(tag) {
		return true
	}
	return styleHasInlineDisplay(attrs["style"])
}

// isPreformattedContextAttrs reports whether an element establishes an explicit
// white-space context and whether that context preserves whitespace. attrs
// contains tokenizer-decoded attributes; nil means no inline style is present.
func isPreformattedContextAttrs(tag string, attrs map[string]string) (preserve, establishes bool) {
	if mode, specified := inlineWhiteSpaceMode(attrs["style"]); specified {
		return mode != whiteSpaceCollapse, true
	}
	if tag == "pre" || tag == "textarea" {
		return true, true
	}
	return false, false
}

func appendNormalizedDiffText(lines *[]diffSourceLine, text string, literal, visible, preformatted bool) bool {
	raw := text
	if !literal {
		text = html.UnescapeString(text)
	}
	if preformatted {
		// Inside a preformatted / white-space:pre* context every space, tab, and
		// newline is significant, so preserve the run verbatim (bounded) instead of
		// collapsing; an empty run still emits nothing.
		if text == "" {
			return true
		}
		if len(*lines) >= maxDiffInputLines {
			return false
		}
		*lines = append(*lines, newDiffSourceLine("pre", text, text))
		return true
	}
	collapsed := collapseHTMLASCIIWhitespace(text)
	if collapsed == "" {
		// Pure inter-tag whitespace. It is preserved only when it could render as a
		// visible space (an inline neighbour, or a newline-free run under global
		// layout CSS); otherwise it is reindent/pretty-print noise and is dropped so
		// ordinary block reindent stays a zero-noise no-op.
		if text == "" || !visible {
			return true
		}
		if len(*lines) >= maxDiffInputLines {
			return false
		}
		*lines = append(*lines, newDiffSourceLine("ws", raw, visibleWhitespace(raw)))
		return true
	}
	if len(*lines) >= maxDiffInputLines {
		return false
	}
	*lines = append(*lines, newDiffSourceLine("text", collapsed, collapsed))
	return true
}

// preContext is one open element's whitespace context for the source-line layer.
// Elements that explicitly set white-space are stacked even when they collapse,
// so a descendant can override an ancestor <pre>.
type preContext struct {
	tag      string
	preserve bool
}

// whiteSpaceMode is the effective CSS white-space behaviour for a node's text.
type whiteSpaceMode int

const (
	whiteSpaceCollapse whiteSpaceMode = iota
	whiteSpacePreserveLines
	whiteSpacePreserve
)

// inlineWhiteSpaceMode resolves an inline style's white-space declaration.
// specified is false when no valid declaration exists — including the CSS-wide
// keywords, which must fall back to the tag default (revert on <pre> is pre).
func inlineWhiteSpaceMode(style string) (mode whiteSpaceMode, specified bool) {
	style = stripCSSComments(style)
	var normalMode, importantMode whiteSpaceMode
	var hasNormal, hasImportant bool
	for _, declaration := range strings.Split(style, ";") {
		property, value, ok := strings.Cut(declaration, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(property), "white-space") {
			continue
		}
		value = strings.ToLower(strings.TrimSpace(value))
		important := false
		// CSS allows whitespace (and comments, stripped above) between ! and important.
		if marker := strings.LastIndexByte(value, '!'); marker >= 0 && strings.TrimSpace(value[marker+1:]) == "important" {
			important = true
			value = strings.TrimSpace(value[:marker])
		}
		parsed, valid := whiteSpaceKeyword(value)
		if !valid {
			// Invalid declarations are dropped at parse time; an earlier valid one wins.
			continue
		}
		if important {
			importantMode, hasImportant = parsed, true
		} else {
			normalMode, hasNormal = parsed, true
		}
	}
	if hasImportant {
		return importantMode, true
	}
	if hasNormal {
		return normalMode, true
	}
	return whiteSpaceCollapse, false
}

func whiteSpaceKeyword(value string) (whiteSpaceMode, bool) {
	switch value {
	case "pre", "pre-wrap", "break-spaces":
		return whiteSpacePreserve, true
	case "pre-line":
		return whiteSpacePreserveLines, true
	case "normal", "nowrap":
		return whiteSpaceCollapse, true
	default:
		return whiteSpaceCollapse, false
	}
}

func stripCSSComments(style string) string {
	if !strings.Contains(style, "/*") {
		return style
	}
	var builder strings.Builder
	builder.Grow(len(style))
	for {
		open := strings.Index(style, "/*")
		if open < 0 {
			builder.WriteString(style)
			return builder.String()
		}
		builder.WriteString(style[:open])
		term := strings.Index(style[open+2:], "*/")
		if term < 0 {
			return builder.String()
		}
		style = style[open+2+term+2:]
	}
}

func popPreformatted(preStack *[]preContext, closeTag string) {
	for pos := len(*preStack) - 1; pos >= 0; pos-- {
		if (*preStack)[pos].tag == closeTag {
			*preStack = (*preStack)[:pos]
			return
		}
	}
}

func isOnlyHTMLASCIIWhitespace(value string) bool {
	for index := 0; index < len(value); index++ {
		if !isHTMLASCIIWhitespace(value[index]) {
			return false
		}
	}
	return true
}

func displayDiffLine(line string) string {
	const maxLine = 1024
	if len(line) <= maxLine {
		return line
	}
	return truncateUTF8(line, maxLine) + "…"
}

func visibleWhitespace(run string) string {
	const preview = 64
	value := run
	if len(value) > preview {
		value = value[:preview]
	}
	value = strings.NewReplacer(" ", "·", "\t", "→", "\r", "\\r", "\n", "\\n").Replace(value)
	if len(run) > preview {
		value += "…"
	}
	return value
}

func diffOutputSize(result *VersionDiff) int {
	encoded, err := json.Marshal(result)
	if err != nil {
		return maxDiffOutputBytes + 1
	}
	return len(encoded)
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && value[limit]&0xc0 == 0x80 {
		limit--
	}
	return value[:limit]
}

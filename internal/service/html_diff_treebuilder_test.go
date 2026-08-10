package service

import (
	"strconv"
	"strings"
	"testing"
)

// assertOracleAgreement pins the structural layer to the reference tree builder:
// equal parser trees must produce no structural change, different parser trees
// must produce at least one.
func assertOracleAgreement(t *testing.T, before, after string, wantEqual bool) *VersionDiff {
	t.Helper()
	beforeTree, afterTree := parserElementTree(t, before), parserElementTree(t, after)
	if equal := beforeTree == afterTree; equal != wantEqual {
		t.Fatalf("parser oracle trees equal = %v, want %v\nbefore:\n%s\nafter:\n%s", equal, wantEqual, beforeTree, afterTree)
	}
	result, err := buildVersionDiff(1, 2, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if wantEqual && len(result.Changes) != 0 {
		t.Fatalf("parser-equal documents produced %d structural changes: %+v", len(result.Changes), result.Changes)
	}
	if !wantEqual && len(result.Changes) == 0 {
		t.Fatalf("parser-different documents produced no structural change: summary=%+v\nbefore:\n%s\nafter:\n%s", result.Summary, beforeTree, afterTree)
	}
	return result
}

// TestDiffIntegrationPointPairsMatchParserOracle covers the reviewer's SVG and
// MathML HTML-integration-point pairs. The stray end tag is ignored by the
// reference tree builder, so an HTML subtree below foreignObject/annotation-xml
// must not be relocated out of its foreign ancestor.
func TestDiffIntegrationPointPairsMatchParserOracle(t *testing.T) {
	tests := []struct {
		name, before, after string
		wantEqual           bool
	}{
		{
			name:      "svg_foreign_object_stray_end_tag",
			before:    `<div><svg><foreignObject><div></svg><b>x</b></div>`,
			after:     `<div><svg><foreignObject><div><b>x</b></div>`,
			wantEqual: true,
		},
		{
			name:      "math_annotation_xml_stray_end_tag",
			before:    `<div><math><annotation-xml encoding="text/html"><div></math><b>x</b></div>`,
			after:     `<div><math><annotation-xml encoding="text/html"><div><b>x</b></div>`,
			wantEqual: true,
		},
		{
			name:   "svg_desc_cdata_text_edit",
			before: `<svg><desc><![CDATA[alpha]]></desc></svg>`,
			after:  `<svg><desc><![CDATA[omega]]></desc></svg>`,
		},
		{
			name:   "math_mtext_cdata_text_edit",
			before: `<math><mtext><![CDATA[alpha]]></mtext></math>`,
			after:  `<math><mtext><![CDATA[omega]]></mtext></math>`,
		},
		{
			name:   "svg_foreign_object_html_text_edit",
			before: `<svg><foreignObject><p>alpha</p></foreignObject></svg>`,
			after:  `<svg><foreignObject><p>omega</p></foreignObject></svg>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertOracleAgreement(t, test.before, test.after, test.wantEqual)
		})
	}
}

// TestDiffStrayEndTagRecoveryMatchesParserOracle covers ordinary HTML content:
// a stray </p> synthesises an empty p element and a stray </br> is treated as
// <br>, so the recovered element must appear in the structural diff and must not
// differ from its explicitly written spelling.
func TestDiffStrayEndTagRecoveryMatchesParserOracle(t *testing.T) {
	tests := []struct {
		name, before, after string
		wantEqual           bool
		wantAddedSuffix     string
	}{
		{
			name:            "stray_close_p_adds_element",
			before:          `<div></div>`,
			after:           `<div></p></div>`,
			wantAddedSuffix: "/div[1]/p[1]",
		},
		{
			name:      "stray_close_p_equals_written_p",
			before:    `<div><p></p></div>`,
			after:     `<div></p></div>`,
			wantEqual: true,
		},
		{
			name:            "stray_close_br_adds_element",
			before:          `<div></div>`,
			after:           `<div></br></div>`,
			wantAddedSuffix: "/div[1]/br[1]",
		},
		{
			name:      "stray_close_br_equals_written_br",
			before:    `<div><br></div>`,
			after:     `<div></br></div>`,
			wantEqual: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := assertOracleAgreement(t, test.before, test.after, test.wantEqual)
			if test.wantAddedSuffix == "" {
				return
			}
			for _, change := range result.Changes {
				if change.Kind == "added" && strings.HasSuffix(change.AfterPath, test.wantAddedSuffix) {
					return
				}
			}
			t.Fatalf("no added change ending in %s: %+v", test.wantAddedSuffix, result.Changes)
		})
	}
}

// TestDiffLineOpsNearBudgetReplayIsDeterministic pins the line diff to a
// deterministic result at the input-line budget. A fully reversed document is the
// worst case for the Myers search; the minimal alignment keeps at least one equal
// line, whereas a wall-clock deadline yields a coarse delete-all/insert-all diff
// that varies with scheduler load.
func TestDiffLineOpsNearBudgetReplayIsDeterministic(t *testing.T) {
	oldLines := make([]diffSourceLine, 0, maxDiffInputLines)
	newLines := make([]diffSourceLine, 0, maxDiffInputLines)
	for index := range maxDiffInputLines {
		oldLines = append(oldLines, diffSourceLine{identity: "line" + strconv.Itoa(index)})
		newLines = append(newLines, diffSourceLine{identity: "line" + strconv.Itoa(maxDiffInputLines-1-index)})
	}
	var first string
	for run := range 3 {
		ops, ok := diffLineOps(oldLines, newLines)
		if !ok {
			t.Fatalf("run %d rejected the input-line budget", run)
		}
		kinds := diffOpKinds(ops)
		if strings.Count(kinds, " ") == 0 {
			t.Fatalf("run %d produced a coarse diff with no equal line", run)
		}
		if run == 0 {
			first = kinds
		} else if kinds != first {
			t.Fatalf("run %d differs from run 0", run)
		}
	}
}

package service

import (
	"fmt"
	"strings"
	"testing"
)

func TestDiffWrapperOwnChangesMatrix(t *testing.T) {
	padding := strings.Repeat("a", maxDiffCompareText+128)
	tests := []struct {
		name, before, after string
	}{
		{"html_attribute", `<html lang="en"><body>x</body></html>`, `<html lang="fr"><body>x</body></html>`},
		{"head_attribute", `<html><head data-theme="dark"></head><body>x</body></html>`, `<html><head data-theme="light"></head><body>x</body></html>`},
		{"body_attribute", `<html><body class="dark">x</body></html>`, `<html><body class="light">x</body></html>`},
		{"body_loose_text_case", `<html><body>Hello</body></html>`, `<html><body>hello</body></html>`},
		{"body_loose_text_past_display_limit", `<html><body>` + padding + ` old</body></html>`, `<html><body>` + padding + ` new</body></html>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Modified != 1 || len(result.Changes) != 1 {
				t.Fatalf("wrapper edit missing: summary=%+v changes=%+v hunks=%d", result.Summary, result.Changes, len(result.CodeHunks))
			}
			if len(result.CodeHunks) == 0 {
				t.Fatal("wrapper edit produced no code hunk")
			}
		})
	}
}

func TestDiffEOFRecoveredMarkupMatrix(t *testing.T) {
	for _, test := range []struct {
		name, before, after string
	}{
		{"unclosed_comment", `<p>old</p><!-- draft`, `<p>new</p><!-- draft`},
		{"comment_only_tail", `<p>same</p><!-- old`, `<p>same</p><!-- new`},
		{"dangling_lt", `<p>old</p><`, `<p>new</p><`},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatalf("publishable EOF recovery returned %v", err)
			}
			if len(result.CodeHunks) == 0 {
				t.Fatal("source edit produced no code hunk")
			}
		})
	}
}

func TestDiffFlatFragmentWithoutAIDsStaysWithinBudget(t *testing.T) {
	var before, after strings.Builder
	for index := range 550 {
		text := fmt.Sprintf("item-%03d", index)
		beforeText, afterText := text, text
		if index == 10 || index == 275 || index == 540 {
			beforeText += "-before"
			afterText += "-after"
		}
		fmt.Fprintf(&before, "<p id=\"p%d\">%s</p>", index, beforeText)
		fmt.Fprintf(&after, "<p id=\"p%d\">%s</p>", index, afterText)
	}
	result, err := buildVersionDiff(1, 2, before.String(), after.String())
	if err != nil {
		t.Fatalf("flat fragment rejected: %v", err)
	}
	if result.Summary.Modified != 3 {
		t.Fatalf("modified = %d, want 3", result.Summary.Modified)
	}
}

func TestDiffStructuralAndCodeHunkConsistencyContract(t *testing.T) {
	tests := []struct {
		name, before, after string
		codeOnly            bool
	}{
		{"element_text", `<p>old</p>`, `<p>new</p>`, false},
		{"wrapper_attribute", `<body class="old"><p>x</p></body>`, `<body class="new"><p>x</p></body>`, false},
		{"formatting_only", `<main><p>x</p></main>`, "<main>\n  <p>x</p>\n</main>", true},
		{"comment_only", `<p>x</p><!-- old -->`, `<p>x</p><!-- new -->`, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := buildVersionDiff(1, 2, test.before, test.after)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.CodeHunks) == 0 {
				t.Fatal("changed source produced no code hunk")
			}
			if test.codeOnly {
				if len(result.Changes) != 0 {
					t.Fatalf("classified code-only edit produced structural changes: %+v", result.Changes)
				}
			} else if len(result.Changes) == 0 {
				t.Fatalf("structural edit has hunks but no changes: %+v", result.CodeHunks)
			}
		})
	}
}

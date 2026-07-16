// Package httpx is the HTTP transport layer: router assembly, middleware, and
// thin handlers over the service layer. Handlers validate and shape; all logic
// lives in services.
package httpx

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lml2468/octo-doc/internal/core"
)

// forkExportInput is the input to buildForkExport.
type forkExportInput struct {
	Slug      string
	Version   int
	HTML      string
	Comments  []core.CommentSnapshot
	Kind      string // "export" | "fork"
	OverlayJS string
	Now       string
}

// buildForkExport assembles the /export and /fork HTML: an agent-readable banner,
// a JSON block, inline ODOC-COMMENT markers around anchored text, and (for fork)
// the overlay booted read-only. Ported from fork-export.ts.
func buildForkExport(in forkExportInput) (string, error) {
	open := make([]core.CommentSnapshot, 0, len(in.Comments))
	for _, c := range in.Comments {
		if c.Status != "resolved" {
			open = append(open, c)
		}
	}
	banner := buildBanner(in, open)

	jsonBlock, err := core.SafeJSONForScript(map[string]any{
		"slug":     in.Slug,
		"version":  in.Version,
		"exported": in.Now,
		"comments": toCommentDTOs(open), // wire DTO (created_at) — consistent with the live API
	})
	if err != nil {
		return "", err
	}
	block := `<script type="application/json" id="odoc-fork-comments">` + jsonBlock + "</script>\n"

	body := markAnchoredText(in.HTML, open)
	if in.Kind == "fork" {
		body, err = core.InjectOverlayCfg(body, in.OverlayJS, core.OverlayConfig{
			Slug:         in.Slug,
			Version:      in.Version,
			Identity:     nil,
			Mode:         "fork",
			OriginalSlug: in.Slug,
		})
		if err != nil {
			return "", err
		}
	}
	return banner + block + body, nil
}

func reactionsText(rs core.Reactions) string {
	var parts []string
	for emoji, users := range rs {
		if len(users) > 0 {
			parts = append(parts, fmt.Sprintf("%s (%d)", core.ForHTMLComment(emoji), len(users)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "    reactions: " + strings.Join(parts, ", ") + "\n"
}

func describeAnchor(c core.CommentSnapshot) string {
	if c.Anchor != nil && c.Anchor.Kind == "element" {
		label := c.Anchor.Label
		if label == "" {
			label = c.Anchor.Selector
		}
		if label == "" {
			label = "element"
		}
		return "(on " + core.ForHTMLComment(label) + ")"
	}
	if c.Anchor != nil && c.Anchor.Text != "" {
		t := strings.ReplaceAll(c.Anchor.Text, `"`, `\"`)
		return `(on text: "` + core.ForHTMLComment(truncate(t, 120)) + `")`
	}
	return "(no anchor)"
}

func buildBanner(in forkExportInput, open []core.CommentSnapshot) string {
	var b strings.Builder
	b.WriteString("<!--\n  ===== octo-doc fork export =====\n")
	b.WriteString("  slug: " + core.ForHTMLComment(in.Slug) + "\n")
	b.WriteString("  version: " + core.ForHTMLComment(strconv.Itoa(in.Version)) + "\n")
	b.WriteString("  exported: " + in.Now + "\n\n")
	b.WriteString("  ## How to use this file\n")
	b.WriteString("  Save it as ~/odocs/<your-new-slug>/v1/index.html (or anywhere you like).\n")
	b.WriteString("  Comments below are read-only metadata bundled with the fork. Agents can\n")
	b.WriteString("  read them to apply changes — say \"apply all comments to this doc\" and the\n")
	b.WriteString("  agent will find the anchored regions (marked with ODOC-COMMENT html\n")
	b.WriteString("  comments inline below) and modify them accordingly.\n\n")
	b.WriteString("  ## Comments included in this export\n")
	b.WriteString("  " + strconv.Itoa(len(open)) + " comment(s).\n")
	for i, c := range open {
		who := "anonymous"
		if c.Author != nil && c.Author.Login != "" {
			who = "@" + core.ForHTMLComment(c.Author.Login)
		}
		fmt.Fprintf(&b, "\n  [%d] %s %s\n", i+1, who, describeAnchor(c))
		b.WriteString(`    "` + core.ForHTMLComment(strings.ReplaceAll(c.Text, "\n", " ")) + "\"\n")
		b.WriteString(reactionsText(c.Reactions))
		for _, r := range c.Replies {
			rWho := "anonymous"
			if r.Author != nil && r.Author.Login != "" {
				rWho = "@" + core.ForHTMLComment(r.Author.Login)
			}
			b.WriteString("      ↳ " + rWho + `: "` + core.ForHTMLComment(strings.ReplaceAll(r.Text, "\n", " ")) + "\"\n")
			b.WriteString(indentLines(reactionsText(r.Reactions), "  "))
		}
	}
	b.WriteString("\n  ===== end octo-doc fork export =====\n-->\n")
	return b.String()
}

func markAnchoredText(html string, open []core.CommentSnapshot) string {
	out := html
	for _, c := range open {
		if c.Anchor == nil || c.Anchor.Text == "" || len(c.Anchor.Text) < 2 {
			continue
		}
		needle := c.Anchor.Text
		idx := strings.Index(out, needle)
		if idx == -1 {
			continue
		}
		who := "anonymous"
		if c.Author != nil && c.Author.Login != "" {
			who = c.Author.Login
		}
		marker := `<!--ODOC-COMMENT id="` + core.ForHTMLComment(c.ID) + `" by="` + core.ForHTMLComment(who) + `"-->` + needle + `<!--/ODOC-COMMENT-->`
		out = out[:idx] + marker + out[idx+len(needle):]
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var lineStartRe = regexp.MustCompile(`(?m)^`)

func indentLines(s, prefix string) string {
	if s == "" {
		return ""
	}
	return lineStartRe.ReplaceAllString(s, prefix)
}

// nowISO returns the current UTC time in ISO format. Wrapped so handlers can be
// tested with a fixed clock if needed.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

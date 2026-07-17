package httpx

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/core"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

func (s *Server) handleLanding(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(landingHTML(s.cfg.RepoURL)))
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) error {
	session, err := s.resolveViewerSession(r)
	if err != nil {
		return err
	}
	if !s.auth.IsOwner(session) {
		http.Redirect(w, r, s.cfg.RepoURL, http.StatusFound)
		return nil
	}
	docs, err := s.docs.ListAllForOwner(r.Context())
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(catalogHTML(session, docs)))
	return nil
}

// pageTokens is the shared :root design-token block for the landing and catalog
// pages. Values mirror DESIGN.md (repo root) and the overlay's own :root block in
// assets/overlay.js — keep the three in sync. This is what makes the pages read
// as the same product as the injected doc chrome.
const pageTokens = `
  :root {
    --octo-primary: #1652f0;
    --octo-primary-hover: #1245d0;
    --octo-ink: #1a1a1a;
    --octo-ink-strong: #111;
    --octo-muted: #888;
    --octo-muted-dark: #666;
    --octo-surface: #fff;
    --octo-surface-subtle: #f5f6f8;
    --octo-border: #e5e5e7;
    --octo-hairline: #eee;
    --octo-radius-md: 6px;
    --octo-radius-lg: 10px;
    --octo-shadow-fab: 0 4px 16px rgba(22,82,240,0.30);
    --octo-shadow-card: 0 2px 8px rgba(0,0,0,0.05);
  }`

// brandMark is a small CSS-drawn logo tile (rounded blue square with an "o·d"
// monogram) — token-driven, no raster asset to keep in sync. Shared by the pages.
const brandMark = `<span class="octo-mark" aria-hidden="true">o<span class="dot">·</span>d</span>`

func landingHTML(repo string) string {
	repoLabel := strings.TrimPrefix(strings.TrimPrefix(repo, "https://"), "http://")
	return `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>octo-doc</title>
<style>` + pageTokens + `
  * { box-sizing: border-box; }
  body { font: 15px system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    min-height: 100vh; margin: 0; display: flex; flex-direction: column;
    align-items: center; justify-content: center; color: var(--octo-ink);
    background: var(--octo-surface); padding: 24px; }
  .card { display: flex; flex-direction: column; align-items: center; gap: 12px;
    text-align: center; max-width: 420px; }
  .octo-mark { display: inline-flex; align-items: center; justify-content: center;
    width: 56px; height: 56px; border-radius: var(--octo-radius-lg);
    background: var(--octo-primary); color: #fff; font-weight: 700; font-size: 22px;
    letter-spacing: -0.02em; box-shadow: 0 4px 16px rgba(22,82,240,0.30); }
  .octo-mark .dot { opacity: 0.7; margin: 0 1px; }
  h1 { font-size: 30px; font-weight: 700; margin: 4px 0 0; color: var(--octo-ink-strong);
    letter-spacing: -0.02em; }
  p { color: var(--octo-muted); margin: 0; font-size: 17px; line-height: 1.5; }
  .sub { margin-top: 10px; font-size: 13px; color: var(--octo-muted); }
  a { color: var(--octo-primary); text-decoration: none; font-weight: 500; }
  a:hover { text-decoration: underline; }
</style></head><body>
  <div class="card">
    ` + brandMark + `
    <h1>octo-doc</h1>
    <p>Prompt-native, commentable documents. Self-hosted.</p>
    <p class="sub">Open a document from its shared link &middot;
      <a href="` + core.EscapeHTML(repo) + `">` + core.EscapeHTML(repoLabel) + `</a></p>
  </div>
</body></html>`
}

func catalogHTML(session *storage.Session, docs []service.OwnerDoc) string {
	var rows strings.Builder
	for _, d := range docs {
		rows.WriteString(`<tr><td><a href="/d/` + core.EscapeHTML(d.Slug) + `/v/` + strconv.Itoa(d.Latest) + `">` +
			core.EscapeHTML(d.Title) + `</a></td><td>` + core.EscapeHTML(d.Slug) + `</td><td>v` + strconv.Itoa(d.Latest) + `</td></tr>`)
	}
	who := ""
	if session != nil && session.Login != "" {
		who = ` &middot; signed in as <b>` + core.EscapeHTML(session.Login) + `</b>`
	}
	body := `<p class="empty">No published docs yet.</p>`
	if len(docs) > 0 {
		body = `<table><thead><tr><th>Title</th><th>Slug</th><th>Version</th></tr></thead><tbody>` + rows.String() + `</tbody></table>`
	}
	return `<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>octo-doc</title>
<style>` + pageTokens + `
  * { box-sizing: border-box; }
  body { font: 15px system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    max-width: 760px; margin: 60px auto; padding: 0 20px; color: var(--octo-ink); }
  .head { display: flex; align-items: center; gap: 12px; margin: 0 0 4px; }
  .octo-mark { display: inline-flex; align-items: center; justify-content: center;
    width: 36px; height: 36px; border-radius: var(--octo-radius-md);
    background: var(--octo-primary); color: #fff; font-weight: 700; font-size: 15px;
    letter-spacing: -0.02em; flex-shrink: 0; }
  .octo-mark .dot { opacity: 0.7; margin: 0 1px; }
  h1 { font-size: 28px; font-weight: 700; margin: 0; color: var(--octo-ink-strong);
    letter-spacing: -0.02em; }
  table { width: 100%; border-collapse: collapse; }
  th, td { text-align: left; padding: 10px 12px; border-bottom: 1px solid var(--octo-border); }
  th { font-size: 12px; text-transform: uppercase; color: var(--octo-muted); letter-spacing: 0.04em; }
  tbody tr:hover { background: var(--octo-surface-subtle); }
  a { color: var(--octo-primary); text-decoration: none; font-weight: 500; }
  a:hover { text-decoration: underline; }
  .empty { color: var(--octo-muted); padding: 40px 0; text-align: center; }
  .who { color: var(--octo-muted); font-size: 13px; margin: 0 0 32px; }
  .who b { color: var(--octo-ink); font-weight: 600; }
</style></head><body>
<div class="head">` + brandMark + `<h1>My docs</h1></div>
<p class="who">Documents hosted on this server` + who + `.</p>
` + body + `
</body></html>`
}

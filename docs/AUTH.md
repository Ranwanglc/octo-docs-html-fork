# Access control: capabilities, codes, and the two-transport model

octo-doc documents are **private by default**. Access is granted by *capabilities*
— credentials that map to a level of access for a specific document. There is no
global public/private switch; privacy is per-document.

## The three capability levels

`resolveCapability(request, slug) → none | reader | author`

| Level | Credential | Can |
| --- | --- | --- |
| **author** | the **write token** (`WRITE_TOKEN`, or a bootstrap token) | read everything incl. drafts; publish, promote, delete; generate/rotate share codes |
| **reader** | a per-doc **share code** | read published versions; comment; react |
| **none** | — | nothing — the server returns **404** (never confirms the doc exists) |

A share code is **read + comment only**. It never unlocks drafts, publishing,
promotion, or deletion — those are the author's alone. Handing out a share link is
therefore safe: it cannot be escalated into write access.

## Per-doc share codes

Every document can have one share code (128-bit, stored **hashed** — a leaked
metadata dump can't reveal it). Mint or rotate it:

```bash
# mint/rotate a read+comment code → { code, url: ".../d/<slug>/v/N?code=<code>" }
curl -sX POST -H "Authorization: Bearer $TOKEN" \
  https://docs.example.com/v1/docs/<slug>/share

# revoke the code — existing links stop working
curl -sX DELETE -H "Authorization: Bearer $TOKEN" \
  https://docs.example.com/v1/docs/<slug>/share
```

or click **Share** in the doc's toolbar. Rotating mints a new code and
invalidates the old one, so a leaked link can be cut off.

## One credential model, two transports

The same capability model is presented two ways, so humans and agents both work
with no special-casing on the server:

- **Browsers** carry the code as `?code=<code>` on the first visit. The server
  validates it, sets an **HttpOnly, SameSite=Lax** cookie scoped to that doc, and
  **302-redirects to the same URL without the query string** — so the secret never
  lingers in browser history, server/proxy logs, or the `Referer` header. Later
  reads and comments ride the cookie automatically.
- **API clients** send the credential as `Authorization: Bearer <cred>` —
  the write token for author operations, or a share code for reader operations
  (e.g. reading comments on a private doc via `GET /v1/comments`). API clients
  never touch cookies.

This is the same split GitHub uses (web session cookie vs. API/CLI token): the
authorization layer is one credential model; only the *transport* differs.

## Drafts

Authoring iterates on a **mutable draft slot** that lives outside the immutable
version numbering. The draft is **author-only** — a share code does not grant
access to it. An author saves the draft with `PUT /v1/docs/<slug>/draft`; a
browser opens it with `?code=<write-token>` → cookie exchange (the write token is
the author credential; it appears in the URL only for the one redirect that
strips it). Promoting the draft (`POST /v1/docs/<slug>/draft/promote`, or the
Publish button) creates an immutable version; that version is then readable by
anyone with the doc's share code.

## Operational notes

- Store codes are compared in constant time; only their SHA-256 hash is persisted.
- Serve over HTTPS in production (`COOKIE_SECURE=true`, the default) so codes and
  cookies are never sent in cleartext. The local docker stack runs plain HTTP for
  convenience.
- There is **no** `PRIVATE` env var anymore — it was a global switch, superseded
  by per-doc capabilities.

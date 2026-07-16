#!/usr/bin/env bash
# octo.sh — a thin curl + jq wrapper over the octo-doc /v1 HTTP API.
#
# octo-doc is API-only; this script is the convenience layer the skill drives so
# the agent doesn't hand-assemble curl for every step. It is intentionally small
# and dependency-light (bash, curl, jq) — NOT a compiled binary.
#
# Config (env):
#   OCTO_BASE_URL   server base, e.g. https://docs.example.com  (required)
#   OCTO_TOKEN      write token — the AUTHOR credential (Bearer)
#   OCTO_CODE       a per-doc share code — the READER credential (Bearer)
#
# Auth: author ops send OCTO_TOKEN; reader ops (pull/comment/react) fall back to
# OCTO_CODE when no token is set. The server maps both to a capability.
#
# Usage:
#   octo.sh bootstrap                          # mint + print the first write token
#   octo.sh publish  <slug> <html-file> [title]# save+create v1 (author)
#   octo.sh draft    <slug> <html-file> [title]# save/overwrite the mutable draft
#   octo.sh promote  <slug> [title]            # draft -> next immutable version
#   octo.sh versions <slug>                    # list a doc's versions
#   octo.sh share    <slug>                    # mint/rotate a read+comment code+URL
#   octo.sh unshare  <slug>                    # revoke the share code
#   octo.sh unpublish <slug>                   # delete the doc (all versions)
#   octo.sh pull     <slug>                    # full comment history (JSON)
#   octo.sh comment  <slug> <text> [version] [anchor-json]   # human comment
#   octo.sh reply    <slug> <parent-id> <text> [status] [applied-in]  # agent reply
#   octo.sh react    <slug> <comment-id> <emoji> [version]   # toggle a reaction
#   octo.sh asset-add   <slug> <file>          # upload media, print its URL
#   octo.sh assets      <slug>                 # list a doc's assets
#   octo.sh render-url  <slug> [version]       # print the doc URL
#
# Every call prints the server's JSON (the {data} on success, {error} on failure)
# and exits non-zero on HTTP >= 400.
set -euo pipefail

die() { printf 'octo: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1"; }
need curl; need jq

: "${OCTO_BASE_URL:?set OCTO_BASE_URL to your octo-doc server}"
BASE="${OCTO_BASE_URL%/}"

# cred picks the strongest credential available: write token (author) else code.
cred() {
  if [ -n "${OCTO_TOKEN:-}" ]; then printf '%s' "$OCTO_TOKEN"; return; fi
  if [ -n "${OCTO_CODE:-}" ]; then printf '%s' "$OCTO_CODE"; return; fi
  printf ''
}

# api METHOD PATH [json-body] — JSON request, returns body, fails on HTTP>=400.
api() {
  local method="$1" path="$2" body="${3:-}" tok; tok="$(cred)"
  local args=(-sS -X "$method" -w '\n%{http_code}')
  [ -n "$tok" ] && args+=(-H "Authorization: Bearer $tok")
  if [ -n "$body" ]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  local out code
  out="$(curl "${args[@]}" "$BASE$path")" || die "request failed: $method $path"
  code="${out##*$'\n'}"; out="${out%$'\n'*}"
  if [ "$code" -ge 400 ]; then
    printf '%s\n' "$out" >&2
    die "HTTP $code on $method $path"
  fi
  printf '%s\n' "$out"
}

# data METHOD PATH [body] — like api() but unwraps the {data} envelope.
data() { api "$@" | jq '.data'; }

require_token() { [ -n "${OCTO_TOKEN:-}" ] || die "this is an author op — set OCTO_TOKEN"; }

# jstr — JSON-encode a shell string safely.
jstr() { jq -Rn --arg s "$1" '$s'; }

cmd="${1:-}"; shift || true
case "$cmd" in
  bootstrap)
    api POST /v1/admin/bootstrap | jq -r '.data.token'
    ;;

  publish|draft)
    require_token
    slug="${1:?slug}"; file="${2:?html-file}"; title="${3:-$slug}"
    [ -f "$file" ] || die "no such file: $file"
    body="$(jq -n --arg slug "$slug" --arg title "$title" --rawfile html "$file" \
      '{slug:$slug, title:$title, html:$html}')"
    if [ "$cmd" = publish ]; then
      data POST /v1/docs "$body"
    else
      data PUT "/v1/docs/$slug/draft" "$body"
    fi
    ;;

  promote)
    require_token
    slug="${1:?slug}"; title="${2:-}"
    body='{}'; [ -n "$title" ] && body="$(jq -n --arg t "$title" '{title:$t}')"
    data POST "/v1/docs/$slug/draft/promote" "$body"
    ;;

  versions)
    slug="${1:?slug}"; data GET "/v1/docs/$slug/versions"
    ;;

  share)
    require_token
    slug="${1:?slug}"; data POST "/v1/docs/$slug/share"
    ;;

  unshare)
    require_token
    slug="${1:?slug}"; data DELETE "/v1/docs/$slug/share"
    ;;

  unpublish)
    require_token
    slug="${1:?slug}"; data DELETE "/v1/docs/$slug"
    ;;

  pull)
    slug="${1:?slug}"
    api GET "/v1/comments?slug=$slug&version=all" | jq '.data'
    ;;

  comment)
    slug="${1:?slug}"; text="${2:?text}"; version="${3:-}"; anchor="${4:-}"
    body="$(jq -n --arg slug "$slug" --arg text "$text" \
      --argjson version "${version:-null}" --argjson anchor "${anchor:-null}" \
      '{slug:$slug, text:$text} + (if $version==null then {} else {version:$version} end)
                                 + (if $anchor==null then {} else {anchor:$anchor} end)')"
    data POST /v1/comments "$body"
    ;;

  reply)
    require_token
    slug="${1:?slug}"; parent="${2:?parent-id}"; text="${3:?text}"
    status="${4:-}"; applied="${5:-}"
    body="$(jq -n --arg slug "$slug" --arg parent "$parent" --arg text "$text" \
      --arg status "$status" --argjson applied "${applied:-null}" \
      '{slug:$slug, parent_id:$parent, text:$text}
       + (if $status=="" then {} else {status:$status} end)
       + (if $applied==null then {} else {applied_in:$applied} end)')"
    data POST /v1/agent/replies "$body"
    ;;

  react)
    slug="${1:?slug}"; cid="${2:?comment-id}"; emoji="${3:?emoji}"; version="${4:-}"
    body="$(jq -n --arg slug "$slug" --arg cid "$cid" --arg emoji "$emoji" \
      --argjson version "${version:-null}" \
      '{slug:$slug, comment_id:$cid, emoji:$emoji}
       + (if $version==null then {} else {version:$version} end)')"
    data POST /v1/reactions "$body"
    ;;

  asset-add)
    require_token
    slug="${1:?slug}"; file="${2:?file}"; tok="$(cred)"
    [ -f "$file" ] || die "no such file: $file"
    out="$(curl -sS -w '\n%{http_code}' -H "Authorization: Bearer $tok" \
      -F "file=@$file" "$BASE/v1/docs/$slug/assets")" || die "upload failed"
    code="${out##*$'\n'}"; out="${out%$'\n'*}"
    [ "$code" -ge 400 ] && { printf '%s\n' "$out" >&2; die "HTTP $code"; }
    printf '%s\n' "$out" | jq '.data'
    ;;

  assets)
    slug="${1:?slug}"; data GET "/v1/docs/$slug/assets"
    ;;

  render-url)
    slug="${1:?slug}"; version="${2:-1}"
    printf '%s/d/%s/v/%s\n' "$BASE" "$slug" "$version"
    ;;

  ""|help|-h|--help)
    sed -n '2,50p' "$0" | sed 's/^# \{0,1\}//'
    ;;

  *)
    die "unknown command: $cmd (try: octo.sh help)"
    ;;
esac

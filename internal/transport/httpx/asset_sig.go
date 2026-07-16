package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// Asset sub-resource signing.
//
// A browser loading an inline <img src="/d/{slug}/assets/{sha}"> cannot carry
// the octo `token` header (native sub-resource loads take no custom headers), so
// such requests reach the doc with no credential and the reader gate 404s them.
// At render time — after the viewer has already passed the doc's read gate — we
// stamp each asset URL with a short-lived HMAC signature bound to slug+sha+exp.
// The asset route accepts a valid signature as proof the URL came from an
// authorized render, without depending on cookies or a share code (so it works
// for creator/session viewers who have neither).

// assetSigTTL bounds how long a rendered asset URL stays loadable. Long enough
// for a page's images to load and brief re-fetches, short enough that a leaked
// URL expires quickly. Only gates asset bytes, not the doc itself.
const assetSigTTL = 15 * time.Minute

// assetSigningKey returns the HMAC key: the dedicated secret when set, else the
// write token (always configured in fusion) so signing works out of the box.
func (s *Server) assetSigningKey() string {
	if s.cfg.AssetSigningSecret != "" {
		return s.cfg.AssetSigningSecret
	}
	return s.cfg.WriteToken
}

// signAsset returns (sig, exp) for an asset URL. exp is a unix-seconds string;
// sig is base64url(HMAC-SHA256(key, slug|sha|exp)).
func (s *Server) signAsset(slug, sha string) (sig, exp string) {
	exp = strconv.FormatInt(time.Now().Add(assetSigTTL).Unix(), 10)
	sig = assetSig(s.assetSigningKey(), slug, sha, exp)
	return sig, exp
}

// verifyAssetSig reports whether sig is a valid, unexpired signature for the
// slug+sha pair. Constant-time compare; empty key/sig never validates.
func (s *Server) verifyAssetSig(slug, sha, sig, exp string) bool {
	key := s.assetSigningKey()
	if key == "" || sig == "" || exp == "" {
		return false
	}
	ts, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > ts {
		return false
	}
	want := assetSig(key, slug, sha, exp)
	return subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1
}

func assetSig(key, slug, sha, exp string) string {
	mac := hmac.New(sha256.New, []byte(key))
	// Length-prefix-free but pipe-delimited over fixed-shape fields (sha is hex,
	// exp is digits, slug has no '|'), so the concatenation is unambiguous.
	mac.Write([]byte(slug + "|" + sha + "|" + exp))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// signAssetURLs stamps every /d/{slug}/assets/{sha} reference in the HTML with a
// short-lived HMAC signature (?sig=&exp=) so native sub-resource loads carry
// their own authorization. Only rewrites this slug's asset refs; leaves
// everything else untouched. Best-effort: a nil/empty key still produces URLs
// (verify side rejects), but in fusion the key is always set.
//
// Input is the stored document HTML whose asset refs are bare (the storage
// contract: signing happens only at this render exit, never written back), so an
// already-signed URL is not expected; we still skip any match already followed by
// a query to stay idempotent if that ever changes.
func (s *Server) signAssetURLs(slug, html string) string {
	prefix := "/d/" + slug + "/assets/"
	re := regexp.MustCompile(regexp.QuoteMeta(prefix) + `[0-9a-f]{64}\??`)
	return re.ReplaceAllStringFunc(html, func(m string) string {
		if strings.HasSuffix(m, "?") {
			return m // already has a query — don't double-sign / clobber it
		}
		sha := strings.TrimPrefix(m, prefix)
		sig, exp := s.signAsset(slug, sha)
		return m + "?sig=" + sig + "&exp=" + exp
	})
}

// requireAssetRead gates the asset route. A valid short-lived signature (minted
// at render time for an already-authorized viewer) authorizes the sub-resource
// directly — this is the path native <img> loads take, since they carry no token
// header/cookie. Absent/invalid signature falls back to the normal reader gate
// (Bearer/cookie/?code), so direct/authenticated fetches still work.
func (s *Server) requireAssetRead(next http.HandlerFunc) http.HandlerFunc {
	readGate := s.requireDocReadHTML(next)
	return func(w http.ResponseWriter, r *http.Request) {
		slug := chi.URLParam(r, "slug")
		sha := chi.URLParam(r, "sha256")
		q := r.URL.Query()
		if s.verifyAssetSig(slug, sha, q.Get("sig"), q.Get("exp")) {
			next(w, r)
			return
		}
		readGate(w, r)
	}
}

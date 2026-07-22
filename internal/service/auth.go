package service

import (
	"context"
	"crypto/subtle"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/config"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
)

// sessionTTLSeconds is the viewer session lifetime (30 days).
const sessionTTLSeconds = 60 * 60 * 24 * 30

// octoSuperAdminRole is the X-Octo-Role value that grants CapAuthor. The
// reverse proxy (OCT-147 docs_proxy) normalises the outgoing role to one of
// {"superAdmin","admin","member"}, so this is a stable wire constant rather
// than a config knob.
const octoSuperAdminRole = "superAdmin"

// AuthService handles write-token validation, admin bootstrap, and viewer
// sessions.
//
// Viewer identity in fusion (OCT-145 方案 C) arrives via internal trust headers
// on every proxied request; there is no local login provider. The session
// machinery (GetSession, CreateSession, Logout, the sessions table) is kept so
// legacy cookie sessions still resolve and so tests can mint sessions directly.
type AuthService struct {
	meta       storage.MetadataStore
	cfg        *config.Config
	lock       sluglock.Locker
	docMembers DocMemberMirror
}

// NewAuthService constructs an AuthService. The locker serializes the one-shot
// bootstrap check-and-set; pass the shared (distributed) locker so bootstrap is
// atomic across app instances, not just within one process.
func NewAuthService(meta storage.MetadataStore, cfg *config.Config, lock sluglock.Locker) *AuthService {
	return &AuthService{meta: meta, cfg: cfg, lock: lock}
}

// WithDocMemberMirror attaches a doc_member mirror so grant changes propagate to
// the rich-doc membership list. Returns s for chaining.
func (s *AuthService) WithDocMemberMirror(m DocMemberMirror) *AuthService {
	s.docMembers = m
	return s
}

// IsValidWriteToken does a constant-time check that token is the static or a
// provisioned write token.
func (s *AuthService) IsValidWriteToken(ctx context.Context, token string) (bool, error) {
	if token == "" {
		return false, nil
	}
	if s.cfg.WriteToken != "" && constantTimeEqual(token, s.cfg.WriteToken) {
		return true, nil
	}
	rec, err := s.meta.GetToken(ctx, token)
	if err != nil {
		return false, err
	}
	return rec != nil, nil
}

// Bootstrap mints the first write token. One-shot: errors once any token exists
// or a static token is configured. The check-and-set runs under a lock so two
// concurrent bootstraps can't both mint a "first" token (single-instance
// guarantee; a multi-instance deployment should disable ALLOW_BOOTSTRAP and
// provision a token out of band).
func (s *AuthService) Bootstrap(ctx context.Context) (string, error) {
	if !s.cfg.AllowBootstrap {
		return "", apperr.Forbidden("bootstrap disabled", "bootstrap_disabled")
	}
	if s.cfg.WriteToken != "" {
		return "", apperr.Conflict("a static WRITE_TOKEN is configured", "static_token_configured")
	}
	var token string
	err := s.lock.With(ctx, "__bootstrap__", func() error {
		exists, aerr := s.meta.AnyToken(ctx)
		if aerr != nil {
			return aerr
		}
		if exists {
			return apperr.Conflict("already bootstrapped", "already_bootstrapped")
		}
		token = NewToken()
		return s.meta.PutToken(ctx, token, storage.TokenRecord{
			Token: token, Created: time.Now().UTC().Format(time.RFC3339), Label: "bootstrap",
		})
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

// MetaFor returns the stored metadata for a slug, or nil when the doc does not
// exist. Exposed so the transport layer can resolve doc ownership (creator uid)
// for author-capability decisions without reaching into the store directly.
func (s *AuthService) MetaFor(ctx context.Context, slug string) (*storage.DocMeta, error) {
	return s.meta.GetMeta(ctx, slug)
}

// DocMembersWired reports whether a doc_member mirror is attached. bestCred
// uses this to decide between the plan③ A3②/A4 doc_member-only path
// (production) and the legacy owner-uid==creator_uid fallback (single-node
// deploys with no rich-doc DB, and in-memory tests that do not wire a mirror).
func (s *AuthService) DocMembersWired() bool { return s.docMembers != nil }

// RoleBySlugUID looks the caller's row up in doc_member for slug. Two-hop:
// slug -> doc_id (via mirror) -> role. Callers use this for plan③ A3
// owner-admin (role=3) short-circuit and A4 reader (role>=1) decisions.
//
// yujiawei round-3 P1a: two very different states must not collapse into the
// same "ok=false" — otherwise capability.go's meta fallback covers both, and
// a revoke on a registered doc silently leaves the legacy meta.grants entry
// still granting read (revoke bypass). docRegistered separates them:
//
//   - docRegistered=false, ok=false: mirror unwired, uid empty, or doc has no
//     doc_member registration yet (async publish, thread-mount, non-mounted).
//     Callers may fall back to legacy meta.
//   - docRegistered=true, ok=false: doc IS registered but uid has no row.
//     Caller MUST treat this as "no access via this tier" and MUST NOT fall
//     back to meta.grants — a stale meta.grants entry from M2 could otherwise
//     resurrect access after DELETE /grants/{uid}.
//   - docRegistered=true, ok=true: role is authoritative.
func (s *AuthService) RoleBySlugUID(ctx context.Context, slug, uid string) (role int, ok, docRegistered bool, err error) {
	if s.docMembers == nil || uid == "" {
		return 0, false, false, nil
	}
	docID, ok, err := s.docMembers.DocIDBySlug(ctx, slug)
	if err != nil {
		return 0, false, false, err
	}
	if !ok {
		return 0, false, false, nil
	}
	role, ok, err = s.docMembers.RoleByDocUID(ctx, docID, uid)
	return role, ok, true, err
}

// GetSession resolves a session from its id, or nil.
func (s *AuthService) GetSession(ctx context.Context, sid string) (*storage.Session, error) {
	if sid == "" {
		return nil, nil
	}
	return s.meta.GetSession(ctx, sid)
}

// CreateSession persists a viewer session and returns its id. Used by the
// OCT-150 http-fallback login provider (/v1/auth/login exchanges an octo token
// for an odoc_sid cookie via this seam); fusion callers do NOT use this path
// (trust-header identity is context-scoped, never persisted). role feeds
// IsOwner (Session.Role == "superAdmin" ⇒ owner); pass "" for no role.
func (s *AuthService) CreateSession(ctx context.Context, login, name, role string, avatarURL *string) (string, error) {
	sid := NewSessionID()
	if name == "" {
		name = login
	}
	session := storage.Session{
		Login: login, Name: name, AvatarURL: avatarURL, Role: role,
		Created: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.meta.PutSession(ctx, sid, session, sessionTTLSeconds); err != nil {
		return "", err
	}
	return sid, nil
}

// IsOwner reports whether a session grants owner (admin) authority.
//
// Two paths: (1) octo superAdmin — Session.Role == "superAdmin", set from the
// X-Octo-Role trust header (OCT-145 方案 C, reverse proxy is the source of
// truth for the role value). (2) legacy OWNER env — a single login string,
// kept for back-compat with local deploys that do not run behind the octo
// proxy. Either path is enough; a nil session is always non-owner. Login
// matching is case-insensitive; role match is exact against the wire constant.
func (s *AuthService) IsOwner(session *storage.Session) bool {
	if session == nil {
		return false
	}
	if session.Role == octoSuperAdminRole {
		return true
	}
	owner := strings.ToLower(s.cfg.Owner)
	return owner != "" && strings.ToLower(session.Login) == owner
}

// Logout destroys a session.
func (s *AuthService) Logout(ctx context.Context, sid string) error {
	if sid == "" {
		return nil
	}
	return s.meta.DeleteSession(ctx, sid)
}

// LoginEnabled reports whether a login provider sits in front of us. Two
// wire-ups turn it on: (1) OCT-145 方案 C reverse-proxy path — LOGIN_ENABLED=1
// tells the overlay a proxy fronts us; (2) OCT-150 http-fallback path —
// OCTO_SERVER_BASE_URL is set so /v1/auth/login is live. Overlay reads it via
// /auth/me.authConfigured to decide whether to render the login affordance.
// Both off ⇒ stand-alone deploy, overlay stays anonymous. Does NOT gate the
// identity middleware: trust headers are consumed unconditionally so a
// misconfigured flag cannot lock the admin out of a fusion deploy.
func (s *AuthService) LoginEnabled() bool {
	return s.cfg.LoginEnabled || s.cfg.OctoServerBaseURL != ""
}

// SessionTTLSeconds exposes the cookie max-age.
func (s *AuthService) SessionTTLSeconds() int { return sessionTTLSeconds }

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

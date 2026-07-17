package httpx

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/octoidentity"
)

func (s *Server) handlePing(w http.ResponseWriter, _ *http.Request) {
	writeData(w, 200, map[string]any{"ok": true, "service": "octo-doc"})
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if s.health != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := s.health(ctx); err != nil {
			if s.logger != nil {
				s.logger.Error("healthz check failed", "err", err.Error())
			}
			writeData(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
			return
		}
	}
	writeData(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) error {
	token, err := s.auth.Bootstrap(r.Context())
	if err != nil {
		return err
	}
	writeData(w, 200, map[string]any{"token": token})
	return nil
}

func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) error {
	session, err := s.resolveViewerSession(r)
	if err != nil {
		return err
	}
	var identity any
	if session != nil {
		identity = map[string]any{
			"login": session.Login, "avatar_url": session.AvatarURL, "name": session.Name,
		}
	}
	writeData(w, 200, map[string]any{
		"identity":       identity,
		"isOwner":        s.auth.IsOwner(session),
		"authConfigured": s.auth.LoginEnabled(),
	})
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) error {
	if err := s.auth.Logout(r.Context(), sessionCookie(r)); err != nil {
		return err
	}
	clearCookie(w, sessionCookieName, s.cfg.CookieSecure)
	writeData(w, 200, map[string]any{"ok": true})
	return nil
}

// handleLogin is the OCT-150 http-fallback login provider: browser POSTs an
// octo token, doc verifies it via octo-server, mints a session, and sets the
// odoc_sid cookie. Trust-header path (OCT-145 方案 C) never reaches here —
// its identity is context-scoped and never persisted.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) error {
	id, err := octoidentity.Get()
	if err != nil {
		if errors.Is(err, octoidentity.ErrDisabled) {
			return apperr.NotFound("login provider not configured")
		}
		return err
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return apperr.Validation("invalid request body", "invalid_body")
	}
	if req.Token == "" {
		return apperr.Unauthorized("login required", "login_required")
	}
	user, err := id.VerifyToken(r.Context(), req.Token)
	if err != nil {
		return err
	}
	if user == nil {
		return apperr.Unauthorized("login required", "login_required")
	}
	var avatar *string
	if user.Avatar != "" {
		a := user.Avatar
		avatar = &a
	}
	sid, err := s.auth.CreateSession(r.Context(), user.UID, user.Name, user.Role, avatar)
	if err != nil {
		return err
	}
	setSessionCookie(w, sid, s.auth.SessionTTLSeconds(), s.cfg.CookieSecure)
	// Response body deliberately opaque — no uid/token echo, cookie is the
	// only credential the client keeps.
	writeData(w, 200, map[string]any{"ok": true})
	return nil
}

package httpx

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/lml2468/octo-doc/internal/platform/apperr"
)

// rateLimiter is a fixed-window limiter keyed by token+IP, in-memory (correct for
// a single instance).
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string]*window
	window time.Duration
	max    int
}

type window struct {
	count   int
	resetAt time.Time
}

func newRateLimiter(win time.Duration, max int) *rateLimiter {
	return &rateLimiter{hits: map[string]*window{}, window: win, max: max}
}

// allow reports whether the request may proceed; if not, it returns the seconds
// until reset.
func (rl *rateLimiter) allow(token, ip string, now time.Time) (bool, int) {
	if rl.max <= 0 {
		return true, 0
	}
	key := truncate(token, 16) + "|" + ip
	rl.mu.Lock()
	defer rl.mu.Unlock()
	w := rl.hits[key]
	if w == nil || w.resetAt.Before(now) {
		w = &window{count: 0, resetAt: now.Add(rl.window)}
		rl.hits[key] = w
	}
	w.count++
	if w.count > rl.max {
		return false, int(w.resetAt.Sub(now).Seconds()) + 1
	}
	if len(rl.hits) > 10_000 {
		for k, v := range rl.hits {
			if v.resetAt.Before(now) {
				delete(rl.hits, k)
			}
		}
	}
	return true, 0
}

// limit wraps a handler, enforcing the limit. When skipGET is true, GET requests
// pass through unmetered (for mixed read/write paths).
func (s *Server) limit(rl *rateLimiter, skipGET bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if skipGET && r.Method == http.MethodGet {
			next(w, r)
			return
		}
		ok, retry := rl.allow(bearerToken(r), s.clientIP(r), time.Now())
		if !ok {
			writeErr(w, s.logger, apperr.RateLimited(retry))
			return
		}
		next(w, r)
	}
}

// resolveClientIP extracts the rate-limit key IP. It honors X-Forwarded-For /
// X-Real-IP ONLY when TrustProxyHeaders is set (i.e. the server sits behind a
// trusted proxy that sets them); otherwise those headers are attacker-controlled
// and are ignored in favor of the actual socket peer, so the limit can't be
// evaded by spoofing a header. Falls back to RemoteAddr, never a shared literal.
func (s *Server) resolveClientIP(r *http.Request) string {
	if s.cfg.TrustProxyHeaders {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// bearerToken extracts a Bearer token from the Authorization header, or "".
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// ctxKey is the private type for this package's context keys.
type ctxKey int

const clientIPKey ctxKey = iota

// accessLog is chi middleware that emits one structured access-log line per
// request and echoes the request id (from middleware.RequestID) as X-Request-Id
// for correlation. It resolves the client IP once and stashes it on the context
// so the rate limiter reuses it rather than re-parsing headers.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := middleware.GetReqID(r.Context())
		if reqID != "" {
			w.Header().Set("X-Request-Id", reqID)
		}
		ip := s.resolveClientIP(r)
		r = r.WithContext(context.WithValue(r.Context(), clientIPKey, ip))
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		if s.logger != nil {
			s.logger.Info("request",
				"request_id", reqID,
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(start).Milliseconds(),
				"ip", ip,
			)
		}
	})
}

// clientIP returns the request's client IP, using the value resolved once by
// accessLog when present, else resolving it directly.
func (s *Server) clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(clientIPKey).(string); ok {
		return ip
	}
	return s.resolveClientIP(r)
}

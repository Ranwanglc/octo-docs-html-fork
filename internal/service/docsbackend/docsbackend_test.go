package docsbackend

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientRejectsEveryRedirectWithoutForwardingBearerToken(t *testing.T) {
	for _, status := range []int{301, 302, 303, 307, 308} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			targetCalls := 0
			target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				targetCalls++
				if strings.Contains(r.Header.Get("Authorization"), "secret") {
					t.Error("redirect target received bearer token")
				}
			}))
			defer target.Close()

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", target.URL+"/stolen")
				w.WriteHeader(status)
			}))
			defer origin.Close()

			client := newWithTimeout(origin.URL+"/v1/bot/docs", "configured-secret", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
			_, err := client.Register(context.Background(), Registration{OctoDocSlug: "doc-1"}, "request-secret")
			if err == nil {
				t.Fatal("redirect response accepted")
			}
			if targetCalls != 0 {
				t.Fatalf("redirect target calls = %d, want 0", targetCalls)
			}
		})
	}
}

func TestClientCanonicalRegisterRejectsIncompletePublisherIdentity(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"docId":"d1","octoDocSlug":"d1","shareUrl":"https://x/d/d1","created":true}`))
	}))
	defer ts.Close()
	client := newWithTimeout(ts.URL, "", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := client.Register(context.Background(), Registration{IdempotencyKey: "k"}, "tok")
	if !IsRegistrationContractIncomplete(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestClientLegacyRegisterSkipsPublisherIdentityCheck(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"docId":"d1","octoDocSlug":"slug-1","shareUrl":"https://x/d/d1"}`))
	}))
	defer ts.Close()
	client := newWithTimeout(ts.URL, "", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res, err := client.Register(context.Background(), Registration{OctoDocSlug: "slug-1"}, "tok")
	if err != nil || res == nil {
		t.Fatalf("legacy register rejected: %v", err)
	}
}

func TestLogRefPrefersSlugThenIdempotencyKey(t *testing.T) {
	if got := logRef(Registration{OctoDocSlug: "s", IdempotencyKey: "k"}); got != "s" {
		t.Fatalf("logRef = %q", got)
	}
	if got := logRef(Registration{IdempotencyKey: "k"}); got != "k" {
		t.Fatalf("logRef = %q", got)
	}
}

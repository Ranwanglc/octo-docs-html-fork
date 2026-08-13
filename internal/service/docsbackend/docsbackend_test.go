package docsbackend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCanonicalRegistrationTerminalErrorsAreClassifiedThroughWrapping(t *testing.T) {
	deleted := fmt.Errorf("wrapped: %w", &CanonicalDocumentDeletedError{})
	if !IsCanonicalDocumentDeleted(deleted) {
		t.Fatal("wrapped deleted error was not classified")
	}
	incomplete := fmt.Errorf("wrapped: %w", &RegistrationContractIncompleteError{})
	if !IsRegistrationContractIncomplete(incomplete) {
		t.Fatal("wrapped contract error was not classified")
	}
	if IsCanonicalDocumentDeleted(errors.New("other")) || IsRegistrationContractIncomplete(errors.New("other")) {
		t.Fatal("unrelated error was classified")
	}
}

func TestInternalRegisterAndBotDeleteURLs(t *testing.T) {
	requests := make(chan *http.Request, 2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(r.Context())
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"docId":"d1","octoDocSlug":"s1","shareUrl":"https://example/d1","created":true}`)
		}
	}))
	defer ts.Close()

	client := newWithTimeout(ts.URL+"/v1/bot/docs", "bot", "internal", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := client.Register(context.Background(), Registration{OctoDocSlug: "s1", Internal: true}, ""); err != nil {
		t.Fatal(err)
	}
	client.Delete(context.Background(), "s1", "")
	post, del := <-requests, <-requests
	if post.Method != http.MethodPost || post.URL.Path != "/internal/html/register" {
		t.Fatalf("register = %s %s", post.Method, post.URL.Path)
	}
	if del.Method != http.MethodDelete || del.URL.Path != "/v1/bot/docs/octo-doc/s1" {
		t.Fatalf("delete = %s %s", del.Method, del.URL.Path)
	}
	if post.Header.Get("X-Internal-Token") != "internal" || del.Header.Get("Authorization") != "Bearer bot" {
		t.Fatal("register/delete did not use their distinct authentication channels")
	}
}

func TestClientRejectsEveryRedirect(t *testing.T) {
	redirected := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := newWithTimeout(origin.URL+"/v1/bot/docs", "bot", "internal", time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := client.Register(context.Background(), Registration{OctoDocSlug: "s1"}, "")
	if err == nil {
		t.Fatal("redirect response accepted")
	}
	if redirected {
		t.Fatal("client followed redirect")
	}
}

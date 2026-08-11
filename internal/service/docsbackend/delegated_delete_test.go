package docsbackend

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestDeleteDelegatedExactBodySignatureHeadersAndNoBearer(t *testing.T) {
	secret := strings.Repeat("*", 32)
	var gotBody []byte
	var gotHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != delegatedDeletePath || r.Method != http.MethodDelete {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := New(server.URL+"/v1/bot/docs", "process-token-must-not-leak", nil)
	in := DelegatedDelete{Slug: "doc-42", DocID: "doc-42", ActorUID: "human"}
	if err := client.DeleteDelegated(context.Background(), in, secret); err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(in)
	if string(gotBody) != string(want) {
		t.Fatalf("body=%s want=%s", gotBody, want)
	}
	timestamp := gotHeader.Get("X-Octo-Timestamp")
	if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		t.Fatalf("timestamp=%q", timestamp)
	}
	digest := sha256.Sum256(gotBody)
	input := fmt.Sprintf("v1\nDELETE\n%s\n%s\n%x", delegatedDeletePath, timestamp, digest)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(input))
	if got := gotHeader.Get("X-Octo-Signature"); got != fmt.Sprintf("v1=%x", mac.Sum(nil)) {
		t.Fatalf("signature=%q", got)
	}
	if gotHeader.Get("Content-Type") != "application/json" || gotHeader.Get("Authorization") != "" || gotHeader.Get("Accept") != "" {
		t.Fatalf("headers=%v", gotHeader)
	}
}

func TestDeleteDelegatedRejectsRedirectAndEmptySecret(t *testing.T) {
	var redirected bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := New(server.URL, "process-token", nil)
	in := DelegatedDelete{Slug: "legacy", ActorUID: "human"}
	if err := client.DeleteDelegated(context.Background(), in, ""); err == nil {
		t.Fatal("empty secret accepted")
	}
	err := client.DeleteDelegated(context.Background(), in, strings.Repeat("s", 32))
	if err == nil || redirected {
		t.Fatalf("err=%v redirected=%v", err, redirected)
	}
}

func TestDeleteDelegatedRejectsNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer server.Close()
	client := New(server.URL+"/v1/bot/docs", "", nil)
	err := client.DeleteDelegated(context.Background(), DelegatedDelete{Slug: "doc-42", ActorUID: "human"}, strings.Repeat("s", 32))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err=%v, want HTTP 404 failure", err)
	}
}

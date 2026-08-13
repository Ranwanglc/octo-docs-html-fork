package httpx_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"testing"
)

func canonicalDraftMultipart(t *testing.T, html []byte) (string, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("idempotency_key", "draft-key"); err != nil {
		t.Fatal(err)
	}
	f, err := w.CreateFormFile("file", "draft.html")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(html); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.String(), w.FormDataContentType()
}

func TestCanonicalDraftMultipartRejectsEmptyAndOversizedHTML(t *testing.T) {
	withStubIdentity(t, stubIdentity{botUID: "publisher-bot", botName: "Publisher", botSpaceID: "space-1", botOwnerUID: "owner-1"})
	h := newTestServer(t, ownerAuthCfg())
	for _, tc := range []struct {
		name string
		html []byte
		want int
		code string
	}{
		{name: "empty", html: nil, want: http.StatusBadRequest, code: "html_required"},
		{name: "oversized", html: bytes.Repeat([]byte("x"), (5<<20)+1), want: http.StatusRequestEntityTooLarge, code: "html_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, contentType := canonicalDraftMultipart(t, tc.html)
			rec := do(t, h, http.MethodPost, "/v1/docs/draft", map[string]string{"Authorization": "Bearer publisher-token", "Content-Type": contentType}, body)
			if rec.Code != tc.want || !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"`+tc.code+`"`)) {
				t.Fatalf("status=%d body=%s; want %d %s", rec.Code, rec.Body.String(), tc.want, tc.code)
			}
		})
	}
}

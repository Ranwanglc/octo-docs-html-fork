package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

type identityRegistrar struct {
	result *docsbackend.RegistrationResult
}

func (r *identityRegistrar) Register(context.Context, docsbackend.Registration, string) (*docsbackend.RegistrationResult, error) {
	return r.result, nil
}
func (*identityRegistrar) Rename(context.Context, string, string, string)          {}
func (*identityRegistrar) Delete(context.Context, string, string) error            { return nil }
func (*identityRegistrar) Published(context.Context, string, string, string) error { return nil }

func identityDocs(r service.DocRegistrar) (*service.DocService, *memory.Store) {
	store := memory.New()
	lock := sluglock.NewMemory()
	return service.NewDocService(store, store, service.NewCommentService(store, lock), lock, "", 5<<20).WithDocsBackendRegistration(r, nil), store
}

func identityState(t *testing.T, store *memory.Store, slug string) []byte {
	t.Helper()
	meta, err := store.GetMeta(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	comments, err := store.GetComments(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	draft, draftOK, err := store.GetDraft(t.Context(), slug)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(struct {
		Meta     *storage.DocMeta
		Comments any
		Draft    string
		DraftOK  bool
	}{meta, comments, draft, draftOK})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCanonicalReplayDenialDoesNotRepairHalfState(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "draft"}[draft], func(t *testing.T) {
			const slug = "doc-half"
			r := &identityRegistrar{result: &docsbackend.RegistrationResult{DocID: slug, OctoDocSlug: slug, ShareURL: "https://share/doc-half", PublisherUID: "publisher", SpaceID: "space", Created: false}}
			docs, store := identityDocs(r)
			meta := storage.DocMeta{Slug: slug, Title: "unchanged", Extra: map[string]any{storage.CanonicalDocIDExtraKey: slug, storage.CanonicalShareURLExtraKey: "https://share/doc-half", "sentinel": "keep"}}
			if err := store.PutMeta(t.Context(), slug, meta); err != nil {
				t.Fatal(err)
			}
			if draft {
				_, err := store.PutDraft(t.Context(), slug, "draft-bytes")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				_, err := store.PutDoc(t.Context(), slug, 1, "publish-bytes")
				if err != nil {
					t.Fatal(err)
				}
			}
			before := identityState(t, store, slug)
			in := service.PublishInput{HTML: "replacement", IdempotencyKey: "same", PublisherToken: "token", PublisherUID: "publisher", PublisherSpaceID: "space"}
			deny := func(string, bool) error { return apperr.NotFound("Not found") }
			var err error
			if draft {
				_, err = docs.SaveDraftMountedAuthorized(t.Context(), in, deny)
			} else {
				_, err = docs.PublishAuthorized(t.Context(), in, deny)
			}
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
				t.Fatalf("err=%v, want hidden deny", err)
			}
			after := identityState(t, store, slug)
			if string(after) != string(before) {
				t.Fatalf("state mutated\nbefore=%s\nafter=%s", before, after)
			}
		})
	}
}

func TestCanonicalCreateRejectsBackendPublisherMismatchBeforeAnyWrite(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "draft"}[draft], func(t *testing.T) {
			r := &identityRegistrar{result: &docsbackend.RegistrationResult{DocID: "doc-spoof", OctoDocSlug: "doc-spoof", ShareURL: "u", PublisherUID: "other", SpaceID: "space", Created: true}}
			docs, store := identityDocs(r)
			in := service.PublishInput{HTML: "x", IdempotencyKey: "key", PublisherToken: "token", PublisherUID: "trusted", PublisherSpaceID: "space"}
			var err error
			if draft {
				_, err = docs.SaveDraftMounted(context.Background(), in)
			} else {
				_, err = docs.Publish(context.Background(), in)
			}
			var ae *apperr.Error
			if !errors.As(err, &ae) || ae.Status != http.StatusForbidden {
				t.Fatalf("err=%v, want 403", err)
			}
			if meta, _ := store.GetMeta(context.Background(), "doc-spoof"); meta != nil {
				t.Fatalf("meta=%+v", meta)
			}
			if versions, _ := store.ListVersions(context.Background(), "doc-spoof"); len(versions) != 0 {
				t.Fatalf("versions=%v", versions)
			}
			if _, ok, _ := store.GetDraft(context.Background(), "doc-spoof"); ok {
				t.Fatal("draft persisted")
			}
			if comments, _ := store.GetComments(context.Background(), "doc-spoof"); len(comments) != 0 {
				t.Fatalf("comments=%+v", comments)
			}
		})
	}
}

func TestUnknownRefAlwaysNotFoundWithoutRegistrar(t *testing.T) {
	docs, _ := identityDocs(nil)
	for _, draft := range []bool{false, true} {
		in := service.PublishInput{Slug: "unknown", HTML: "x"}
		var err error
		if draft {
			_, err = docs.SaveDraftMountedAuthorized(context.Background(), in, func(string, bool) error { return nil })
		} else {
			_, err = docs.PublishAuthorized(context.Background(), in, func(string, bool) error { return nil })
		}
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
			t.Fatalf("draft=%v err=%v", draft, err)
		}
	}
}

func TestCanonicalCreateWithoutRegistrarIsUnavailableAndWritesNothing(t *testing.T) {
	docs, store := identityDocs(nil)
	_, err := docs.Publish(context.Background(), service.PublishInput{HTML: "x", IdempotencyKey: "key", PublisherToken: "token", PublisherUID: "bot", PublisherSpaceID: "space"})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "registration_unavailable" {
		t.Fatalf("err=%v", err)
	}
	if metas, _ := store.ListMeta(context.Background()); len(metas) != 0 {
		t.Fatalf("metas=%+v", metas)
	}
}

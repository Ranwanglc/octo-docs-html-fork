package service_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/sluglock"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/service/docsbackend"
	"github.com/Mininglamp-OSS/octo-docs-html/internal/storage/memory"
)

// noopRegistrar is a DocRegistrar that just records that Register ran. It
// serves the goroutine gate in afterPublished so the reconcile hook fires.
type noopRegistrar struct {
	registered atomic.Int32
}

func (r *noopRegistrar) Register(context.Context, docsbackend.Registration, string) {
	r.registered.Add(1)
}
func (*noopRegistrar) Rename(context.Context, string, string, string) {}
func (*noopRegistrar) Delete(context.Context, string, string)         {}

// yujiawei round-4 P1: afterPublished must invoke the injected reconciler
// after the async registration completes so grants written to meta.grants
// during the pre-registration gap survive the strict wired A4 gate.
func TestAfterPublishedTriggersGrantReconciler(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, "", 5<<20)
	registrar := &noopRegistrar{}
	docs = docs.WithDocsBackendRegistration(registrar, nil)

	var called atomic.Int32
	var seenSlug atomic.Value
	docs = docs.WithGrantReconciler(func(_ context.Context, slug string) error {
		called.Add(1)
		seenSlug.Store(slug)
		return nil
	})

	// MountType=group makes registrationForMount return ok=true so the
	// goroutine runs Register then the reconcile hook.
	ctx := context.Background()
	if _, err := docs.Publish(ctx, service.PublishInput{
		Slug:      "docGap",
		HTML:      "<html><body><p>x</p></body></html>",
		MountType: "group",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if called.Load() > 0 && registrar.registered.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := called.Load(); got != 1 {
		t.Fatalf("reconciler called %d times; want 1", got)
	}
	if got := registrar.registered.Load(); got != 1 {
		t.Fatalf("registrar called %d times; want 1 (reconcile must run only after Register)", got)
	}
	if got, _ := seenSlug.Load().(string); got != "docGap" {
		t.Fatalf("reconciler saw slug %q; want docGap", got)
	}
}

// afterPublished with a nil reconciler is a plain fire-and-forget register:
// no panic, no reconcile.
func TestAfterPublishedNilReconcilerSafe(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	docs := service.NewDocService(store, store, comments, locker, "", 5<<20).
		WithDocsBackendRegistration(&noopRegistrar{}, nil)

	if _, err := docs.Publish(context.Background(), service.PublishInput{
		Slug:      "docNoRec",
		HTML:      "<html><body><p>x</p></body></html>",
		MountType: "group",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Give the goroutine a moment; the test passes if we don't panic.
	time.Sleep(50 * time.Millisecond)
}

// thread-mount docs never register, so afterPublished must not fire the
// reconciler for them — matching the "not registerable ⇒ nothing to
// reconcile" invariant.
func TestAfterPublishedThreadMountSkipsReconciler(t *testing.T) {
	store := memory.New()
	locker := sluglock.NewMemory()
	comments := service.NewCommentService(store, locker)
	registrar := &noopRegistrar{}
	var called atomic.Int32
	docs := service.NewDocService(store, store, comments, locker, "", 5<<20).
		WithDocsBackendRegistration(registrar, nil).
		WithGrantReconciler(func(context.Context, string) error {
			called.Add(1)
			return nil
		})

	if _, err := docs.Publish(context.Background(), service.PublishInput{
		Slug:      "docThr",
		HTML:      "<html><body><p>x</p></body></html>",
		MountType: "thread",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if called.Load() != 0 {
		t.Fatalf("reconciler fired on thread-mount doc: %d calls", called.Load())
	}
	if registrar.registered.Load() != 0 {
		t.Fatalf("registrar called on thread-mount doc: %d", registrar.registered.Load())
	}
}

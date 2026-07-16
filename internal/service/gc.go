package service

import (
	"context"
	"regexp"
	"strings"
	"time"
)

// Orphan asset garbage collection (P2.4). Content-addressed assets can outlive the
// HTML that referenced them — an author uploads one and never references it, or
// edits the reference away. GCAssets finds assets no live HTML references and, if
// they are older than a grace period, deletes them. See docs/ASSETS.md (P2.4).

// hexRunRe matches a maximal run of >=64 lowercase-hex chars. References are
// collected by sliding a 64-char window across each run (see scanInto), so a bare
// 64-hex content address is found no matter where it sits — a normal same-origin
// URL, a fork/other doc that copied that URL (#44), or JS that builds the URL from
// a bare sha in a variable/data island (#44). Sliding is required for correctness:
// a plain non-overlapping `[0-9a-f]{64}` match tiles from offset 0 and would MISS
// a real sha that follows a hex run whose length is not a multiple of 64 (e.g. a
// 40-char git sha immediately before the address) — an unsafe
// (delete-a-referenced-asset) miss. Over-generating windows only over-retains,
// which is the safe direction. Stored addresses are lowercase (hex.EncodeToString),
// so lowercase-only matching covers every real reference.
var hexRunRe = regexp.MustCompile(`[0-9a-f]{64,}`)

// maxSlideRun bounds the full offset-by-offset slide. A hex run this long is a
// data blob, not concatenated content-address references, so beyond it scanInto
// falls back to 64-aligned windows (plus the tail). This caps the per-run window
// count at O(len) inserts for realistic runs while avoiding pathological
// O(len)-distinct-key memory blow-up from a multi-MB inline hex payload. The bound
// comfortably exceeds any realistic concatenation of a few content addresses.
const maxSlideRun = 4096

// GCReport summarizes a garbage-collection pass.
type GCReport struct {
	Scanned    int        // asset-bearing slugs scanned
	Assets     int        // asset records examined
	Deleted    []GCDelete // assets removed (or that would be, in dry-run)
	Kept       int        // assets kept (referenced or within grace)
	Referenced int        // assets kept specifically because they are referenced
}

// GCDelete identifies one removed asset.
type GCDelete struct {
	Slug   string
	SHA256 string
	Size   int64
	Reason string // "unreferenced"
}

// GCAssets deletes assets that are BOTH unreferenced by any live HTML AND older
// than `grace` (measured from the asset's Created timestamp against `now`). Assets
// within the grace window are kept even if unreferenced, so an upload not yet
// wired into a draft isn't reaped mid-authoring. In dryRun, nothing is deleted but
// the report lists what would be. `now` is injected for testability.
//
// References are resolved GLOBALLY — across every doc's published versions and
// draft — so a content address reused across docs (e.g. a fork keeping the source
// slug's URL) keeps the asset alive (#44). Asset-bearing slugs are enumerated via
// ListAssetSlugs, not ListMeta, so assets under a slug with no doc row are still
// collected (#45).
//
// Concurrency: the global reference set is a snapshot taken before any lock. Each
// slug's delete decision then runs under that slug's lock, re-scanning the slug's
// OWN current HTML inside the lock and merging with the snapshot — this closes the
// common same-slug race (an author editing their own doc during a pass) (#46).
// A residual cross-doc race remains: a *different* doc that begins referencing an
// asset after the snapshot, while the pass is mid-flight, is not re-observed, so
// that asset could still be deleted. The grace window bounds the exposure (only
// past-grace assets are eligible), and GC is an opt-in maintenance command; a full
// fix would require a global lock or a post-lock global re-scan.
func (s *AssetService) GCAssets(ctx context.Context, grace time.Duration, now time.Time, dryRun bool) (GCReport, error) {
	// Enumerate asset-bearing slugs once and reuse for both the global scan and the
	// per-slug pass (avoids a duplicate ListAssetSlugs round trip).
	assetSlugs, err := s.meta.ListAssetSlugs(ctx)
	if err != nil {
		return GCReport{}, err
	}

	// Phase 1: global reference set from a snapshot of all docs' HTML.
	referenced, err := s.allReferencedSHAs(ctx, assetSlugs)
	if err != nil {
		return GCReport{}, err
	}

	// Phase 2: per asset-bearing slug, decide + delete under the slug lock.
	var report GCReport
	for _, slug := range assetSlugs {
		report.Scanned++
		if err := s.gcSlug(ctx, slug, referenced, grace, now, dryRun, &report); err != nil {
			return report, err
		}
	}
	return report, nil
}

// gcSlug garbage-collects one slug's assets under that slug's lock. It re-scans the
// slug's own current HTML inside the lock and merges those references with the
// global snapshot, so a same-slug publish that landed after the phase-1 snapshot
// cannot cause a just-referenced asset to be deleted (#46).
func (s *AssetService) gcSlug(ctx context.Context, slug string, globalRefs map[string]struct{}, grace time.Duration, now time.Time, dryRun bool, report *GCReport) error {
	return s.lock.With(ctx, slug, func() error {
		// Re-scan this slug's own current HTML under the lock (see #46). Versions are
		// immutable but a concurrent promote can add one, so re-read all of them, not
		// just the draft.
		fresh := map[string]struct{}{}
		if err := s.scanSlugInto(ctx, slug, fresh); err != nil {
			return err
		}
		assets, err := s.meta.ListAssetMeta(ctx, slug)
		if err != nil {
			return err
		}
		for _, a := range assets {
			report.Assets++
			_, refGlobal := globalRefs[a.SHA256]
			_, refFresh := fresh[a.SHA256]
			if refGlobal || refFresh {
				report.Kept++
				report.Referenced++
				continue
			}
			// Unreferenced: keep if still within the grace window (uploaded but not
			// yet wired in). Fail SAFE — if the timestamp can't be parsed, keep the
			// asset rather than delete it (deletion is irreversible).
			within, parsed := withinGrace(a.Created, now, grace)
			if !parsed || within {
				report.Kept++
				continue
			}
			del := GCDelete{Slug: slug, SHA256: a.SHA256, Size: a.Size, Reason: "unreferenced"}
			if !dryRun {
				// Delete first; only record it as deleted on success, so a mid-pass
				// failure doesn't overstate what was reclaimed.
				if derr := s.deleteLocked(ctx, slug, a.SHA256); derr != nil {
					return derr
				}
			}
			report.Deleted = append(report.Deleted, del)
		}
		return nil
	})
}

// allReferencedSHAs scans every doc's published versions and draft for bare
// content addresses, returning the union across ALL slugs. The union is what makes
// cross-doc / fork / JS-built references keep an asset alive (#44). assetSlugs is
// the pre-fetched asset-bearing slug list, folded in so a slug that holds assets
// (and possibly a draft referencing them) but has no doc row is still scanned.
func (s *AssetService) allReferencedSHAs(ctx context.Context, assetSlugs []string) (map[string]struct{}, error) {
	refs := map[string]struct{}{}

	// Every slug that has a doc row (published versions + draft)...
	docs, err := s.meta.ListMeta(ctx)
	if err != nil {
		return nil, err
	}
	scanned := map[string]struct{}{}
	for _, entry := range docs {
		if err := s.scanSlugInto(ctx, entry.Slug, refs); err != nil {
			return nil, err
		}
		scanned[entry.Slug] = struct{}{}
	}
	// ...plus any asset-bearing slug with no doc row (defensive; keeps the union
	// complete for a draft that references its own assets).
	for _, slug := range assetSlugs {
		if _, done := scanned[slug]; done {
			continue
		}
		if err := s.scanSlugInto(ctx, slug, refs); err != nil {
			return nil, err
		}
	}
	return refs, nil
}

// scanSlugInto scans a slug's published versions and draft HTML, adding every bare
// content address it finds to refs.
func (s *AssetService) scanSlugInto(ctx context.Context, slug string, refs map[string]struct{}) error {
	versions, err := s.blobs.ListVersions(ctx, slug)
	if err != nil {
		return err
	}
	for _, n := range versions {
		html, ok, err := s.blobs.GetDoc(ctx, slug, n)
		if err != nil {
			return err
		}
		if ok {
			scanInto(html, refs)
		}
	}
	if html, ok, err := s.blobs.GetDraft(ctx, slug); err != nil {
		return err
	} else if ok {
		scanInto(html, refs)
	}
	return nil
}

// scanInto adds every 64-hex content address in html to refs. It slides a 64-wide
// window across each maximal hex run so an address is found regardless of its
// offset within a longer run (see hexRunRe). For runs longer than maxSlideRun it
// falls back to 64-aligned windows plus the tail window, bounding cost on a huge
// inline hex blob while still catching any realistic reference (a clean
// assets/<sha> is an exact-64 run; short concatenations stay well under the cap).
//
// Each key is copied (via strings.Clone) rather than sliced directly out of html:
// a substring shares the source's backing array, so storing raw slices would pin
// every scanned multi-MB HTML blob in memory for the whole GC pass.
func scanInto(html string, refs map[string]struct{}) {
	// Clone only on first sight: strings.Clone forces an allocation, so checking
	// membership first avoids re-cloning a window already seen (common in a
	// repetitive hex run).
	add := func(w string) {
		if _, ok := refs[w]; !ok {
			refs[strings.Clone(w)] = struct{}{}
		}
	}
	for _, run := range hexRunRe.FindAllString(html, -1) {
		n := len(run)
		if n <= maxSlideRun {
			for i := 0; i+64 <= n; i++ {
				add(run[i : i+64])
			}
			continue
		}
		// Oversized run: 64-aligned windows + the final window.
		for i := 0; i+64 <= n; i += 64 {
			add(run[i : i+64])
		}
		add(run[n-64:])
	}
}

// withinGrace reports whether an asset created at `created` (RFC3339-ish, as
// nowISO writes) is still inside the grace window ending at `now`. The second
// return (parsed) is false when the timestamp can't be parsed; callers fail SAFE
// on !parsed (keep the asset) since deletion is irreversible.
func withinGrace(created string, now time.Time, grace time.Duration) (within bool, parsed bool) {
	t, err := time.Parse(isoLayout, created)
	if err != nil {
		// Try RFC3339 as a fallback for any differently-formatted timestamps.
		t, err = time.Parse(time.RFC3339, created)
		if err != nil {
			return false, false
		}
	}
	return now.Sub(t) < grace, true
}

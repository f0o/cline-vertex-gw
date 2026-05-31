package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genai"
)

// fakeListModels lets us measure how many times the underlying discovery is
// invoked. We can't call ListModels directly without GCP creds, so the cache
// test exercises the cache wrapper via a small helper that takes a function.

// listFunc abstracts the upstream call for the test-only helper below.
type listFunc func(context.Context) ([]*genai.Model, error)

// listModelsCachedWith is a test-only re-implementation of ListModelsCached
// parameterized over the upstream call. The real method's behavior is
// identical except it calls vc.ListModels directly; we mirror its logic
// here so we can assert hit/miss/TTL semantics without standing up a real
// VertexClient.
func listModelsCachedWith(ctx context.Context, fetch listFunc, onHit, onMiss func()) ([]*genai.Model, error) {
	ttl := tagsCacheTTL()
	if ttl <= 0 {
		onMiss()
		return fetch(ctx)
	}

	tagsCache.mu.Lock()
	if tagsCache.entry != nil && time.Since(tagsCache.entry.at) < ttl {
		out := tagsCache.entry.models
		tagsCache.mu.Unlock()
		onHit()
		return out, nil
	}
	tagsCache.mu.Unlock()

	onMiss()
	models, err := fetch(ctx)
	if err != nil {
		return nil, err
	}

	tagsCache.mu.Lock()
	tagsCache.entry = &tagsCacheEntry{models: models, at: time.Now()}
	tagsCache.mu.Unlock()
	return models, nil
}

func TestTagsCacheTTL(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"default when unset", "", defaultTagsTTL},
		{"explicit 30s", "30", 30 * time.Second},
		{"zero disables", "0", 0},
		{"negative falls back to default", "-5", defaultTagsTTL},
		{"garbage falls back to default", "not-a-number", defaultTagsTTL},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				t.Setenv(envTagsCacheTTL, "")
			} else {
				t.Setenv(envTagsCacheTTL, tc.env)
			}
			// Unsetenv-like behavior: when "" the LookupEnv returns ok=true
			// because t.Setenv set it. To exercise the truly-unset path the
			// default-case is covered by the other entries; this branch just
			// verifies a present-but-empty value falls back to default too.
			if got := tagsCacheTTL(); got != tc.want {
				t.Errorf("tagsCacheTTL = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListModelsCachedHitMiss(t *testing.T) {
	t.Setenv(envTagsCacheTTL, "60")
	InvalidateTagsCache()
	t.Cleanup(InvalidateTagsCache)

	var calls int32
	fetch := func(ctx context.Context) ([]*genai.Model, error) {
		atomic.AddInt32(&calls, 1)
		return []*genai.Model{{Name: "publishers/google/models/gemini-test"}}, nil
	}
	var hits, misses int32
	onHit := func() { atomic.AddInt32(&hits, 1) }
	onMiss := func() { atomic.AddInt32(&misses, 1) }

	ctx := context.Background()

	// First call: miss.
	if _, err := listModelsCachedWith(ctx, fetch, onHit, onMiss); err != nil {
		t.Fatalf("call 1: %v", err)
	}
	// Second call: hit.
	if _, err := listModelsCachedWith(ctx, fetch, onHit, onMiss); err != nil {
		t.Fatalf("call 2: %v", err)
	}
	// Third call: hit.
	if _, err := listModelsCachedWith(ctx, fetch, onHit, onMiss); err != nil {
		t.Fatalf("call 3: %v", err)
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("fetch calls = %d, want 1 (subsequent should be served from cache)", got)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("hits = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&misses); got != 1 {
		t.Errorf("misses = %d, want 1", got)
	}
}

func TestListModelsCachedDisabled(t *testing.T) {
	t.Setenv(envTagsCacheTTL, "0")
	InvalidateTagsCache()
	t.Cleanup(InvalidateTagsCache)

	var calls int32
	fetch := func(ctx context.Context) ([]*genai.Model, error) {
		atomic.AddInt32(&calls, 1)
		return []*genai.Model{{Name: "publishers/google/models/gemini-test"}}, nil
	}
	onNoop := func() {}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := listModelsCachedWith(ctx, fetch, onNoop, onNoop); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Errorf("fetch calls = %d, want 5 (cache disabled)", got)
	}
}

func TestListModelsCachedErrorNotCached(t *testing.T) {
	t.Setenv(envTagsCacheTTL, "60")
	InvalidateTagsCache()
	t.Cleanup(InvalidateTagsCache)

	upstreamErr := errors.New("upstream down")
	var calls int32
	fetch := func(ctx context.Context) ([]*genai.Model, error) {
		atomic.AddInt32(&calls, 1)
		return nil, upstreamErr
	}
	onNoop := func() {}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_, err := listModelsCachedWith(ctx, fetch, onNoop, onNoop)
		if !errors.Is(err, upstreamErr) {
			t.Fatalf("call %d: err = %v, want %v", i, err, upstreamErr)
		}
	}

	// All three calls should have hit the upstream — failures must never be
	// cached (otherwise a transient outage would lock /api/tags into 5xx
	// for the full TTL).
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("fetch calls = %d, want 3 (errors should bypass cache)", got)
	}
}

func TestListModelsCachedExpiry(t *testing.T) {
	// 1-second TTL keeps the test fast while still exercising the
	// time.Since() expiry branch.
	t.Setenv(envTagsCacheTTL, "1")
	InvalidateTagsCache()
	t.Cleanup(InvalidateTagsCache)

	var calls int32
	fetch := func(ctx context.Context) ([]*genai.Model, error) {
		atomic.AddInt32(&calls, 1)
		return []*genai.Model{{Name: "publishers/google/models/gemini-test"}}, nil
	}
	onNoop := func() {}

	ctx := context.Background()
	if _, err := listModelsCachedWith(ctx, fetch, onNoop, onNoop); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Within TTL: served from cache.
	if _, err := listModelsCachedWith(ctx, fetch, onNoop, onNoop); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("fetch calls before expiry = %d, want 1", got)
	}

	// Wait past TTL and verify the next call refetches.
	time.Sleep(1100 * time.Millisecond)
	if _, err := listModelsCachedWith(ctx, fetch, onNoop, onNoop); err != nil {
		t.Fatalf("post-expiry call: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("fetch calls after expiry = %d, want 2", got)
	}
}

func TestSetTagsCacheMetricsNilSafe(t *testing.T) {
	// nils should be ignored so partial installation never zeroes hooks.
	SetTagsCacheMetrics(nil, nil)
	// Then a real one should work.
	var calls int32
	SetTagsCacheMetrics(
		func() { atomic.AddInt32(&calls, 1) },
		func() { atomic.AddInt32(&calls, 10) },
	)
	cacheHitCallback()
	cacheMissCallback()
	if got := atomic.LoadInt32(&calls); got != 11 {
		t.Errorf("callback calls = %d, want 11", got)
	}
}

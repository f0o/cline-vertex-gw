package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ModelPrice is a duplicate type definition used in cache tests to keep the package decoupled.
type ModelPrice struct {
	InputPerM  float64 `json:"input_per_m"`
	OutputPerM float64 `json:"output_per_m"`
	Source     string  `json:"source"`
}

// payload is a small serializable type used across the fs cache tests.
type fsTestPayload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// writeRawFile writes bytes directly to path (used to seed a corrupt file).
func writeRawFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o644)
}

// backdateFSCache rewrites the on-disk envelope at path so its written_at
// timestamp is `age` in the past, letting tests deterministically age a cache
// file past its TTL without sleeping.
func backdateFSCache(t *testing.T, path string, age time.Duration) {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("backdate read: %v", err)
	}
	var env fsCacheEnvelope
	if err := json.Unmarshal(blob, &env); err != nil {
		t.Fatalf("backdate unmarshal: %v", err)
	}
	env.WrittenAt = time.Now().Add(-age)
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("backdate marshal: %v", err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("backdate write: %v", err)
	}
}

// TestFSCacheRoundTrip verifies that a payload written to the cache can be read
// back intact and is reported fresh within the TTL window.
func TestFSCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envCacheDir, dir)

	want := fsTestPayload{Name: "gemini", Count: 7}
	if err := WriteFSCache("roundtrip.json", want); err != nil {
		t.Fatalf("writeFSCache: %v", err)
	}

	var got fsTestPayload
	fresh, err := ReadFSCache("roundtrip.json", &got)
	if err != nil {
		t.Fatalf("readFSCache: %v", err)
	}
	if !fresh {
		t.Errorf("expected fresh=true for a just-written file")
	}
	if got != want {
		t.Errorf("round-trip mismatch: got %+v want %+v", got, want)
	}
}

// TestFSCacheMissingFile verifies a missing file is reported as a non-fresh
// miss with no error so callers fall through to a live refresh.
func TestFSCacheMissingFile(t *testing.T) {
	t.Setenv(envCacheDir, t.TempDir())

	var got fsTestPayload
	fresh, err := ReadFSCache("does-not-exist.json", &got)
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if fresh {
		t.Errorf("expected fresh=false for a missing file")
	}
}

// TestFSCacheStale verifies that a file older than the TTL is reported as stale
// (fresh=false) while still unmarshaling the payload (so callers can use it as
// a fallback after a failed refresh).
func TestFSCacheStale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envCacheDir, dir)
	// 1-second TTL so we can age past it quickly by backdating the envelope.
	t.Setenv(envFSCacheTTL, "1")

	want := fsTestPayload{Name: "claude", Count: 3}
	if err := WriteFSCache("stale.json", want); err != nil {
		t.Fatalf("writeFSCache: %v", err)
	}

	// Backdate the on-disk envelope so it is well past the 1s TTL.
	backdateFSCache(t, filepath.Join(dir, "stale.json"), 2*time.Hour)

	var got fsTestPayload
	fresh, err := ReadFSCache("stale.json", &got)
	if err != nil {
		t.Fatalf("readFSCache: %v", err)
	}
	if fresh {
		t.Errorf("expected fresh=false for an aged-out file")
	}
	if got != want {
		t.Errorf("stale payload should still unmarshal: got %+v want %+v", got, want)
	}
}

// TestFSCacheZeroTTLAlwaysStale verifies a 0 TTL forces every read to be
// treated as stale (always-refresh mode).
func TestFSCacheZeroTTLAlwaysStale(t *testing.T) {
	t.Setenv(envCacheDir, t.TempDir())
	t.Setenv(envFSCacheTTL, "0")

	if err := WriteFSCache("zero.json", fsTestPayload{Name: "x", Count: 1}); err != nil {
		t.Fatalf("writeFSCache: %v", err)
	}
	var got fsTestPayload
	fresh, err := ReadFSCache("zero.json", &got)
	if err != nil {
		t.Fatalf("readFSCache: %v", err)
	}
	if fresh {
		t.Errorf("expected fresh=false when TTL=0")
	}
}

// TestFSCacheCorruptFile verifies a corrupt/partial file is treated as a miss
// rather than a fatal error.
func TestFSCacheCorruptFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envCacheDir, dir)

	if err := writeRawFile(filepath.Join(dir, "corrupt.json"), []byte("{not valid json")); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	var got fsTestPayload
	fresh, err := ReadFSCache("corrupt.json", &got)
	if err != nil {
		t.Fatalf("expected no error for corrupt file, got %v", err)
	}
	if fresh {
		t.Errorf("expected fresh=false for a corrupt file")
	}
}

// TestFSCachePricingTableRoundTrip verifies the billing/pricing table type
// survives a cache round-trip (the real payload used by WarmPricing).
func TestFSCachePricingTableRoundTrip(t *testing.T) {
	t.Setenv(envCacheDir, t.TempDir())

	want := map[string]ModelPrice{
		"geminipro": {InputPerM: 1.25, OutputPerM: 5.0, Source: "Vertex AI"},
	}
	if err := WriteFSCache(PricingCacheFile, want); err != nil {
		t.Fatalf("writeFSCache: %v", err)
	}
	var got map[string]ModelPrice
	fresh, err := ReadFSCache(PricingCacheFile, &got)
	if err != nil {
		t.Fatalf("readFSCache: %v", err)
	}
	if !fresh {
		t.Errorf("expected fresh=true")
	}
	if got["geminipro"] != want["geminipro"] {
		t.Errorf("pricing round-trip mismatch: got %+v want %+v", got, want)
	}
}

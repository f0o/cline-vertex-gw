package cache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.f0o.dev/cline-vertex-gw/pkg/logx"
)

var logCache = logx.Scoped("cache")

// Filesystem cache for Models and Billing information.
//
// Why: the model catalog and the billing/pricing table both change on the order
// of days-to-weeks, yet rebuilding either at startup costs several seconds of
// fan-out Vertex / Cloud Billing API calls. Persisting the last good result to
// disk lets a freshly (re)started process serve cached data immediately and
// only pay the live-refresh cost when the on-disk copy is missing or stale.
//
// The cache is intentionally simple: one JSON file per dataset, each wrapping
// the payload in an envelope that records when it was written. "Lazy refresh on
// startup" means: at boot we read the file; if it is absent or older than the
// TTL (default 1 day) we refresh live and rewrite it, otherwise we load it as
// is. There are no background goroutines and no locks beyond the single-process
// write — a corrupt or partial file is treated as a miss (correctness by
// omission), never a fatal error.

const (
	// envCacheDir overrides the directory used for on-disk cache files.
	envCacheDir = "GW_CACHE_DIR"
	// envFSCacheTTL overrides the freshness window (seconds) for on-disk
	// cache files. Default is 1 day. A 0-or-negative value forces every
	// startup load to be treated as stale (always refresh).
	envFSCacheTTL = "GW_FS_CACHE_TTL_SEC"

	defaultFSCacheTTL = 24 * time.Hour

	// File names for the two datasets we persist.
	ModelsCacheFile  = "models.json"
	PricingCacheFile = "pricing.json"
)

// fsCacheEnvelope wraps a cached payload with the timestamp it was written so
// staleness can be evaluated on the next startup. Data is stored as raw JSON so
// the envelope is reusable for any payload type.
type fsCacheEnvelope struct {
	WrittenAt time.Time       `json:"written_at"`
	Data      json.RawMessage `json:"data"`
}

// envDurationSeconds reads an env var holding a count of seconds and returns it
// as a time.Duration. Returns def when the var is unset, empty, non-numeric, or
// negative. Read on each call (not cached) so tests can override via t.Setenv.
func envDurationSeconds(name string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return def
	}
	return time.Duration(n) * time.Second
}

// fsCacheTTL reads the configured freshness window each call so tests can
// override via t.Setenv.
func fsCacheTTL() time.Duration {
	return envDurationSeconds(envFSCacheTTL, defaultFSCacheTTL)
}

// cacheDir resolves the directory used for cache files. It prefers GW_CACHE_DIR;
// otherwise it falls back to the OS user cache dir under a per-app subfolder,
// and finally to a temp-dir subfolder if even that is unavailable. The returned
// directory is created if it does not exist.
func cacheDir() (string, error) {
	dir := os.Getenv(envCacheDir)
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			base = os.TempDir()
		}
		dir = filepath.Join(base, "cline-vertex-gw")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir %q: %w", dir, err)
	}
	return dir, nil
}

// WriteFSCache marshals payload into a timestamped envelope and writes it
// atomically (temp file + rename) to name within the cache directory.
func WriteFSCache(name string, payload any) error {
	dir, err := cacheDir()
	if err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	env := fsCacheEnvelope{WrittenAt: time.Now(), Data: data}
	blob, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}

	dst := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// ReadFSCache loads name from the cache directory, unmarshals the payload into
// out, and reports whether the on-disk copy is still fresh (younger than the
// configured TTL). A missing file, unreadable file, parse error, or expired
// timestamp all return fresh=false with no error so callers treat it as a
// cache miss and refresh live. A genuine I/O surprise is returned as err.
func ReadFSCache(name string, out any) (fresh bool, err error) {
	dir, derr := cacheDir()
	if derr != nil {
		return false, derr
	}
	blob, rerr := os.ReadFile(filepath.Join(dir, name))
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return false, nil
		}
		return false, fmt.Errorf("read cache file: %w", rerr)
	}
	var env fsCacheEnvelope
	if err := json.Unmarshal(blob, &env); err != nil {
		// Corrupt/partial file: treat as a miss so the next refresh repairs it.
		return false, nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return false, nil
	}
	ttl := fsCacheTTL()
	fresh = ttl > 0 && !env.WrittenAt.IsZero() && time.Since(env.WrittenAt) < ttl
	return fresh, nil
}

// CleanupElidedFiles scans the cache directory and deletes any files starting with "elided_"
// and ending with ".json" that have a modification time older than the specified TTL.
// It returns the number of deleted files and any error encountered.
func CleanupElidedFiles(ttl time.Duration) (int, error) {
	dir, err := cacheDir()
	if err != nil {
		return 0, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("read cache dir: %w", err)
	}

	now := time.Now()
	deletedCount := 0

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "elided_") && strings.HasSuffix(name, ".json") {
			info, err := entry.Info()
			if err != nil {
				logCache.Warnf("failed to get info for cached file %s: %v", name, err)
				continue
			}

			if now.Sub(info.ModTime()) > ttl {
				path := filepath.Join(dir, name)
				if err := os.Remove(path); err != nil {
					logCache.Warnf("failed to delete stale cached file %s: %v", name, err)
					continue
				}
				deletedCount++
			}
		}
	}

	return deletedCount, nil
}

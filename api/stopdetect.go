package api

import (
	"context"
	"hash/fnv"
	"os"
	"strconv"
	"strings"
)

// Loop / runaway-output detection knobs. The motivating case is a model that
// gets stuck repeating the same sentence or paragraph indefinitely; without
// intervention the gateway happily streams (and bills) until the upstream's
// max-output-tokens or the request context's deadline is reached.
//
//   - GW_LOOP_DETECTOR (default: on)
//     Master switch. Set to "0"/"false"/"off" to disable detection entirely.
//
//   - GW_LOOP_DETECT_WINDOW (default: 512)
//     Size, in characters, of the rolling buffer the detector inspects.
//     We hash overlapping fixed-size chunks within this window and stop when
//     the same chunk hash repeats more than GW_LOOP_DETECT_THRESHOLD times.
//
//   - GW_LOOP_DETECT_CHUNK (default: 64)
//     Size of each hashed chunk. Smaller catches short loops earlier but
//     risks false positives on legitimately repetitive prose (e.g. tables).
//
//   - GW_LOOP_DETECT_THRESHOLD (default: 6)
//     Number of identical chunk hashes within the window that triggers a
//     loop signal. 6 means ~384 chars of identical repetition (6 * 64).
//
// All knobs are read once at startup; toggle via env then restart.
var (
	loopDetectorEnabled  = envBoolAPI("GW_LOOP_DETECTOR", true)
	loopDetectWindow     = envIntAPI("GW_LOOP_DETECT_WINDOW", 512)
	loopDetectChunk      = envIntAPI("GW_LOOP_DETECT_CHUNK", 64)
	loopDetectThreshold  = envIntAPI("GW_LOOP_DETECT_THRESHOLD", 6)
)

// envBoolAPI is a thin local copy of provider.envBool — we don't want a
// cross-package dep cycle just for a 10-line helper. Accepts the usual
// 1/0/true/false/on/off/yes/no spellings (case-insensitive).
func envBoolAPI(name string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	switch v {
	case "":
		return def
	case "1", "t", "true", "on", "y", "yes":
		return true
	case "0", "f", "false", "off", "n", "no":
		return false
	default:
		return def
	}
}

// envIntAPI parses a positive int env var with a default. Returns the
// default on any error or non-positive value.
func envIntAPI(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// LoopDetector watches a stream of text chunks for runaway repetition. The
// implementation is deliberately simple: we maintain a fixed-size sliding
// byte window of the most recent output and count how often any chunk-sized
// substring's FNV-1a hash appears within that window. When a hash crosses
// the configured threshold we declare a loop.
//
// LoopDetector is NOT thread-safe; one instance per upstream stream.
type LoopDetector struct {
	enabled    bool
	window     int // characters of history to retain
	chunkSize  int // size of each hashed substring
	threshold  int // hash count that signals a loop

	buf []byte
}

// NewLoopDetector returns a detector configured from the package-level env
// knobs. Pass the result to Observe(chunk) after every text fragment; check
// LoopDetected() once per chunk to decide whether to cancel the stream.
func NewLoopDetector() *LoopDetector {
	d := &LoopDetector{
		enabled:   loopDetectorEnabled,
		window:    loopDetectWindow,
		chunkSize: loopDetectChunk,
		threshold: loopDetectThreshold,
	}
	if d.window < d.chunkSize*2 {
		// A meaningful detection window must hold at least two chunks; if the
		// operator configures something silly, fall back to defaults.
		d.window = 512
		d.chunkSize = 64
	}
	d.buf = make([]byte, 0, d.window+d.chunkSize)
	return d
}

// Observe appends a text fragment to the rolling buffer, trimming the front
// to keep at most `window` bytes retained. Safe to call with empty strings.
func (d *LoopDetector) Observe(text string) {
	if d == nil || !d.enabled || text == "" {
		return
	}
	d.buf = append(d.buf, text...)
	if len(d.buf) > d.window {
		// Drop the oldest bytes; keep only the last `window` bytes.
		d.buf = d.buf[len(d.buf)-d.window:]
	}
}

// LoopDetected returns true if the rolling buffer contains a chunk-sized
// substring whose FNV-1a hash has appeared more than `threshold` times.
// Returns false if detection is disabled or there isn't enough text yet.
func (d *LoopDetector) LoopDetected() bool {
	if d == nil || !d.enabled {
		return false
	}
	if len(d.buf) < d.chunkSize*d.threshold {
		return false
	}
	counts := make(map[uint64]int, len(d.buf))
	for i := 0; i+d.chunkSize <= len(d.buf); i++ {
		h := fnv.New64a()
		_, _ = h.Write(d.buf[i : i+d.chunkSize])
		k := h.Sum64()
		counts[k]++
		if counts[k] > d.threshold {
			return true
		}
	}
	return false
}

// WatchAndCancel wires the detector to a context cancel function: each call
// to Observe checks for a loop and invokes cancel() at most once on the
// first detection. This is the convenience entry point the stream handlers
// use.
//
// Returns a callback compatible with streamCallback signature semantics —
// receiving a chunk, advancing detector state, and returning a non-nil
// error if a loop was detected (which the stream loop treats as a graceful
// terminator).
func (d *LoopDetector) WatchAndCancel(ctx context.Context, cancel context.CancelFunc) func(string) bool {
	fired := false
	return func(text string) bool {
		if d == nil || !d.enabled || fired {
			return false
		}
		d.Observe(text)
		if d.LoopDetected() {
			fired = true
			cancel()
			return true
		}
		return false
	}
}
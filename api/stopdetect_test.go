package api

import (
	"context"
	"strings"
	"testing"
)

// withLoopKnobs temporarily overrides the package-level loop-detector knobs
// and returns a restore func. Tests must `defer restore()` to keep state
// isolated.
func withLoopKnobs(t *testing.T, enabled bool, window, chunk, threshold int) func() {
	t.Helper()
	prevE, prevW, prevC, prevT := loopDetectorEnabled, loopDetectWindow, loopDetectChunk, loopDetectThreshold
	loopDetectorEnabled = enabled
	loopDetectWindow = window
	loopDetectChunk = chunk
	loopDetectThreshold = threshold
	return func() {
		loopDetectorEnabled = prevE
		loopDetectWindow = prevW
		loopDetectChunk = prevC
		loopDetectThreshold = prevT
	}
}

// TestLoopDetector_Disabled verifies the master switch: with detection off
// no amount of repetition should fire the signal. This is the safety hatch
// operators reach for when a false positive bites a legitimate workload.
func TestLoopDetector_Disabled(t *testing.T) {
	defer withLoopKnobs(t, false, 512, 64, 6)()

	d := NewLoopDetector()
	for i := 0; i < 100; i++ {
		d.Observe(strings.Repeat("loop", 1000))
	}
	if d.LoopDetected() {
		t.Error("LoopDetected = true with detector disabled; want false")
	}
}

// TestLoopDetector_NoRepetition verifies that varied output never trips the
// detector. Cline often emits long, non-repeating prose; a false positive
// here would prematurely truncate legitimate generations.
func TestLoopDetector_NoRepetition(t *testing.T) {
	defer withLoopKnobs(t, true, 512, 64, 6)()

	d := NewLoopDetector()
	// Produce diverse content well over the window size.
	for i := 0; i < 50; i++ {
		d.Observe(strings.Repeat(string(rune('a'+i%26)), 32))
	}
	if d.LoopDetected() {
		t.Error("LoopDetected = true on varied content; want false")
	}
}

// TestLoopDetector_RepeatedFiresAtThreshold verifies the positive case: when
// the same chunk-sized substring repeats more than the threshold within the
// window, the detector fires.
func TestLoopDetector_RepeatedFiresAtThreshold(t *testing.T) {
	defer withLoopKnobs(t, true, 1024, 16, 4)()

	d := NewLoopDetector()
	// Repeat a 16-char block 10 times — well above the threshold of 4.
	for i := 0; i < 10; i++ {
		d.Observe("ABCDEFGHIJKLMNOP") // exactly 16 chars
	}
	if !d.LoopDetected() {
		t.Errorf("LoopDetected = false; want true after %d repetitions of a 16-char chunk", 10)
	}
}

// TestLoopDetector_BelowThresholdNoFire verifies that legitimate short-form
// repetition (e.g. a list with 3 identical bullets) doesn't trip a detector
// configured with a higher threshold.
func TestLoopDetector_BelowThresholdNoFire(t *testing.T) {
	defer withLoopKnobs(t, true, 1024, 16, 6)()

	d := NewLoopDetector()
	// Only 3 repetitions — threshold is 6.
	for i := 0; i < 3; i++ {
		d.Observe("ABCDEFGHIJKLMNOP")
	}
	if d.LoopDetected() {
		t.Error("LoopDetected = true at 3 reps; threshold is 6 — want false")
	}
}

// TestLoopDetector_WindowSlides verifies that old repetitions falling out
// the back of the rolling window stop counting. This prevents historical
// patterns from triggering spuriously on later, unrelated output.
func TestLoopDetector_WindowSlides(t *testing.T) {
	defer withLoopKnobs(t, true, 200, 16, 4)()

	d := NewLoopDetector()
	// Fill the window with one pattern (would fire if it stayed).
	for i := 0; i < 10; i++ {
		d.Observe("OLDOLDOLDOLDOLDA") // 16 chars
	}
	// At this point the detector likely fires; verify, then flush with new
	// content and confirm it no longer fires.
	if !d.LoopDetected() {
		t.Fatalf("setup invariant: expected LoopDetected after initial reps")
	}
	// Push 500 chars of varied content (well over the 200-char window).
	for i := 0; i < 50; i++ {
		d.Observe(strings.Repeat(string(rune('a'+i%26)), 10))
	}
	if d.LoopDetected() {
		t.Error("LoopDetected = true after window slid past old pattern; want false")
	}
}

// TestLoopDetector_WatchAndCancel verifies the convenience wiring used by
// the stream handler: a single cancel call on first detection, and idempotency
// on subsequent ones (so we don't spam-cancel an already-cancelled context).
func TestLoopDetector_WatchAndCancel(t *testing.T) {
	defer withLoopKnobs(t, true, 1024, 16, 4)()

	d := NewLoopDetector()
	ctx, cancel := context.WithCancel(context.Background())
	watch := d.WatchAndCancel(ctx, cancel)

	// Feed harmless varied chunks first — context must NOT be cancelled.
	for i := 0; i < 3; i++ {
		if fired := watch(string(rune('a' + i))); fired {
			t.Fatalf("watch fired on varied input")
		}
	}
	if ctx.Err() != nil {
		t.Fatalf("ctx cancelled prematurely: %v", ctx.Err())
	}

	// Now feed enough repetition to trigger.
	var firedCount int
	for i := 0; i < 10; i++ {
		if watch("ABCDEFGHIJKLMNOP") {
			firedCount++
		}
	}
	if firedCount != 1 {
		t.Errorf("watch fired %d times; want exactly 1 (idempotent on retrigger)", firedCount)
	}
	if ctx.Err() == nil {
		t.Error("ctx not cancelled after loop detection; want cancelled")
	}
}

// TestLoopDetector_NilSafe verifies the nil-receiver safety net. Code paths
// that conditionally allocate a detector (e.g. when the env knob is off)
// often pass nil; the public methods must accept that gracefully.
func TestLoopDetector_NilSafe(t *testing.T) {
	var d *LoopDetector
	// No panics.
	d.Observe("hello")
	if d.LoopDetected() {
		t.Error("nil-receiver LoopDetected returned true; want false")
	}
}

// TestLoopDetector_NotEnoughDataNoFire verifies that the detector doesn't
// spuriously fire when the buffer hasn't accumulated enough data yet to
// meaningfully compare. (chunk * threshold = minimum sensible sample size.)
func TestLoopDetector_NotEnoughDataNoFire(t *testing.T) {
	defer withLoopKnobs(t, true, 1024, 16, 4)()

	d := NewLoopDetector()
	// Only feed (chunk * threshold) - 1 = 63 chars; below the floor.
	d.Observe(strings.Repeat("A", 63))
	if d.LoopDetected() {
		t.Error("LoopDetected = true on too-small buffer; want false")
	}
}

// TestEnvBoolAPI exercises the package-local boolean env-var parser.
func TestEnvBoolAPI(t *testing.T) {
	const key = "GW_TEST_ENVBOOLAPI"
	cases := []struct {
		v    string
		def  bool
		want bool
	}{
		{"", true, true},
		{"", false, false},
		{"on", false, true},
		{"OFF", true, false},
		{"1", false, true},
		{"0", true, false},
		{"???", true, true},   // garbage → default
		{"???", false, false}, // garbage → default
	}
	for _, c := range cases {
		t.Setenv(key, c.v)
		if got := envBoolAPI(key, c.def); got != c.want {
			t.Errorf("envBoolAPI(%q, %v) = %v; want %v", c.v, c.def, got, c.want)
		}
	}
}

// TestEnvIntAPI exercises the package-local int env-var parser, including
// the "non-positive → default" behavior which protects against zero values
// silently disabling counters.
func TestEnvIntAPI(t *testing.T) {
	const key = "GW_TEST_ENVINTAPI"
	cases := []struct {
		v    string
		def  int
		want int
	}{
		{"", 42, 42},
		{"100", 42, 100},
		{"0", 42, 42},   // non-positive → default
		{"-5", 42, 42},  // negative → default
		{"abc", 42, 42}, // garbage → default
	}
	for _, c := range cases {
		t.Setenv(key, c.v)
		if got := envIntAPI(key, c.def); got != c.want {
			t.Errorf("envIntAPI(%q, %d) = %d; want %d", c.v, c.def, got, c.want)
		}
	}
}

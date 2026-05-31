package provider

import (
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"testing"
)

// withCaps temporarily overrides the package-level cap variables (which are
// normally set from env at startup) and returns a restore func. Tests must
// call `defer restore()` to keep state from leaking between cases.
func withCaps(t *testing.T, def, hard int32) func() {
	t.Helper()
	prevDef, prevHard := defaultMaxOutputTokens, hardMaxOutputTokens
	defaultMaxOutputTokens = def
	hardMaxOutputTokens = hard
	return func() {
		defaultMaxOutputTokens = prevDef
		hardMaxOutputTokens = prevHard
	}
}

func i32p(v int32) *int32 { return &v }

// TestApplyOutputCaps_NoKnobs verifies the fast path: with both env knobs
// at their default zero, the function MUST return its input unchanged (same
// pointer, no allocation), preserving the existing behavior for users who
// haven't configured the caps.
func TestApplyOutputCaps_NoKnobs(t *testing.T) {
	defer withCaps(t, 0, 0)()

	// nil opts → nil out.
	if got := ApplyOutputCaps(nil); got != nil {
		t.Errorf("ApplyOutputCaps(nil) = %+v; want nil", got)
	}

	// Non-nil opts: same pointer back (no copy).
	in := &pipeline.GenerationOptions{MaxTokens: i32p(99999)}
	if got := ApplyOutputCaps(in); got != in {
		t.Errorf("ApplyOutputCaps returned a new pointer when knobs disabled; want same pointer")
	}
}

// TestApplyOutputCaps_HardClampDown verifies the hard cap clamps an
// over-large caller value DOWN to the configured ceiling.
func TestApplyOutputCaps_HardClampDown(t *testing.T) {
	defer withCaps(t, 0, 1000)()

	in := &pipeline.GenerationOptions{MaxTokens: i32p(5000)}
	out := ApplyOutputCaps(in)
	if out == nil || out.MaxTokens == nil {
		t.Fatalf("got nil result; want clamped output")
	}
	if *out.MaxTokens != 1000 {
		t.Errorf("MaxTokens = %d; want 1000 (hard cap)", *out.MaxTokens)
	}
	// Caller's struct must not be mutated.
	if *in.MaxTokens != 5000 {
		t.Errorf("input MaxTokens mutated to %d; want preserved at 5000", *in.MaxTokens)
	}
}

// TestApplyOutputCaps_HardCapAppliedWhenUnset verifies that an unset
// MaxTokens (nil pointer) still gets clamped to the hard cap, so a runaway
// completion can't slip through by simply omitting the field.
func TestApplyOutputCaps_HardCapAppliedWhenUnset(t *testing.T) {
	defer withCaps(t, 0, 1000)()

	in := &pipeline.GenerationOptions{} // MaxTokens nil
	out := ApplyOutputCaps(in)
	if out == nil || out.MaxTokens == nil {
		t.Fatalf("got nil result; want hard-cap applied")
	}
	if *out.MaxTokens != 1000 {
		t.Errorf("MaxTokens = %d; want 1000", *out.MaxTokens)
	}
}

// TestApplyOutputCaps_HardCapDoesNotRaiseSmallerValue verifies that the
// hard cap is a CEILING, not a floor: a smaller caller-supplied value stays.
func TestApplyOutputCaps_HardCapDoesNotRaiseSmallerValue(t *testing.T) {
	defer withCaps(t, 0, 1000)()

	in := &pipeline.GenerationOptions{MaxTokens: i32p(200)}
	out := ApplyOutputCaps(in)
	if out == nil || out.MaxTokens == nil {
		t.Fatalf("got nil result; want passthrough")
	}
	if *out.MaxTokens != 200 {
		t.Errorf("MaxTokens = %d; want 200 (unchanged, below hard cap)", *out.MaxTokens)
	}
}

// TestApplyOutputCaps_DefaultFillsUnset verifies that GW_DEFAULT_MAX_OUTPUT_TOKENS
// fills in a value when the caller left MaxTokens unset.
func TestApplyOutputCaps_DefaultFillsUnset(t *testing.T) {
	defer withCaps(t, 800, 0)()

	in := &pipeline.GenerationOptions{} // MaxTokens nil
	out := ApplyOutputCaps(in)
	if out == nil || out.MaxTokens == nil {
		t.Fatalf("got nil result; want default applied")
	}
	if *out.MaxTokens != 800 {
		t.Errorf("MaxTokens = %d; want 800 (default)", *out.MaxTokens)
	}
}

// TestApplyOutputCaps_DefaultDoesNotOverrideExplicit verifies that the
// default is only used as a fallback — an explicit caller value (even large)
// is preserved when only the default is set (no hard cap).
func TestApplyOutputCaps_DefaultDoesNotOverrideExplicit(t *testing.T) {
	defer withCaps(t, 800, 0)()

	in := &pipeline.GenerationOptions{MaxTokens: i32p(5000)}
	out := ApplyOutputCaps(in)
	if out != in {
		// Default-only path with explicit value → no change, return same pointer.
		t.Errorf("ApplyOutputCaps returned a new pointer; want same pointer (default-only, explicit value)")
	}
	if out.MaxTokens == nil || *out.MaxTokens != 5000 {
		t.Errorf("MaxTokens = %v; want 5000 (preserved)", out.MaxTokens)
	}
}

// TestApplyOutputCaps_HardAndDefault verifies the combined behavior: hard
// cap takes precedence whenever the caller's value (or the default) would
// exceed it; default is used only when both knobs apply.
func TestApplyOutputCaps_HardAndDefault(t *testing.T) {
	defer withCaps(t, 800, 1000)()

	cases := []struct {
		name string
		in   *pipeline.GenerationOptions
		want int32
	}{
		{"unset → default", &pipeline.GenerationOptions{}, 800},
		{"under hard, kept", &pipeline.GenerationOptions{MaxTokens: i32p(500)}, 500},
		{"above hard, clamped", &pipeline.GenerationOptions{MaxTokens: i32p(9999)}, 1000},
		{"equal to hard, kept", &pipeline.GenerationOptions{MaxTokens: i32p(1000)}, 1000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := ApplyOutputCaps(c.in)
			if out == nil || out.MaxTokens == nil {
				t.Fatalf("got nil result")
			}
			if *out.MaxTokens != c.want {
				t.Errorf("MaxTokens = %d; want %d", *out.MaxTokens, c.want)
			}
		})
	}
}

// TestApplyOutputCaps_PreservesOtherFields verifies that the clamp logic
// copies all other pipeline.GenerationOptions fields when it needs to allocate a new
// struct (we never want to lose Temperature/TopP/etc.).
func TestApplyOutputCaps_PreservesOtherFields(t *testing.T) {
	defer withCaps(t, 0, 1000)()

	var temp float32 = 0.42
	var topP float32 = 0.9
	var topK int32 = 40
	in := &pipeline.GenerationOptions{
		Temperature: &temp,
		TopP:        &topP,
		TopK:        &topK,
		Stop:        []string{"</done>"},
		MaxTokens:   i32p(5000), // will be clamped
	}
	out := ApplyOutputCaps(in)
	if out == nil {
		t.Fatal("got nil; want clamped output")
	}
	if out.Temperature != &temp || out.TopP != &topP || out.TopK != &topK {
		t.Errorf("other fields lost: temp=%v topP=%v topK=%v",
			out.Temperature, out.TopP, out.TopK)
	}
	if len(out.Stop) != 1 || out.Stop[0] != "</done>" {
		t.Errorf("Stop lost: %v", out.Stop)
	}
	if out.MaxTokens == nil || *out.MaxTokens != 1000 {
		t.Errorf("MaxTokens = %v; want clamped 1000", out.MaxTokens)
	}
}

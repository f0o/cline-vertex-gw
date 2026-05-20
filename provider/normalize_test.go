package provider

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// withNormalize temporarily toggles the package-level switch for tests and
// returns a restore func — mirrors the pattern in budget_test.go.
func withNormalize(t *testing.T, enabled bool) func() {
	t.Helper()
	prev := normalizeWhitespace
	normalizeWhitespace = enabled
	return func() { normalizeWhitespace = prev }
}

// TestNormalizeText_BOM checks that a leading UTF-8 BOM is stripped (the
// byte sequence U+FEFF would otherwise consume a token for no reason).
func TestNormalizeText_BOM(t *testing.T) {
	in := "\ufeffhello"
	got := normalizeText(in)
	if got != "hello" {
		t.Errorf("normalizeText(BOM+hello) = %q; want %q", got, "hello")
	}
}

// TestNormalizeText_CRLF verifies CRLF and bare CR both collapse to LF —
// avoids redundant tokens on Windows-origin files pasted by Cline.
func TestNormalizeText_CRLF(t *testing.T) {
	in := "a\r\nb\rc\n"
	got := normalizeText(in)
	want := "a\nb\nc" // trailing blank line also stripped
	if got != want {
		t.Errorf("normalizeText(CR mix) = %q; want %q", got, want)
	}
}

// TestNormalizeText_TrailingSpaces verifies trailing horizontal whitespace
// is removed but inner whitespace (indentation, intra-line spacing) is
// preserved. This is critical for code blocks.
func TestNormalizeText_TrailingSpaces(t *testing.T) {
	in := "    func foo() {   \n        return 1\t\n    }   \n"
	got := normalizeText(in)
	want := "    func foo() {\n        return 1\n    }"
	if got != want {
		t.Errorf("normalizeText(trail+indent) = %q; want %q", got, want)
	}
}

// TestNormalizeText_CollapseBlankRuns verifies runs of 3+ blank lines are
// capped at 2 (which renders as a single blank separator).
func TestNormalizeText_CollapseBlankRuns(t *testing.T) {
	in := "para1\n\n\n\n\npara2"
	got := normalizeText(in)
	want := "para1\n\n\npara2" // 2 blank lines retained → 3 \n
	if got != want {
		t.Errorf("normalizeText(blank run) = %q; want %q", got, want)
	}
}

// TestNormalizeText_Idempotent verifies running the transform twice yields
// the same result — a stability check.
func TestNormalizeText_Idempotent(t *testing.T) {
	in := "  hi  \r\nthere\t\n\n\n\nend\n"
	once := normalizeText(in)
	twice := normalizeText(once)
	if once != twice {
		t.Errorf("not idempotent: first=%q second=%q", once, twice)
	}
}

// TestNormalizeText_AllBlankInput verifies all-blank input collapses to "".
// Without the tail-trim, we'd leak an empty-string element.
func TestNormalizeText_AllBlankInput(t *testing.T) {
	in := "\n\n\n   \n\t\n"
	got := normalizeText(in)
	if got != "" {
		t.Errorf("normalizeText(all blanks) = %q; want %q", got, "")
	}
}

// TestNormalizeText_PreservesNonAsciiWhitespace verifies a non-breaking
// space (U+00A0) inside a line is NOT removed. We only strip ASCII space
// and tab; deviating would break some prose deliberately formatted with
// NBSPs (rare but possible).
func TestNormalizeText_PreservesNonAsciiWhitespace(t *testing.T) {
	in := "hello\u00a0world\n"
	got := normalizeText(in)
	want := "hello\u00a0world"
	if got != want {
		t.Errorf("normalizeText(nbsp) = %q; want %q", got, want)
	}
}

// TestNormalizeWhitespace_DisabledFastPath verifies the zero-knob fast
// path returns the input slice header unchanged.
func TestNormalizeWhitespace_DisabledFastPath(t *testing.T) {
	defer withNormalize(t, false)()

	in := []*genai.Content{
		mkTurn(genai.RoleUser, "hello   \r\nworld\n\n\n\n"),
	}
	out := NormalizeWhitespace(in)
	if &out[0] != &in[0] {
		// Slice-header identity isn't strictly required, but the function
		// returns `in` directly so this is a useful invariant check.
		// (If we ever change the API to always copy, this test needs to
		// switch to comparing Part.Text byte-for-byte.)
		t.Error("expected fast-path return of input slice when disabled")
	}
}

// TestNormalizeWhitespace_EnabledTransforms verifies the end-to-end happy
// path: dirty input goes in, normalized text comes out, original is
// untouched (no in-place mutation).
func TestNormalizeWhitespace_EnabledTransforms(t *testing.T) {
	defer withNormalize(t, true)()

	original := "tool output   \r\n\r\n\r\n\r\nline2  \n"
	in := []*genai.Content{mkTurn(genai.RoleUser, original)}
	out := NormalizeWhitespace(in)

	if len(out) != 1 || len(out[0].Parts) != 1 {
		t.Fatalf("unexpected shape: %+v", out)
	}
	got := out[0].Parts[0].Text
	want := "tool output\n\n\nline2" // collapsed blanks; trailing trimmed
	if got != want {
		t.Errorf("normalized text = %q; want %q", got, want)
	}
	// Original must be untouched (callers may share / retry).
	if in[0].Parts[0].Text != original {
		t.Errorf("input mutated; was %q now %q", original, in[0].Parts[0].Text)
	}
}

// TestNormalizeWhitespace_NilSafe verifies the function tolerates nil
// entries and nil parts without panicking. The dispatch layer is the only
// caller today but defensive nil handling cheaply prevents future
// regressions.
func TestNormalizeWhitespace_NilSafe(t *testing.T) {
	defer withNormalize(t, true)()

	in := []*genai.Content{
		nil,
		{Role: genai.RoleUser, Parts: []*genai.Part{nil, {Text: "ok  "}, nil}},
	}
	out := NormalizeWhitespace(in)
	if len(out) != 2 {
		t.Fatalf("len(out)=%d; want 2", len(out))
	}
	if out[0] != nil {
		t.Errorf("nil entry not preserved")
	}
	if out[1].Parts[1].Text != "ok" {
		t.Errorf("text not normalized: got %q", out[1].Parts[1].Text)
	}
}

// TestNormalizeSystemPrompt_RespectsKnob verifies the system-prompt helper
// honors the knob and applies the same transform.
func TestNormalizeSystemPrompt_RespectsKnob(t *testing.T) {
	defer withNormalize(t, true)()

	in := "  You are an assistant.   \r\n\r\n\r\n\r\nBe brief.  \n"
	got := NormalizeSystemPrompt(in)
	want := "  You are an assistant.\n\n\nBe brief."
	if got != want {
		t.Errorf("NormalizeSystemPrompt = %q; want %q", got, want)
	}

	// Disabled: returns input unchanged.
	normalizeWhitespace = false
	if NormalizeSystemPrompt(in) != in {
		t.Errorf("disabled path should return input unchanged")
	}
}

// TestNormalizeText_NoOpFastPath verifies that already-clean text comes
// back unchanged (an important property — we don't want to penalize good
// inputs by forcing an allocation roundtrip if avoidable).
func TestNormalizeText_NoOpFastPath(t *testing.T) {
	in := "clean line one\nclean line two\nclean line three"
	got := normalizeText(in)
	if got != in {
		t.Errorf("clean text changed: was %q now %q", in, got)
	}
}

// TestEnvBool_KnownValues spot-checks a handful of accepted strings —
// guards against accidental regressions in the parsing table.
func TestEnvBool_KnownValues(t *testing.T) {
	cases := []struct {
		key  string
		val  string
		def  bool
		want bool
	}{
		{"GW_TEST_X1", "1", false, true},
		{"GW_TEST_X2", "true", false, true},
		{"GW_TEST_X3", "ON", false, true},
		{"GW_TEST_X4", "0", true, false},
		{"GW_TEST_X5", "OFF", true, false},
		{"GW_TEST_X6", "garbage", true, true},  // garbage -> default
		{"GW_TEST_X7", "garbage", false, false}, // garbage -> default
		{"GW_TEST_X8", "", true, true},          // unset -> default
	}
	for _, c := range cases {
		t.Run(c.val, func(t *testing.T) {
			if c.val != "" {
				t.Setenv(c.key, c.val)
			}
			got := envBool(c.key, c.def)
			if got != c.want {
				t.Errorf("envBool(%q=%q, def=%v) = %v; want %v",
					c.key, c.val, c.def, got, c.want)
			}
		})
	}
	_ = strings.Builder{} // keep strings import even if future edits trim usages
}
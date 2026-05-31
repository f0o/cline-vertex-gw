package pipeline

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

// TestPipeline_AllDisabledIsNoOp verifies that with every knob OFF, the
// pipeline returns the input contents and system prompt with no
// modifications. This is the safety-net case — operators who don't want any
// compression should be able to turn it ALL off.
func TestPipeline_AllDisabledIsNoOp(t *testing.T) {
	defer withNormalize(t, false)()
	defer withCollapseEnv(t, false, 256)()
	defer withMaxInputChars(t, 0)()
	defer withDedup(t, false, 512)()

	body := "  hello world  \r\n\r\n\r\n\r\nend  \n"
	in := []*genai.Content{
		mkTurn(genai.RoleUser, body),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, body),
	}
	sys := "  system   \r\n"

	out, gotSys := ApplyCompressionPipeline(in, sys, false, nil)
	if gotSys != sys {
		t.Errorf("system prompt mutated: was %q now %q", sys, gotSys)
	}
	if len(out) != len(in) {
		t.Errorf("len(out)=%d; want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].Parts[0].Text != in[i].Parts[0].Text {
			t.Errorf("turn %d mutated", i)
		}
	}
}

// TestPipeline_NormalizeRunsBeforeTrim verifies that whitespace normalization
// happens before byte-budget trimming, which means normalization can save a
// turn from being dropped. This is the load-bearing ordering decision in
// ApplyCompressionPipeline; if it ever silently flips, tests catch it.
func TestPipeline_NormalizeRunsBeforeTrim(t *testing.T) {
	defer withNormalize(t, true)()
	defer withCollapseEnv(t, false, 256)()
	// Budget = 200 bytes total. The first turn has 80 bytes of real
	// content + 200 bytes of trailing-blank-line/CRLF padding. After
	// normalization it shrinks to ~80B, which should let it survive the
	// trim. Without normalization it would be ~280B and get dropped.
	defer withMaxInputChars(t, 200)()
	defer withDedup(t, false, 512)()

	bloated := strings.Repeat("a", 80) + strings.Repeat("\r\n   \r\n", 30)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, bloated),
		mkTurn(genai.RoleUser, "tail"),
	}

	out, _ := ApplyCompressionPipeline(in, "", false, nil)
	if len(out) != 2 {
		t.Fatalf("expected normalization to save the bloated turn from trim; got len=%d", len(out))
	}
	if strings.Contains(out[0].Parts[0].Text, "\r\n") {
		t.Errorf("normalization didn't run: CRLF still present")
	}
}

// TestPipeline_EnvCollapseHelpsTrim verifies that env-block collapsing runs
// before byte-budget trimming, so collapsing big stale env_details payloads
// frees up budget for actual conversation content.
func TestPipeline_EnvCollapseHelpsTrim(t *testing.T) {
	defer withNormalize(t, false)()
	defer withCollapseEnv(t, true, 100)()
	// 800 byte budget. Turn 1 has 5kB of env block + 50 bytes of real
	// content. Without collapse, turn 1 would be too large and dropped.
	// With collapse it shrinks to ~150B and fits.
	defer withMaxInputChars(t, 800)()
	defer withDedup(t, false, 512)()

	bigEnv := makeEnvBlock(5000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, "real content "+bigEnv),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, "final"),
	}
	out, _ := ApplyCompressionPipeline(in, "", false, nil)
	if len(out) != 3 {
		t.Fatalf("expected env-collapse to let turn 1 survive trim; got len=%d", len(out))
	}
	if strings.Contains(out[0].Parts[0].Text, strings.Repeat("x", 5000)) {
		t.Errorf("env block was not collapsed in pipeline")
	}
	if !strings.Contains(out[0].Parts[0].Text, "real content") {
		t.Errorf("real-content prefix was lost: %q", out[0].Parts[0].Text)
	}
}

// TestPipeline_DedupAfterTrim verifies dedup operates on the post-trim
// window: if turn 1 gets dropped by the trim, turn 3's content (which is
// a duplicate of turn 1) is the FIRST occurrence in the trimmed window
// and should NOT be replaced with a placeholder pointing to a turn that
// no longer exists.
func TestPipeline_DedupAfterTrim(t *testing.T) {
	defer withNormalize(t, false)()
	defer withCollapseEnv(t, false, 256)()
	defer withMaxInputChars(t, 5000)()
	defer withDedup(t, true, 100)()

	big := strings.Repeat("D", 3000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big), // turn 1 - probably trimmed
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, big), // turn 3 - kept
	}
	out, _ := ApplyCompressionPipeline(in, "", false, nil)

	// Find the last user turn in the output and assert its body wasn't
	// replaced with a placeholder pointing at a turn that's no longer
	// in the window.
	last := out[len(out)-1].Parts[0].Text
	if strings.Contains(last, "identical content already shown") {
		t.Errorf("post-trim dedup pointed at a dropped turn: %q", last)
	}
}

// TestPipeline_DedupReplacesDuplicateInKeptWindow verifies that within the
// kept window after trim, duplicates ARE replaced.
func TestPipeline_DedupReplacesDuplicateInKeptWindow(t *testing.T) {
	defer withNormalize(t, false)()
	defer withCollapseEnv(t, false, 256)()
	defer withMaxInputChars(t, 0)() // no trim
	defer withDedup(t, true, 100)()

	big := strings.Repeat("E", 2000)
	in := []*genai.Content{
		mkTurn(genai.RoleUser, big),
		mkTurn(genai.RoleModel, "ack"),
		mkTurn(genai.RoleUser, big),
	}
	out, _ := ApplyCompressionPipeline(in, "", false, nil)
	if !strings.Contains(out[2].Parts[0].Text, "identical content already shown in turn 1") {
		t.Errorf("dedup didn't replace turn 3 dup: %q", out[2].Parts[0].Text)
	}
}

// TestPipeline_FullStackInteraction is the headline scenario: a realistic
// Cline-style conversation with bloated whitespace, stale env blocks, AND
// repeated identical file pastes. Whitespace normalization + env collapse
// + dedup should all contribute.
//
// Note on dedup behavior: the compressor matches WHOLE-PART hashes (not
// substring matches), so a re-pasted file wrapped in different surrounding
// text won't dedup against itself. To exercise dedup in this test the
// repeated file paste lives in its OWN part on each turn.
func TestPipeline_FullStackInteraction(t *testing.T) {
	defer withNormalize(t, true)()
	defer withCollapseEnv(t, true, 100)()
	defer withMaxInputChars(t, 50_000)() // generous so nothing trims
	defer withDedup(t, true, 100)()

	bigFile := strings.Repeat("function foo() { return 1; }\n", 100) // ~3KB
	envBlock := makeEnvBlock(3000)
	bloated := func(s string) string { return s + "   \r\n\r\n\r\n\r\n" }

	// Two parts per user turn: the wrapper (different each turn) and the
	// file paste (identical each turn → eligible for dedup).
	mkUserTurn := func(wrapper string) *genai.Content {
		return &genai.Content{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{Text: bloated(wrapper)},
				{Text: bigFile},
				{Text: envBlock},
			},
		}
	}

	in := []*genai.Content{
		mkUserTurn("read_file foo.js"),
		mkTurn(genai.RoleModel, bloated("Got it.")),
		mkUserTurn("read_file foo.js again"),
		mkTurn(genai.RoleModel, bloated("Still got it.")),
		mkUserTurn("Now apply the fix."),
	}
	originalTotal := 0
	for _, c := range in {
		originalTotal += contentBytes(c)
	}

	out, _ := ApplyCompressionPipeline(in, "", false, nil)

	compressedTotal := 0
	for _, c := range out {
		compressedTotal += contentBytes(c)
	}
	if compressedTotal >= originalTotal {
		t.Errorf("compression made no progress: original=%dB compressed=%dB",
			originalTotal, compressedTotal)
	}

	// Last turn's env block (3rd part) must be intact verbatim.
	lastEnvPart := out[len(out)-1].Parts[2].Text
	if lastEnvPart != envBlock {
		t.Errorf("last turn's env block was modified; len=%d want %d",
			len(lastEnvPart), len(envBlock))
	}
	// First-occurrence file paste in turn 1 should survive verbatim
	// (after whitespace normalization, which doesn't change this body).
	if !strings.Contains(out[0].Parts[1].Text, "function foo()") {
		t.Errorf("turn 1's file paste was lost")
	}
	// Turns 3 and 5's identical file paste should be a placeholder
	// pointing back at turn 1.
	for _, turnIdx := range []int{2, 4} {
		got := out[turnIdx].Parts[1].Text
		if strings.Contains(got, "function foo()") {
			t.Errorf("turn %d's duplicate file paste was not deduped: %q",
				turnIdx+1, got)
		}
		if !strings.Contains(got, "identical content already shown in turn 1") {
			t.Errorf("turn %d's dedup placeholder doesn't point at turn 1: %q",
				turnIdx+1, got)
		}
	}
	// CRLF should be gone everywhere (normalization ran).
	for i, c := range out {
		for _, p := range c.Parts {
			if strings.Contains(p.Text, "\r\n") {
				t.Errorf("turn %d still has CRLF after pipeline", i+1)
			}
		}
	}

	t.Logf("compression: %dB -> %dB (%.0f%% reduction)",
		originalTotal, compressedTotal,
		100.0*(1.0-float64(compressedTotal)/float64(originalTotal)))
}

// TestPipeline_CompressionMetrics verifies that our registered callback
// gets invoked with correct stage names and positive bytes-saved values.
func TestPipeline_CompressionMetrics(t *testing.T) {
	defer withNormalize(t, false)()
	defer withCollapseEnv(t, true, 100)()
	defer withMaxInputChars(t, 0)()
	defer withDedup(t, true, 100)()

	// Track callback invocations.
	called := make(map[string]int)
	SetCompressionMetrics(func(stage string, bytes int) {
		called[stage] = bytes
	})
	defer func() {
		onCompressionSaved = func(stage string, bytes int) {}
	}()

	envBlock := makeEnvBlock(3000)
	big := strings.Repeat("E", 2000)
	in := []*genai.Content{
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{Text: "first user turn " + envBlock},
				{Text: big},
			},
		},
		{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: "ack"}},
		},
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{Text: "second user turn " + envBlock}, // envBlock here will be collapsed
				{Text: big},                          // big here will be exact-deduplicated
			},
		},
	}

	_, _ = ApplyCompressionPipeline(in, "", false, nil)

	if called["envblocks"] <= 0 {
		t.Errorf("expected envblocks callback to receive positive saved bytes, got: %d", called["envblocks"])
	}
	if called["dedup"] <= 0 {
		t.Errorf("expected dedup callback to receive positive saved bytes, got: %d", called["dedup"])
	}
}

// TestPipeline_CompressionMetricsExtended verifies that ALL 8 optimization and compression
// stages correctly invoke the metrics callback and log structured bytes saved.
func TestPipeline_CompressionMetricsExtended(t *testing.T) {
	defer withNormalize(t, true)()
	defer withCollapseEnv(t, true, 100)()
	defer withDedup(t, true, 150)()
	defer withSubstringDedup(t, true, 300)()
	defer withPruneTools(t, true)()
	defer withToolResult(t, true, 500, 50, 50)()
	defer withLoopBreak(t, true, false)()

	called := make(map[string]int)
	SetCompressionMetrics(func(stage string, bytes int) {
		called[stage] += bytes
	})
	defer func() {
		onCompressionSaved = func(stage string, bytes int) {}
	}()

	envBlock := makeEnvBlock(3000)
	big := strings.Repeat("E", 200)
	substringNeedle := "THIS IS A REALLY BIG NEEDLE EMBEDDED IN USER TURNS " + strings.Repeat("N", 300)

	// Run 1: High/No budget to test 7 of the stages (keeping first turn intact for dedup needle)
	t.Run("Stage 1-7 (no trim)", func(t *testing.T) {
		defer withMaxInputChars(t, 0)()

		in := []*genai.Content{
			// 1. Initial user turn containing some bloated whitespace, big text, and envBlock
			{
				Role: genai.RoleUser,
				Parts: []*genai.Part{
					{Text: "first user turn " + envBlock + "   \r\n\r\n\r\n"},
					{Text: big},
					{Text: substringNeedle},
				},
			},
			// 2. Initial model turn that makes a read_file call
			{
				Role: "model",
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							Name: "read_file",
							Args: map[string]any{"path": "foo.txt"},
							ID:   "call_001",
						},
					},
				},
			},
			// 3. User response to tool call (will be pruned later by a subsequent identical call)
			{
				Role: "user",
				Parts: []*genai.Part{
					{
						FunctionResponse: &genai.FunctionResponse{
							Name: "read_file",
							Response: map[string]any{"output": "huge file content " + strings.Repeat("F", 1000)},
							ID:   "call_001",
						},
					},
				},
			},
			// 4. Model repeats the exact same read_file call (superseding turn 2)
			{
				Role: "model",
				Parts: []*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							Name: "read_file",
							Args: map[string]any{"path": "foo.txt"},
							ID:   "call_002",
						},
					},
				},
			},
			// 5. User response to second tool call
			{
				Role: "user",
				Parts: []*genai.Part{
					{
						FunctionResponse: &genai.FunctionResponse{
							Name: "read_file",
							Response: map[string]any{"output": "huge file content " + strings.Repeat("F", 1000)},
							ID:   "call_002",
						},
					},
				},
			},
			// 6. User turn that scolded model (will be trapped/collapsed by BreakLoopTrap if duplicated)
			{
				Role: "user",
				Parts: []*genai.Part{
					{Text: "You did not use a tool in your previous response!"},
				},
			},
			// 7. Duplicate scolding turn (duplicate loop trap)
			{
				Role: "user",
				Parts: []*genai.Part{
					{Text: "You did not use a tool in your previous response!"},
				},
			},
			// 8. Model turn with some tool result that is oversized (for tool result truncation)
			{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "I will read the file."},
				},
			},
			// 9. Tool response to read_file that is oversized (will be middle-elided)
			{
				Role: "user",
				Parts: []*genai.Part{
					{
						FunctionResponse: &genai.FunctionResponse{
							Name: "read_file",
							Response: map[string]any{"output": "head line\n" + strings.Repeat("A\n", 500) + "tail line"},
							ID:   "call_003",
						},
					},
				},
			},
			// 10. Final user turn containing duplicate envBlock, duplicate big block, and substring needle
			{
				Role: genai.RoleUser,
				Parts: []*genai.Part{
					{Text: "second user turn " + envBlock}, // envBlock here will be collapsed
					{Text: big},                          // big here will be exact-deduplicated
					{Text: "Some extra text " + substringNeedle}, // substring deduplication
				},
			},
		}

		_, _ = ApplyCompressionPipeline(in, "   system prompt text   \r\n\r\n\r\n", false, nil)
	})

	// Run 2: Low budget to test "trim" stage specifically
	t.Run("Stage 8 (with trim)", func(t *testing.T) {
		defer withMaxInputChars(t, 150)()

		in := []*genai.Content{
			{
				Role:  genai.RoleUser,
				Parts: []*genai.Part{{Text: "massive history turn that will get trimmed" + strings.Repeat("X", 3000)}},
			},
			{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "ack"}},
			},
			{
				Role:  genai.RoleUser,
				Parts: []*genai.Part{{Text: "short user turn that survives"}},
			},
		}

		_, _ = ApplyCompressionPipeline(in, "system prompt", false, nil)
	})

	// Verify that ALL metrics were called with positive numbers
	stages := []string{"normalize", "envblocks", "toolresult", "dedup", "dedup_substring", "prune_tools", "trim", "loopbreak"}
	for _, stage := range stages {
		if called[stage] <= 0 {
			t.Errorf("expected stage %q callback to receive positive saved bytes, got: %d", stage, called[stage])
		} else {
			t.Logf("stage %q successfully reported %d saved bytes", stage, called[stage])
		}
	}
}

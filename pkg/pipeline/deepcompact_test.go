package pipeline

import (
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestDeepCompactHistoricalTurns(t *testing.T) {
	// Enable deep compaction and configure small keep window for testing
	t.Setenv("GW_DEEP_COMPACT", "true")
	t.Setenv("GW_DEEP_COMPACT_KEEP_TURNS", "3")
	t.Setenv("GW_DEEP_COMPACT_MAX_BYTES", "20")
	t.Setenv("GW_DEEP_COMPACT_HEAD_BYTES", "10")
	t.Setenv("GW_DEEP_COMPACT_TAIL_BYTES", "5")

	// Back up package-level configurations to restore after the test
	origEnabled := deepCompactEnabled
	origKeepTurns := deepCompactKeepTurns
	origMaxBytes := deepCompactMaxBytes
	origHeadBytes := deepCompactHeadBytes
	origTailBytes := deepCompactTailBytes

	t.Cleanup(func() {
		deepCompactEnabled = origEnabled
		deepCompactKeepTurns = origKeepTurns
		deepCompactMaxBytes = origMaxBytes
		deepCompactHeadBytes = origHeadBytes
		deepCompactTailBytes = origTailBytes
	})

	// Apply overrides
	deepCompactEnabled = true
	deepCompactKeepTurns = 3
	deepCompactMaxBytes = 20
	deepCompactHeadBytes = 10
	deepCompactTailBytes = 5

	contents := []*genai.Content{
		// Turn 0: Cold. Has long FunctionResponse and long text.
		{
			Role: "user",
			Parts: []*genai.Part{
				{
					Text: "This is a very long user query that is in the cold history and exceeds our max bytes threshold.",
				},
				{
					FunctionResponse: &genai.FunctionResponse{
						Name: "custom_unregistered_tool",
						Response: map[string]any{
							"stdout": "Compilation succeeded with 0 errors and 15 warnings. Emitted binary dist/app.",
						},
					},
				},
			},
		},
		// Turn 1: Cold. Has small text.
		{
			Role: "model",
			Parts: []*genai.Part{
				{
					Text: "short",
				},
			},
		},
		// Turn 2: Warm boundary. Has long text. Should NOT be touched.
		{
			Role: "user",
			Parts: []*genai.Part{
				{
					Text: "This is a warm-window user query which is very long but should be preserved intact.",
				},
			},
		},
		// Turn 3: Warm. Has long FunctionResponse. Should NOT be touched.
		{
			Role: "model",
			Parts: []*genai.Part{
				{
					FunctionResponse: &genai.FunctionResponse{
						Name: "read_file",
						Response: map[string]any{
							"content": "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"warm window content intact\")\n}",
						},
					},
				},
			},
		},
		// Turn 4: Warm (latest). Has long text. Should NOT be touched.
		{
			Role: "user",
			Parts: []*genai.Part{
				{
					Text: "Final user turn in the warm window.",
				},
			},
		},
	}

	var metricStage string
	var metricBytes int
	onCompressionSaved = func(stage string, bytes int) {
		metricStage = stage
		metricBytes = bytes
	}
	defer func() {
		onCompressionSaved = func(stage string, bytes int) {}
	}()

	result := DeepCompactHistoricalTurns(contents)

	if len(result) != len(contents) {
		t.Fatalf("expected length %d, got %d", len(contents), len(result))
	}

	// 1. Verify Turn 0 (Cold) is deeply compacted
	t0 := result[0]
	if !containsElision(t0.Parts[0].Text) {
		t.Errorf("Turn 0 part 0 (text) was not deeply compacted: %q", t0.Parts[0].Text)
	}
	frStdout := t0.Parts[1].FunctionResponse.Response["stdout"].(string)
	if !containsElision(frStdout) {
		t.Errorf("Turn 0 part 1 (FunctionResponse) was not deeply compacted: %q", frStdout)
	}

	// 2. Verify Turn 1 (Cold but small) is untouched
	t1 := result[1]
	if t1.Parts[0].Text != "short" {
		t.Errorf("Turn 1 part 0 (small text) was corrupted: %q", t1.Parts[0].Text)
	}

	// 3. Verify Turn 2 (Warm) is untouched
	t2 := result[2]
	expectedT2 := "This is a warm-window user query which is very long but should be preserved intact."
	if t2.Parts[0].Text != expectedT2 {
		t.Errorf("Turn 2 (warm boundary) was mutated: got %q, want %q", t2.Parts[0].Text, expectedT2)
	}

	// 4. Verify Turn 3 (Warm) is untouched
	t3 := result[3]
	frContent := t3.Parts[0].FunctionResponse.Response["content"].(string)
	if containsElision(frContent) {
		t.Errorf("Turn 3 (warm FunctionResponse) was compacted: %q", frContent)
	}

	// 5. Verify Turn 4 (Warm) is untouched
	t4 := result[4]
	expectedT4 := "Final user turn in the warm window."
	if t4.Parts[0].Text != expectedT4 {
		t.Errorf("Turn 4 (warm latest) was mutated: got %q, want %q", t4.Parts[0].Text, expectedT4)
	}

	// Verify that deepcompact metrics were correctly triggered
	if metricStage != "deepcompact" {
		t.Errorf("expected metric stage 'deepcompact', got %q", metricStage)
	}
	if metricBytes <= 0 {
		t.Errorf("expected positive metric bytes saved, got %d", metricBytes)
	}
}

func TestDeepCompactHistoricalTurns_Semantic(t *testing.T) {
	// Enable deep compaction
	t.Setenv("GW_DEEP_COMPACT", "true")
	t.Setenv("GW_DEEP_COMPACT_KEEP_TURNS", "1")
	t.Setenv("GW_DEEP_COMPACT_MAX_BYTES", "10")

	origEnabled := deepCompactEnabled
	origKeepTurns := deepCompactKeepTurns
	origMaxBytes := deepCompactMaxBytes

	t.Cleanup(func() {
		deepCompactEnabled = origEnabled
		deepCompactKeepTurns = origKeepTurns
		deepCompactMaxBytes = origMaxBytes
	})

	deepCompactEnabled = true
	deepCompactKeepTurns = 1
	deepCompactMaxBytes = 10

	contents := []*genai.Content{
		// Turn 0: Cold. Has various tool results to semantically collapse
		{
			Role: "user",
			Parts: []*genai.Part{
				// 1. read_file
				{
					FunctionResponse: &genai.FunctionResponse{
						Name: "read_file",
						Response: map[string]any{
							"path":    "main.go",
							"content": "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"stale file content\")\n}",
						},
					},
				},
				// 2. execute_command
				{
					FunctionResponse: &genai.FunctionResponse{
						Name: "execute_command",
						Response: map[string]any{
							"stdout": "Some massive standard output log running for 500 lines...",
						},
					},
				},
				// 3. web_search
				{
					FunctionResponse: &genai.FunctionResponse{
						Name: "web_search",
						Response: map[string]any{
							"content": "Results:\n1. Weather in Sundbyberg is 22C\n2. Population 50000",
						},
					},
				},
				// 4. web_fetch
				{
					FunctionResponse: &genai.FunctionResponse{
						Name: "web_fetch",
						Response: map[string]any{
							"text": "<html><body><h1>Massive webpage body...</h1></body></html>",
						},
					},
				},
			},
		},
		// Turn 1: Warm
		{
			Role: "model",
			Parts: []*genai.Part{
				{Text: "ack"},
			},
		},
	}

	result := DeepCompactHistoricalTurns(contents)

	t0 := result[0]

	// 1. Verify read_file is semantic collapsed
	rf := t0.Parts[0].FunctionResponse.Response["content"].(string)
	if !strings.Contains(rf, "read file 'main.go' successfully") {
		t.Errorf("read_file was not semantically collapsed: %q", rf)
	}

	// 2. Verify execute_command is semantic collapsed
	ec := t0.Parts[1].FunctionResponse.Response["stdout"].(string)
	if !strings.Contains(ec, "shell command completed") {
		t.Errorf("execute_command was not semantically collapsed: %q", ec)
	}

	// 3. Verify web_search is semantic collapsed
	ws := t0.Parts[2].FunctionResponse.Response["content"].(string)
	if !strings.Contains(ws, "web query completed successfully") {
		t.Errorf("web_search was not semantically collapsed: %q", ws)
	}

	// 4. Verify web_fetch is semantic collapsed
	wf := t0.Parts[3].FunctionResponse.Response["text"].(string)
	if !strings.Contains(wf, "web page fetch completed") {
		t.Errorf("web_fetch was not semantically collapsed: %q", wf)
	}
}

func TestPruneActiveTools(t *testing.T) {
	// Enable Active Tool Pruning and set window to 2 turns
	t.Setenv("GW_ACTIVE_TOOL_PRUNING", "true")
	t.Setenv("GW_ACTIVE_TOOL_PRUNING_WINDOW", "2")

	origPruneEnabled := activeToolPruningEnabled
	origPruneWindow := activeToolPruningWindow

	t.Cleanup(func() {
		activeToolPruningEnabled = origPruneEnabled
		activeToolPruningWindow = origPruneWindow
	})

	activeToolPruningEnabled = true
	activeToolPruningWindow = 2

	contents := []*genai.Content{
		// Turn 0 (Cold): Called web_search
		{
			Role: "user",
			Parts: []*genai.Part{
				{
					FunctionCall: &genai.FunctionCall{
						Name: "web_search",
					},
				},
			},
		},
		// Turn 1 (Cold/Warm limit): No tool calls
		{
			Role: "model",
			Parts: []*genai.Part{
				{Text: "ack"},
			},
		},
		// Turn 2 (Warm): Called execute_command
		{
			Role: "user",
			Parts: []*genai.Part{
				{
					FunctionCall: &genai.FunctionCall{
						Name: "execute_command",
					},
				},
			},
		},
		// Turn 3 (Warm - latest): No tool calls
		{
			Role: "model",
			Parts: []*genai.Part{
				{Text: "ready"},
			},
		},
	}

	opts := &GenerationOptions{
		Tools: []*genai.Tool{
			// 1. web_search (Cold): Used in Turn 0, but went cold. Should be pruned.
			{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "web_search"},
				},
			},
			// 2. execute_command (Warm/Immune): Used in Turn 2 (recently) and is immune. Should be kept.
			{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "execute_command"},
				},
			},
			// 3. write_to_file (Immune): Never used but whitelisted/immune. Should be kept.
			{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "write_to_file"},
				},
			},
			// 4. another_unused_tool (New/Never used): Never used in history. Should be kept.
			{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "another_unused_tool"},
				},
			},
		},
	}

	var metricStage string
	var metricBytes int
	onCompressionSaved = func(stage string, bytes int) {
		metricStage = stage
		metricBytes = bytes
	}
	defer func() {
		onCompressionSaved = func(stage string, bytes int) {}
	}()

	prunedCount := PruneActiveTools(contents, opts)

	if prunedCount != 1 {
		t.Errorf("expected 1 pruned tool, got %d", prunedCount)
	}

	if len(opts.Tools) != 3 {
		t.Errorf("expected 3 remaining tools, got %d", len(opts.Tools))
	}

	// Verify that web_search is gone
	for _, tool := range opts.Tools {
		for _, fd := range tool.FunctionDeclarations {
			if fd.Name == "web_search" {
				t.Error("expected web_search to be pruned, but it is still present")
			}
		}
	}

	// Verify that metrics were correctly triggered
	if metricStage != "active_tool_pruning" {
		t.Errorf("expected metric stage 'active_tool_pruning', got %q", metricStage)
	}
	if metricBytes <= 0 {
		t.Errorf("expected positive metric bytes saved, got %d", metricBytes)
	}
}

func containsElision(s string) bool {
	return len(s) > 0 && (strings.Contains(s, "elided") || strings.Contains(s, "compacted"))
}

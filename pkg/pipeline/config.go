package pipeline

import (
	"os"
	"strconv"
	"strings"

	"go.f0o.dev/cline-vertex-gw/pkg/logx"
)

// Global package-level configurations for the prompt optimization pipeline.
// Centralizing these here avoids duplicate definitions across individual stage files,
// supports the GW_PROFILE master toggle, and permits individual environment overrides.
var (
	normalizeWhitespace        bool
	cacheAlignerEnabled        bool
	breakLoopTrapEnabled       bool
	loopTrapNudgeEnabled       bool
	collapseEnvBlocks          bool
	collapseEnvMinBytes        int32
	dedupReplay                bool
	dedupMinBytes              int32
	dedupSubstring             bool
	dedupSubstringMinBytes     int32
	toolResultTruncate         bool
	toolResultMaxBytes         int32
	toolResultHeadBytes        int32
	toolResultTailBytes        int32
	pruneStaleTools            bool
	deepCompactEnabled         bool
	deepCompactKeepTurns       int32
	deepCompactMaxBytes        int32
	deepCompactHeadBytes       int32
	deepCompactTailBytes       int32
	activeToolPruningEnabled   bool
	activeToolPruningWindow    int32
	activeToolPruningWhitelist string
	maxInputChars              int32
	toolResultRetainWindow     int32
	writeActionElision         bool
	writeActionRetainWindow    int32
)

// ActiveProfileName holds the string representation of the currently loaded profile.
var ActiveProfileName string

type profileBaseline struct {
	NormalizeWhitespace        bool
	CacheAlignerEnabled        bool
	BreakLoopTrapEnabled       bool
	LoopTrapNudgeEnabled       bool
	CollapseEnvBlocks          bool
	CollapseEnvMinBytes        int32
	DedupReplay                bool
	DedupMinBytes              int32
	DedupSubstring             bool
	DedupSubstringMinBytes     int32
	ToolResultTruncate         bool
	ToolResultMaxBytes         int32
	ToolResultHeadBytes        int32
	ToolResultTailBytes        int32
	PruneStaleTools            bool
	DeepCompactEnabled         bool
	DeepCompactKeepTurns       int32
	DeepCompactMaxBytes        int32
	DeepCompactHeadBytes       int32
	DeepCompactTailBytes       int32
	ActiveToolPruningEnabled   bool
	ActiveToolPruningWindow    int32
	ActiveToolPruningWhitelist string
	MaxInputChars              int32
	ToolResultRetainWindow     int32
	WriteActionElision         bool
	WriteActionRetainWindow    int32
}

func init() {
	LoadConfig()
}

// LoadConfig parses environment configurations governed by GW_PROFILE.
// This is exposed as a helper so tests can dynamically re-initialize config.
func LoadConfig() {
	// Read GW_PROFILE (defaults to Profile 3: Balanced)
	profileStr := strings.ToLower(strings.TrimSpace(os.Getenv("GW_PROFILE")))
	if profileStr == "" {
		profileStr = "balanced"
	}

	baseline, resolvedName := getProfileBaseline(profileStr)
	ActiveProfileName = resolvedName

	// 1. Core Toggles
	normalizeWhitespace = envBool("GW_NORMALIZE_WHITESPACE", baseline.NormalizeWhitespace)
	cacheAlignerEnabled = envBool("GW_CACHE_ALIGNER", baseline.CacheAlignerEnabled)
	breakLoopTrapEnabled = envBool("GW_BREAK_LOOP_TRAP", baseline.BreakLoopTrapEnabled)
	loopTrapNudgeEnabled = envBool("GW_LOOP_TRAP_NUDGE", baseline.LoopTrapNudgeEnabled)

	// 2. Collapse Env Blocks
	collapseEnvBlocks = envBool("GW_COLLAPSE_ENV_BLOCKS", baseline.CollapseEnvBlocks)
	collapseEnvMinBytes = envInt32WithFallback("GW_COLLAPSE_ENV_MIN_BYTES", "GW_COLLAPSE_ENV_THRESHOLD", baseline.CollapseEnvMinBytes)

	// 3. Dedup Replay
	dedupReplay = envBool("GW_DEDUP_REPLAY", baseline.DedupReplay)
	dedupMinBytes = envInt32WithFallback("GW_DEDUP_MIN_BYTES", "GW_DEDUP_THRESHOLD", baseline.DedupMinBytes)

	// 4. Dedup Substring
	dedupSubstring = envBool("GW_DEDUP_SUBSTRING", baseline.DedupSubstring)
	dedupSubstringMinBytes = envInt32WithFallback("GW_DEDUP_SUBSTRING_MIN_BYTES", "GW_DEDUP_SUBSTRING_THRESHOLD", baseline.DedupSubstringMinBytes)

	// 5. Tool Result Truncation
	toolResultTruncate = envBool("GW_TOOL_RESULT_TRUNCATE", baseline.ToolResultTruncate)
	toolResultMaxBytes = envInt32WithFallback("GW_TOOL_RESULT_MAX_BYTES", "GW_TOOL_TRUNCATE_LIMIT", baseline.ToolResultMaxBytes)
	toolResultHeadBytes = envInt32("GW_TOOL_RESULT_HEAD_BYTES", baseline.ToolResultHeadBytes)
	toolResultTailBytes = envInt32("GW_TOOL_RESULT_TAIL_BYTES", baseline.ToolResultTailBytes)
	toolResultRetainWindow = envInt32("GW_TOOL_RESULT_RETAIN_WINDOW", baseline.ToolResultRetainWindow)

	// 6. Stale Tool Pruning
	pruneStaleTools = envBool("GW_PRUNE_STALE_TOOLS", baseline.PruneStaleTools)

	// 7. Deep Compaction
	deepCompactEnabled = envBool("GW_DEEP_COMPACT", baseline.DeepCompactEnabled)
	deepCompactKeepTurns = envInt32("GW_DEEP_COMPACT_KEEP_TURNS", baseline.DeepCompactKeepTurns)
	deepCompactMaxBytes = envInt32("GW_DEEP_COMPACT_MAX_BYTES", baseline.DeepCompactMaxBytes)
	deepCompactHeadBytes = envInt32("GW_DEEP_COMPACT_HEAD_BYTES", baseline.DeepCompactHeadBytes)
	deepCompactTailBytes = envInt32("GW_DEEP_COMPACT_TAIL_BYTES", baseline.DeepCompactTailBytes)

	// 8. Active Tool Pruning
	activeToolPruningEnabled = envBool("GW_ACTIVE_TOOL_PRUNING", baseline.ActiveToolPruningEnabled)
	activeToolPruningWindow = envInt32("GW_ACTIVE_TOOL_PRUNING_WINDOW", baseline.ActiveToolPruningWindow)
	activeToolPruningWhitelist = envString("GW_ACTIVE_TOOL_PRUNING_WHITELIST", baseline.ActiveToolPruningWhitelist)

	// 9. Context Trim
	maxInputChars = envInt32("GW_MAX_INPUT_CHARS", baseline.MaxInputChars)

	// 10. Write Action Elision
	writeActionElision = envBool("GW_WRITE_ACTION_ELISION", baseline.WriteActionElision)
	writeActionRetainWindow = envInt32("GW_WRITE_ACTION_RETAIN_WINDOW", baseline.WriteActionRetainWindow)
}

// getProfileBaseline maps the profile string or integer to the baseline settings.
func getProfileBaseline(p string) (profileBaseline, string) {
	// Standardized Whitelists
	defaultWhitelist := "write_to_file,replace_in_file,execute_command,read_file,ask_followup_question,attempt_completion,new_task"
	extremeWhitelist := "write_to_file,replace_in_file,execute_command"

	switch p {
	case "1", "passthrough", "raw":
		return profileBaseline{
			NormalizeWhitespace:        false,
			CacheAlignerEnabled:        false,
			BreakLoopTrapEnabled:       false,
			LoopTrapNudgeEnabled:       false,
			CollapseEnvBlocks:          false,
			CollapseEnvMinBytes:        256,
			DedupReplay:                false,
			DedupMinBytes:              512,
			DedupSubstring:             false,
			DedupSubstringMinBytes:     1024,
			ToolResultTruncate:         false,
			ToolResultMaxBytes:         8000,
			ToolResultHeadBytes:        2000,
			ToolResultTailBytes:        1000,
			PruneStaleTools:            false,
			DeepCompactEnabled:         false,
			DeepCompactKeepTurns:       12,
			DeepCompactMaxBytes:        500,
			DeepCompactHeadBytes:       200,
			DeepCompactTailBytes:       100,
			ActiveToolPruningEnabled:   false,
			ActiveToolPruningWindow:    20,
			ActiveToolPruningWhitelist: defaultWhitelist,
			MaxInputChars:              0,
			ToolResultRetainWindow:     0,
			WriteActionElision:         false,
			WriteActionRetainWindow:    0,
		}, "1. Pass-Through (Raw)"

	case "2", "gentle", "conservative":
		return profileBaseline{
			NormalizeWhitespace:        true,
			CacheAlignerEnabled:        true,
			BreakLoopTrapEnabled:       true,
			LoopTrapNudgeEnabled:       false,
			CollapseEnvBlocks:          true,
			CollapseEnvMinBytes:        1024,
			DedupReplay:                true,
			DedupMinBytes:              1024,
			DedupSubstring:             false,
			DedupSubstringMinBytes:     1024,
			ToolResultTruncate:         false, // disabled
			ToolResultMaxBytes:         8000,
			ToolResultHeadBytes:        2000,
			ToolResultTailBytes:        1000,
			PruneStaleTools:            false,
			DeepCompactEnabled:         false,
			DeepCompactKeepTurns:       12,
			DeepCompactMaxBytes:        500,
			DeepCompactHeadBytes:       200,
			DeepCompactTailBytes:       100,
			ActiveToolPruningEnabled:   false,
			ActiveToolPruningWindow:    20,
			ActiveToolPruningWhitelist: defaultWhitelist,
			MaxInputChars:              0,
			ToolResultRetainWindow:     5,
			WriteActionElision:         false,
			WriteActionRetainWindow:    5,
		}, "2. Gentle"

	case "4", "aggressive":
		return profileBaseline{
			NormalizeWhitespace:        true,
			CacheAlignerEnabled:        true,
			BreakLoopTrapEnabled:       true,
			LoopTrapNudgeEnabled:       true,
			CollapseEnvBlocks:          true,
			CollapseEnvMinBytes:        128,
			DedupReplay:                true,
			DedupMinBytes:              256,
			DedupSubstring:             true,
			DedupSubstringMinBytes:     512,
			ToolResultTruncate:         true,
			ToolResultMaxBytes:         4096,
			ToolResultHeadBytes:        2000,
			ToolResultTailBytes:        1000,
			PruneStaleTools:            true,
			DeepCompactEnabled:         true,
			DeepCompactKeepTurns:       12,
			DeepCompactMaxBytes:        500,
			DeepCompactHeadBytes:       200,
			DeepCompactTailBytes:       100,
			ActiveToolPruningEnabled:   true,
			ActiveToolPruningWindow:    20,
			ActiveToolPruningWhitelist: defaultWhitelist,
			MaxInputChars:              0,
			ToolResultRetainWindow:     2,
			WriteActionElision:         true,
			WriteActionRetainWindow:    2,
		}, "4. Aggressive"

	case "5", "extreme", "squeeze":
		return profileBaseline{
			NormalizeWhitespace:        true,
			CacheAlignerEnabled:        true,
			BreakLoopTrapEnabled:       true,
			LoopTrapNudgeEnabled:       true,
			CollapseEnvBlocks:          true,
			CollapseEnvMinBytes:        64,
			DedupReplay:                true,
			DedupMinBytes:              128,
			DedupSubstring:             true,
			DedupSubstringMinBytes:     256,
			ToolResultTruncate:         true,
			ToolResultMaxBytes:         2048,
			ToolResultHeadBytes:        1000, // scaled down
			ToolResultTailBytes:        500,  // scaled down
			PruneStaleTools:            true,
			DeepCompactEnabled:         true,
			DeepCompactKeepTurns:       8,
			DeepCompactMaxBytes:        250,
			DeepCompactHeadBytes:       100,
			DeepCompactTailBytes:       50,
			ActiveToolPruningEnabled:   true,
			ActiveToolPruningWindow:    10,
			ActiveToolPruningWhitelist: extremeWhitelist,
			MaxInputChars:              350000,
			ToolResultRetainWindow:     1,
			WriteActionElision:         true,
			WriteActionRetainWindow:    1,
		}, "5. Extreme Squeeze"

	case "3", "balanced", "default":
		fallthrough
	default:
		// Profile 3: Balanced (Default Gateway Settings)
		return profileBaseline{
			NormalizeWhitespace:        true,
			CacheAlignerEnabled:        true,
			BreakLoopTrapEnabled:       true,
			LoopTrapNudgeEnabled:       true,
			CollapseEnvBlocks:          true,
			CollapseEnvMinBytes:        256,
			DedupReplay:                true,
			DedupMinBytes:              512,
			DedupSubstring:             false,
			DedupSubstringMinBytes:     1024,
			ToolResultTruncate:         true,
			ToolResultMaxBytes:         8000,
			ToolResultHeadBytes:        2000,
			ToolResultTailBytes:        1000,
			PruneStaleTools:            false,
			DeepCompactEnabled:         false,
			DeepCompactKeepTurns:       12,
			DeepCompactMaxBytes:        500,
			DeepCompactHeadBytes:       200,
			DeepCompactTailBytes:       100,
			ActiveToolPruningEnabled:   false,
			ActiveToolPruningWindow:    20,
			ActiveToolPruningWhitelist: defaultWhitelist,
			MaxInputChars:              0,
			ToolResultRetainWindow:     3,
			WriteActionElision:         true,
			WriteActionRetainWindow:    3,
		}, "3. Balanced (Default)"
	}
}

// envInt32WithFallback parses a non-negative int32 from primary or fallback env var.
func envInt32WithFallback(primary, fallback string, def int32) int32 {
	v := strings.TrimSpace(os.Getenv(primary))
	if v == "" {
		v = strings.TrimSpace(os.Getenv(fallback))
	}
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		logx.Warn("invalid env value (want non-negative int); using default",
			"env", primary, "value", v, "default", def)
		return def
	}
	return int32(n)
}


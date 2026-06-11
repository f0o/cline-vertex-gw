package pipeline

import (
	"os"
	"testing"
)

func TestProfileBaselines(t *testing.T) {
	tests := []struct {
		profile     string
		wantProfile string
		verify      func(*testing.T)
	}{
		{
			profile:     "1",
			wantProfile: "1. Pass-Through (Raw)",
			verify: func(t *testing.T) {
				if normalizeWhitespace {
					t.Error("want normalizeWhitespace false")
				}
				if cacheAlignerEnabled {
					t.Error("want cacheAlignerEnabled false")
				}
				if breakLoopTrapEnabled {
					t.Error("want breakLoopTrapEnabled false")
				}
				if collapseEnvBlocks {
					t.Error("want collapseEnvBlocks false")
				}
				if dedupReplay {
					t.Error("want dedupReplay false")
				}
				if toolResultTruncate {
					t.Error("want toolResultTruncate false")
				}
				if pruneStaleTools {
					t.Error("want pruneStaleTools false")
				}
				if deepCompactEnabled {
					t.Error("want deepCompactEnabled false")
				}
				if activeToolPruningEnabled {
					t.Error("want activeToolPruningEnabled false")
				}
				if smartCrusherEnabled {
					t.Error("want smartCrusherEnabled false")
				}
				if syntaxCompressorEnabled {
					t.Error("want syntaxCompressorEnabled false")
				}
				if learningLoopEnabled {
					t.Error("want learningLoopEnabled false")
				}
			},
		},
		{
			profile:     "passthrough",
			wantProfile: "1. Pass-Through (Raw)",
			verify: func(t *testing.T) {
				if normalizeWhitespace {
					t.Error("want normalizeWhitespace false")
				}
			},
		},
		{
			profile:     "2",
			wantProfile: "2. Gentle",
			verify: func(t *testing.T) {
				if !normalizeWhitespace {
					t.Error("want normalizeWhitespace true")
				}
				if !cacheAlignerEnabled {
					t.Error("want cacheAlignerEnabled true")
				}
				if !breakLoopTrapEnabled {
					t.Error("want breakLoopTrapEnabled true")
				}
				if loopTrapNudgeEnabled {
					t.Error("want loopTrapNudgeEnabled false")
				}
				if collapseEnvMinBytes != 1024 {
					t.Errorf("want collapseEnvMinBytes 1024, got %d", collapseEnvMinBytes)
				}
				if dedupMinBytes != 1024 {
					t.Errorf("want dedupMinBytes 1024, got %d", dedupMinBytes)
				}
				if toolResultTruncate {
					t.Error("want toolResultTruncate false")
				}
				if smartCrusherEnabled {
					t.Error("want smartCrusherEnabled false")
				}
				if syntaxCompressorEnabled {
					t.Error("want syntaxCompressorEnabled false")
				}
				if learningLoopEnabled {
					t.Error("want learningLoopEnabled false")
				}
			},
		},
		{
			profile:     "3",
			wantProfile: "3. Balanced (Default)",
			verify: func(t *testing.T) {
				if !normalizeWhitespace {
					t.Error("want normalizeWhitespace true")
				}
				if !cacheAlignerEnabled {
					t.Error("want cacheAlignerEnabled true")
				}
				if !breakLoopTrapEnabled {
					t.Error("want breakLoopTrapEnabled true")
				}
				if !loopTrapNudgeEnabled {
					t.Error("want loopTrapNudgeEnabled true")
				}
				if !collapseEnvBlocks {
					t.Error("want collapseEnvBlocks true")
				}
				if collapseEnvMinBytes != 256 {
					t.Errorf("want collapseEnvMinBytes 256, got %d", collapseEnvMinBytes)
				}
				if !dedupReplay {
					t.Error("want dedupReplay true")
				}
				if dedupMinBytes != 512 {
					t.Errorf("want dedupMinBytes 512, got %d", dedupMinBytes)
				}
				if dedupSubstring {
					t.Error("want dedupSubstring false")
				}
				if !toolResultTruncate {
					t.Error("want toolResultTruncate true")
				}
				if toolResultMaxBytes != 8000 {
					t.Errorf("want toolResultMaxBytes 8000, got %d", toolResultMaxBytes)
				}
				if pruneStaleTools {
					t.Error("want pruneStaleTools false")
				}
				if deepCompactEnabled {
					t.Error("want deepCompactEnabled false")
				}
				if activeToolPruningEnabled {
					t.Error("want activeToolPruningEnabled false")
				}
				if !smartCrusherEnabled {
					t.Error("want smartCrusherEnabled true")
				}
				if syntaxCompressorEnabled {
					t.Error("want syntaxCompressorEnabled false")
				}
				if learningLoopEnabled {
					t.Error("want learningLoopEnabled false")
				}
			},
		},
		{
			profile:     "balanced",
			wantProfile: "3. Balanced (Default)",
			verify: func(t *testing.T) {
				if !normalizeWhitespace {
					t.Error("want normalizeWhitespace true")
				}
			},
		},
		{
			profile:     "4",
			wantProfile: "4. Aggressive",
			verify: func(t *testing.T) {
				if !normalizeWhitespace {
					t.Error("want normalizeWhitespace true")
				}
				if !cacheAlignerEnabled {
					t.Error("want cacheAlignerEnabled true")
				}
				if !breakLoopTrapEnabled {
					t.Error("want breakLoopTrapEnabled true")
				}
				if !collapseEnvBlocks {
					t.Error("want collapseEnvBlocks true")
				}
				if collapseEnvMinBytes != 128 {
					t.Errorf("want collapseEnvMinBytes 128, got %d", collapseEnvMinBytes)
				}
				if !dedupReplay {
					t.Error("want dedupReplay true")
				}
				if dedupMinBytes != 256 {
					t.Errorf("want dedupMinBytes 256, got %d", dedupMinBytes)
				}
				if !dedupSubstring {
					t.Error("want dedupSubstring true")
				}
				if dedupSubstringMinBytes != 512 {
					t.Errorf("want dedupSubstringMinBytes 512, got %d", dedupSubstringMinBytes)
				}
				if !toolResultTruncate {
					t.Error("want toolResultTruncate true")
				}
				if toolResultMaxBytes != 4096 {
					t.Errorf("want toolResultMaxBytes 4096, got %d", toolResultMaxBytes)
				}
				if !pruneStaleTools {
					t.Error("want pruneStaleTools true")
				}
				if !deepCompactEnabled {
					t.Error("want deepCompactEnabled true")
				}
				if deepCompactKeepTurns != 12 {
					t.Errorf("want deepCompactKeepTurns 12, got %d", deepCompactKeepTurns)
				}
				if !activeToolPruningEnabled {
					t.Error("want activeToolPruningEnabled true")
				}
				if activeToolPruningWindow != 20 {
					t.Errorf("want activeToolPruningWindow 20, got %d", activeToolPruningWindow)
				}
				if !smartCrusherEnabled {
					t.Error("want smartCrusherEnabled true")
				}
				if !syntaxCompressorEnabled {
					t.Error("want syntaxCompressorEnabled true")
				}
				if learningLoopEnabled {
					t.Error("want learningLoopEnabled false")
				}
			},
		},
		{
			profile:     "5",
			wantProfile: "5. Extreme Squeeze",
			verify: func(t *testing.T) {
				if !normalizeWhitespace {
					t.Error("want normalizeWhitespace true")
				}
				if !cacheAlignerEnabled {
					t.Error("want cacheAlignerEnabled true")
				}
				if !breakLoopTrapEnabled {
					t.Error("want breakLoopTrapEnabled true")
				}
				if !collapseEnvBlocks {
					t.Error("want collapseEnvBlocks true")
				}
				if collapseEnvMinBytes != 64 {
					t.Errorf("want collapseEnvMinBytes 64, got %d", collapseEnvMinBytes)
				}
				if !dedupReplay {
					t.Error("want dedupReplay true")
				}
				if dedupMinBytes != 128 {
					t.Errorf("want dedupMinBytes 128, got %d", dedupMinBytes)
				}
				if !dedupSubstring {
					t.Error("want dedupSubstring true")
				}
				if dedupSubstringMinBytes != 256 {
					t.Errorf("want dedupSubstringMinBytes 256, got %d", dedupSubstringMinBytes)
				}
				if !toolResultTruncate {
					t.Error("want toolResultTruncate true")
				}
				if toolResultMaxBytes != 2048 {
					t.Errorf("want toolResultMaxBytes 2048, got %d", toolResultMaxBytes)
				}
				if !pruneStaleTools {
					t.Error("want pruneStaleTools true")
				}
				if !deepCompactEnabled {
					t.Error("want deepCompactEnabled true")
				}
				if deepCompactKeepTurns != 8 {
					t.Errorf("want deepCompactKeepTurns 8, got %d", deepCompactKeepTurns)
				}
				if deepCompactMaxBytes != 250 {
					t.Errorf("want deepCompactMaxBytes 250, got %d", deepCompactMaxBytes)
				}
				if deepCompactHeadBytes != 100 {
					t.Errorf("want deepCompactHeadBytes 100, got %d", deepCompactHeadBytes)
				}
				if deepCompactTailBytes != 50 {
					t.Errorf("want deepCompactTailBytes 50, got %d", deepCompactTailBytes)
				}
				if !activeToolPruningEnabled {
					t.Error("want activeToolPruningEnabled true")
				}
				if activeToolPruningWindow != 10 {
					t.Errorf("want activeToolPruningWindow 10, got %d", activeToolPruningWindow)
				}
				if activeToolPruningWhitelist != "write_to_file,replace_in_file,execute_command" {
					t.Errorf("want activeToolPruningWhitelist ... got %s", activeToolPruningWhitelist)
				}
				if maxInputChars != 350000 {
					t.Errorf("want maxInputChars 350000, got %d", maxInputChars)
				}
				if !smartCrusherEnabled {
					t.Error("want smartCrusherEnabled true")
				}
				if !syntaxCompressorEnabled {
					t.Error("want syntaxCompressorEnabled true")
				}
				if !learningLoopEnabled {
					t.Error("want learningLoopEnabled true")
				}
			},
		},
	}

	// Back up all environment variables we might modify
	origProfile := os.Getenv("GW_PROFILE")
	origNormalize := os.Getenv("GW_NORMALIZE_WHITESPACE")
	origCacheAligner := os.Getenv("GW_CACHE_ALIGNER")
	origCollapse := os.Getenv("GW_COLLAPSE_ENV_BLOCKS")
	origCollapseMin := os.Getenv("GW_COLLAPSE_ENV_MIN_BYTES")
	origCollapseThreshold := os.Getenv("GW_COLLAPSE_ENV_THRESHOLD")
	origDedupMin := os.Getenv("GW_DEDUP_MIN_BYTES")
	origDedupThreshold := os.Getenv("GW_DEDUP_THRESHOLD")
	origDedupSubMin := os.Getenv("GW_DEDUP_SUBSTRING_MIN_BYTES")
	origDedupSubThreshold := os.Getenv("GW_DEDUP_SUBSTRING_THRESHOLD")
	origToolMax := os.Getenv("GW_TOOL_RESULT_MAX_BYTES")
	origToolLimit := os.Getenv("GW_TOOL_TRUNCATE_LIMIT")
	origSmartCrusher := os.Getenv("GW_SMART_CRUSHER")
	origSyntax := os.Getenv("GW_SYNTAX_COMPRESSOR")
	origLearning := os.Getenv("GW_LEARNING_LOOP")

	defer func() {
		os.Setenv("GW_PROFILE", origProfile)
		os.Setenv("GW_NORMALIZE_WHITESPACE", origNormalize)
		os.Setenv("GW_CACHE_ALIGNER", origCacheAligner)
		os.Setenv("GW_COLLAPSE_ENV_BLOCKS", origCollapse)
		os.Setenv("GW_COLLAPSE_ENV_MIN_BYTES", origCollapseMin)
		os.Setenv("GW_COLLAPSE_ENV_THRESHOLD", origCollapseThreshold)
		os.Setenv("GW_DEDUP_MIN_BYTES", origDedupMin)
		os.Setenv("GW_DEDUP_THRESHOLD", origDedupThreshold)
		os.Setenv("GW_DEDUP_SUBSTRING_MIN_BYTES", origDedupSubMin)
		os.Setenv("GW_DEDUP_SUBSTRING_THRESHOLD", origDedupSubThreshold)
		os.Setenv("GW_TOOL_RESULT_MAX_BYTES", origToolMax)
		os.Setenv("GW_TOOL_TRUNCATE_LIMIT", origToolLimit)
		os.Setenv("GW_SMART_CRUSHER", origSmartCrusher)
		os.Setenv("GW_SYNTAX_COMPRESSOR", origSyntax)
		os.Setenv("GW_LEARNING_LOOP", origLearning)
		LoadConfig() // Restore original config state
	}()

	for _, tc := range tests {
		t.Run(tc.profile, func(t *testing.T) {
			// Clear any overrides from environment first
			os.Unsetenv("GW_NORMALIZE_WHITESPACE")
			os.Unsetenv("GW_CACHE_ALIGNER")
			os.Unsetenv("GW_COLLAPSE_ENV_BLOCKS")
			os.Unsetenv("GW_COLLAPSE_ENV_MIN_BYTES")
			os.Unsetenv("GW_COLLAPSE_ENV_THRESHOLD")
			os.Unsetenv("GW_DEDUP_MIN_BYTES")
			os.Unsetenv("GW_DEDUP_THRESHOLD")
			os.Unsetenv("GW_DEDUP_SUBSTRING_MIN_BYTES")
			os.Unsetenv("GW_DEDUP_SUBSTRING_THRESHOLD")
			os.Unsetenv("GW_TOOL_RESULT_MAX_BYTES")
			os.Unsetenv("GW_TOOL_TRUNCATE_LIMIT")
			os.Unsetenv("GW_SMART_CRUSHER")
			os.Unsetenv("GW_SYNTAX_COMPRESSOR")
			os.Unsetenv("GW_LEARNING_LOOP")

			os.Setenv("GW_PROFILE", tc.profile)
			LoadConfig()

			if ActiveProfileName != tc.wantProfile {
				t.Errorf("want profile name %q, got %q", tc.wantProfile, ActiveProfileName)
			}
			tc.verify(t)
		})
	}
}

func TestProfileOverridesAndFallbacks(t *testing.T) {
	origProfile := os.Getenv("GW_PROFILE")
	origNormalize := os.Getenv("GW_NORMALIZE_WHITESPACE")
	origCollapseThreshold := os.Getenv("GW_COLLAPSE_ENV_THRESHOLD")
	origDedupThreshold := os.Getenv("GW_DEDUP_THRESHOLD")
	origDedupSubThreshold := os.Getenv("GW_DEDUP_SUBSTRING_THRESHOLD")
	origToolLimit := os.Getenv("GW_TOOL_TRUNCATE_LIMIT")
	origSmartCrusher := os.Getenv("GW_SMART_CRUSHER")
	origSyntax := os.Getenv("GW_SYNTAX_COMPRESSOR")
	origLearning := os.Getenv("GW_LEARNING_LOOP")

	defer func() {
		os.Setenv("GW_PROFILE", origProfile)
		os.Setenv("GW_NORMALIZE_WHITESPACE", origNormalize)
		os.Setenv("GW_COLLAPSE_ENV_THRESHOLD", origCollapseThreshold)
		os.Setenv("GW_DEDUP_THRESHOLD", origDedupThreshold)
		os.Setenv("GW_DEDUP_SUBSTRING_THRESHOLD", origDedupSubThreshold)
		os.Setenv("GW_TOOL_TRUNCATE_LIMIT", origToolLimit)
		os.Setenv("GW_SMART_CRUSHER", origSmartCrusher)
		os.Setenv("GW_SYNTAX_COMPRESSOR", origSyntax)
		os.Setenv("GW_LEARNING_LOOP", origLearning)
		LoadConfig()
	}()

	// 1. Setup profile = 4 (Aggressive) but override normalizeWhitespace to false
	os.Setenv("GW_PROFILE", "aggressive")
	os.Setenv("GW_NORMALIZE_WHITESPACE", "false")
	os.Setenv("GW_SMART_CRUSHER", "false")
	os.Setenv("GW_SYNTAX_COMPRESSOR", "false")
	os.Setenv("GW_LEARNING_LOOP", "false")
	LoadConfig()

	if normalizeWhitespace {
		t.Error("want normalizeWhitespace overridden to false")
	}
	if !deepCompactEnabled {
		t.Error("want deepCompactEnabled true from Aggressive baseline")
	}
	if smartCrusherEnabled {
		t.Error("want smartCrusherEnabled overridden to false")
	}
	if syntaxCompressorEnabled {
		t.Error("want syntaxCompressorEnabled overridden to false")
	}
	if learningLoopEnabled {
		t.Error("want learningLoopEnabled overridden to false")
	}

	// 2. Setup profile = 3 (Balanced) but test fallback aliases
	os.Setenv("GW_PROFILE", "3")
	os.Unsetenv("GW_NORMALIZE_WHITESPACE")

	os.Setenv("GW_COLLAPSE_ENV_THRESHOLD", "99")
	os.Setenv("GW_DEDUP_THRESHOLD", "88")
	os.Setenv("GW_DEDUP_SUBSTRING_THRESHOLD", "77")
	os.Setenv("GW_TOOL_TRUNCATE_LIMIT", "66")
	LoadConfig()

	if collapseEnvMinBytes != 99 {
		t.Errorf("want collapseEnvMinBytes fallback to 99, got %d", collapseEnvMinBytes)
	}
	if dedupMinBytes != 88 {
		t.Errorf("want dedupMinBytes fallback to 88, got %d", dedupMinBytes)
	}
	if dedupSubstringMinBytes != 77 {
		t.Errorf("want dedupSubstringMinBytes fallback to 77, got %d", dedupSubstringMinBytes)
	}
	if toolResultMaxBytes != 66 {
		t.Errorf("want toolResultMaxBytes fallback to 66, got %d", toolResultMaxBytes)
	}
}

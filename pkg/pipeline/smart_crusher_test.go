package pipeline

import (
	"fmt"
	"strings"
	"testing"
)

func TestSmartCrushJSON(t *testing.T) {
	// Enable SmartCrush
	smartCrusherEnabled = true

	// 1. Array with no errors, size > 4. Should be collapsed.
	// Make it large enough so that saved > 0 (overcoming the 112-byte marker overhead)
	var sb strings.Builder
	sb.WriteString("[")
	for i := 1; i <= 50; i++ {
		sb.WriteString(fmt.Sprintf(`{"id": %d, "name": "alice_with_a_long_name_to_increase_byte_count_for_compression"},`, i))
	}
	// Trim trailing comma
	s := sb.String()
	s = s[:len(s)-1] + "]"

	crushed, saved := SmartCrush(s)
	if saved <= 0 {
		t.Fatalf("expected JSON to be crushed, but saved was %d. Crushed: %s", saved, crushed)
	}

	if !strings.Contains(crushed, "elided 46 items") {
		t.Errorf("expected collapsed array middle indicator, got: %s", crushed)
	}

	// 2. Array with an error in the middle. The item with error should be kept!
	var sb2 strings.Builder
	sb2.WriteString("[")
	for i := 1; i <= 50; i++ {
		if i == 25 {
			sb2.WriteString(`{"id": 25, "name": "charlie_error_failed_status_exception_critical_log"},`)
		} else {
			sb2.WriteString(fmt.Sprintf(`{"id": %d, "name": "alice_with_a_long_name_to_increase_byte_count_for_compression"},`, i))
		}
	}
	s2 := sb2.String()
	s2 = s2[:len(s2)-1] + "]"

	crushed2, saved2 := SmartCrush(s2)
	if saved2 <= 0 {
		t.Fatalf("expected errorJSON to be crushed, but saved was %d", saved2)
	}

	if !strings.Contains(crushed2, "charlie_error_failed_status_exception_critical_log") {
		t.Errorf("expected item containing error term to be preserved, but it was elided! Got: %s", crushed2)
	}

	// Verify it still contains a collapse marker since there are other middle items
	if !strings.Contains(crushed2, "elided 22 items") {
		t.Errorf("expected elision of other clean items, got: %s", crushed2)
	}
}

func TestSmartCrushLog(t *testing.T) {
	// Enable SmartCrush
	smartCrusherEnabled = true

	// 1. Clean log without errors. Should collapse middle.
	var cleanLines []string
	for i := 1; i <= 100; i++ {
		cleanLines = append(cleanLines, fmt.Sprintf("line %d: processing clean standard state with standard execution step and sequence details", i))
	}

	cleanLog := strings.Join(cleanLines, "\n")
	crushed, saved := SmartCrush(cleanLog)
	if saved <= 0 {
		t.Fatalf("expected clean log to be crushed, but saved was %d", saved)
	}

	if !strings.Contains(crushed, "elided 94 clean log lines") {
		t.Errorf("expected clean lines to be collapsed, got: %s", crushed)
	}

	// 2. Log with an error in the middle. The error line should be preserved!
	var errorLines []string
	for i := 1; i <= 100; i++ {
		if i == 50 {
			errorLines = append(errorLines, "line 50: ERROR: failed to compile file.go in current working directory")
		} else {
			errorLines = append(errorLines, fmt.Sprintf("line %d: processing clean standard state with standard execution step and sequence details", i))
		}
	}

	errorLog := strings.Join(errorLines, "\n")
	crushed2, saved2 := SmartCrush(errorLog)
	if saved2 <= 0 {
		t.Fatalf("expected error log to be crushed, but saved was %d", saved2)
	}

	if !strings.Contains(crushed2, "ERROR: failed to compile file.go") {
		t.Errorf("expected error line to be preserved, got: %s", crushed2)
	}

	if !strings.Contains(crushed2, "elided 46 clean log lines") {
		t.Errorf("expected clean lines to still be collapsed around the error, got: %s", crushed2)
	}
}

func TestSmartCrushWalkJSONEdgeCases(t *testing.T) {
	// Object crushing should not crash on non-slice nested components
	objJSON := `{"data": {"nested": "value", "empty": []}, "status": "success", "count": 20}`
	_, _ = SmartCrush(objJSON)
}

package logx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// withCapturedDefault swaps in a JSON slog handler writing to buf at the given
// level for the duration of fn, restoring the previous default afterwards.
func withCapturedDefault(t *testing.T, level slog.Level, fn func()) *bytes.Buffer {
	t.Helper()
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: level})))
	fn()
	return buf
}

// decodeRecords parses newline-delimited JSON slog records from buf.
func decodeRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad json record %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestHelpersEmitCorrectLevels verifies each leveled helper stamps the matching
// slog level — the core promise of the package.
func TestHelpersEmitCorrectLevels(t *testing.T) {
	buf := withCapturedDefault(t, slog.LevelDebug, func() {
		Debug("d")
		Info("i")
		Warn("w")
		Error("e")
	})
	recs := decodeRecords(t, buf)
	want := []string{"DEBUG", "INFO", "WARN", "ERROR"}
	if len(recs) != len(want) {
		t.Fatalf("got %d records, want %d", len(recs), len(want))
	}
	for i, lvl := range want {
		if recs[i]["level"] != lvl {
			t.Errorf("record %d level=%v, want %v", i, recs[i]["level"], lvl)
		}
	}
}

// TestLevelFiltering verifies LOG_LEVEL-style filtering actually drops lower
// levels — proving severity is honored rather than collapsed to one level.
func TestLevelFiltering(t *testing.T) {
	buf := withCapturedDefault(t, slog.LevelWarn, func() {
		Debug("d")
		Info("i")
		Warn("w")
		Error("e")
	})
	recs := decodeRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("expected only WARN+ERROR to pass at LevelWarn, got %d records", len(recs))
	}
	for _, r := range recs {
		if r["level"] == "DEBUG" || r["level"] == "INFO" {
			t.Errorf("unexpected low-level record passed filter: %v", r["level"])
		}
	}
}

// TestComponentAttribute verifies For/Scoped attach the component attribute so
// the event stream is filterable by subsystem.
func TestComponentAttribute(t *testing.T) {
	buf := withCapturedDefault(t, slog.LevelDebug, func() {
		For("pricing").Info("hello")
		Scoped("tags").Debugf("found %d", 7)
	})
	recs := decodeRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("got %d records, want 2", len(recs))
	}
	if recs[0]["component"] != "pricing" {
		t.Errorf("record 0 component=%v, want pricing", recs[0]["component"])
	}
	if recs[1]["component"] != "tags" {
		t.Errorf("record 1 component=%v, want tags", recs[1]["component"])
	}
	if recs[1]["msg"] != "found 7" {
		t.Errorf("Debugf msg=%v, want 'found 7'", recs[1]["msg"])
	}
}

// TestScopedLoggerResolvesDefaultLazily is the regression test for the bug
// where package-level `var x = logx.Scoped("c")` froze the bootstrap
// INFO-level handler and silently dropped DEBUG records even when the process
// later installed a DEBUG handler. A Scoped logger created BEFORE SetDefault
// must still honor the handler/level installed AFTER it.
func TestScopedLoggerResolvesDefaultLazily(t *testing.T) {
	// Create the logger first, while the default is whatever it is now.
	lg := Scoped("pipeline")

	// Now install a DEBUG JSON handler — simulating main.configureLogging()
	// running after package-level loggers were constructed.
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	buf := &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	lg.Debugf("optimization ran: saved %dB", 1234)

	recs := decodeRecords(t, buf)
	if len(recs) != 1 {
		t.Fatalf("expected the DEBUG line to be emitted after late SetDefault, got %d records", len(recs))
	}
	if recs[0]["level"] != "DEBUG" {
		t.Errorf("level=%v, want DEBUG", recs[0]["level"])
	}
	if recs[0]["component"] != "pipeline" {
		t.Errorf("component=%v, want pipeline", recs[0]["component"])
	}
}

// TestScopedFHelperLevels verifies the printf-style helpers map to the right
// levels.

func TestScopedFHelperLevels(t *testing.T) {
	buf := withCapturedDefault(t, slog.LevelDebug, func() {
		lg := Scoped("c")
		lg.Debugf("d%d", 1)
		lg.Infof("i%d", 2)
		lg.Warnf("w%d", 3)
		lg.Errorf("e%d", 4)
	})
	recs := decodeRecords(t, buf)
	want := []string{"DEBUG", "INFO", "WARN", "ERROR"}
	for i, lvl := range want {
		if recs[i]["level"] != lvl {
			t.Errorf("record %d level=%v, want %v", i, recs[i]["level"], lvl)
		}
	}
}

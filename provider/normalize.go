package provider

import (
	"go.f0o.dev/cline-vertex-gw/logx"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logNormalize scopes pipeline-compression logs to component=normalize (DEBUG: per-request diagnostics).
var logNormalize = logx.Scoped("normalize")

// Whitespace normalization is the cheapest, safest token-compression layer
// in the gateway. It targets bloat introduced by tool outputs and editor
// pastes: trailing spaces, multiple blank lines, BOMs, and CRLF line
// endings — all of which consume input tokens for zero semantic value.
//
//   - GW_NORMALIZE_WHITESPACE (default: on)
//     When enabled (or unset), each text part is rewritten in place:
//   - CRLF and CR are converted to LF
//   - leading BOMs (U+FEFF) are stripped
//   - trailing whitespace on each line is removed
//   - runs of 3+ blank lines are collapsed to 2
//     The transform is lossless for code (leading whitespace, which carries
//     indentation, is preserved) and prose (sentence content is untouched).
//     The system prompt is normalized too because it dominates byte counts
//     in Cline workloads.
//
// Set to any of {0,false,off,no} to disable.
var normalizeWhitespace = envBool("GW_NORMALIZE_WHITESPACE", true)

// NormalizeWhitespace returns a new slice of *genai.Content where each Part's
// Text has been normalized. Original Contents/Parts are NOT mutated — the
// caller's slice may be shared across retries or goroutines.
//
// When the GW_NORMALIZE_WHITESPACE knob is off this is a fast-path no-op.
//
// NormalizedSystemPrompt is the matching helper for the system prompt
// string, kept separate so handlers can pass it through cleanly.
func NormalizeWhitespace(contents []*genai.Content) []*genai.Content {
	if !normalizeWhitespace {
		return contents
	}
	if len(contents) == 0 {
		return contents
	}
	out := make([]*genai.Content, len(contents))
	totalSaved := 0
	for i, c := range contents {
		if c == nil {
			out[i] = nil
			continue
		}
		nc := &genai.Content{Role: c.Role}
		if len(c.Parts) > 0 {
			nc.Parts = make([]*genai.Part, len(c.Parts))
			for j, p := range c.Parts {
				if p == nil {
					nc.Parts[j] = nil
					continue
				}
				if p.Text == "" {
					// Preserve non-text parts as-is (the budget trimmer
					// already treats them as opaque 8B placeholders).
					np := *p
					nc.Parts[j] = &np
					continue
				}
				np := *p
				np.Text = normalizeText(p.Text)
				totalSaved += len(p.Text) - len(np.Text)
				nc.Parts[j] = &np
			}
		}
		out[i] = nc
	}
	if totalSaved > 0 {
		logNormalize.L().Debug("normalized contents whitespace",
			slog.Int("bytes_saved", totalSaved),
		)
		onCompressionSaved("normalize", totalSaved)
	}
	return out
}

// NormalizeSystemPrompt applies the same transform to a standalone string
// (the system prompt is carried separately from Contents).
func NormalizeSystemPrompt(s string) string {
	if !normalizeWhitespace || s == "" {
		return s
	}
	ns := normalizeText(s)
	saved := len(s) - len(ns)
	if saved > 0 {
		logNormalize.L().Debug("normalized system prompt whitespace",
			slog.Int("bytes_saved", saved),
		)
		onCompressionSaved("normalize", saved)
	}
	return ns
}

// normalizeText performs the actual byte-level rewrite. It's structured to
// allocate at most one new string for the typical case (no leading BOM, no
// CRs, already-clean trailing whitespace and blank-line runs).
//
// Order of operations:
//  1. Strip a leading UTF-8 BOM if present.
//  2. Replace CRLF -> LF and bare CR -> LF.
//  3. For each line, trim trailing horizontal whitespace (spaces and tabs).
//  4. Collapse runs of 3+ blank lines down to exactly 2 ("\n\n").
//
// We deliberately do NOT touch leading whitespace (preserves code
// indentation) and do NOT collapse intra-line whitespace runs (would break
// formatted text like tables or ASCII art).
func normalizeText(s string) string {
	if s == "" {
		return s
	}

	// 1. BOM strip.
	const bom = "\ufeff"
	s = strings.TrimPrefix(s, bom)

	// 2. CRLF/CR -> LF. Only allocate if needed.
	if strings.Contains(s, "\r") {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
	}

	// 3 + 4 in a single pass. We re-emit line by line so we can:
	//   - trim trailing spaces/tabs (rule 3)
	//   - keep a running count of consecutive blank lines and cap it at 2
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blankRun := 0
	for _, ln := range lines {
		// Trim only trailing horizontal whitespace; we keep all other
		// trailing chars (e.g. punctuation) intact.
		trimmed := strings.TrimRight(ln, " \t")
		if trimmed == "" {
			blankRun++
			// Cap at 2 trailing blank lines (which renders as one blank
			// separator between paragraphs after rejoin with "\n").
			if blankRun > 2 {
				continue
			}
		} else {
			blankRun = 0
		}
		out = append(out, trimmed)
	}
	// Trim trailing blank lines entirely — they're pure tail noise.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

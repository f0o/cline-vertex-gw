package pipeline

import (
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logDedupSub scopes pipeline-compression logs to component=dedup-substring (DEBUG: per-request diagnostics).
var logDedupSub = logx.Scoped("dedup-substring")

// Whole-block dedup (DedupReplayedBlocks) only fires on EXACT matches: the
// later text part must be byte-identical to an earlier one. In real Cline
// sessions the common case is subtler — the model re-shows a file it edited
// (so the body differs by a few lines), or a tool re-pastes a large block
// that is now embedded inside a slightly larger turn. Whole-block dedup
// misses both.
//
// This compressor catches PARTIAL re-pastes: when a large earlier text block
// appears VERBATIM as a contiguous substring inside a later same-role text
// part, the embedded copy is replaced with a back-pointer, leaving the
// surrounding (new) text intact.
//
//   - GW_DEDUP_SUBSTRING (default: OFF)
//     Opt-in. This is more aggressive than whole-block dedup and carries a
//     small risk of confusing a model that expects to re-read the full inline
//     content, so it ships disabled by default. Turn on with {1,true,on,yes}.
//   - GW_DEDUP_SUBSTRING_MIN_BYTES (default: 1024)
//     Minimum length of the earlier block that we will look for as a
//     substring. Larger than the whole-block threshold because substring
//     search is costlier and the win must clearly beat the placeholder.
//
// Conservative on purpose:
//   - Only EARLIER full text blocks (≥ threshold) are used as needles, and we
//     only search same-role later turns. A user turn's content is never
//     replaced with a pointer to an assistant turn (speaker matters).
//   - The needle must appear as a CONTIGUOUS verbatim substring. We never do
//     fuzzy / token-overlap matching — that risks deleting text the model
//     genuinely needs.
//   - At most one needle is collapsed per later part (the longest applicable
//     one), keeping the rewrite easy to reason about.
//   - First occurrences are always kept verbatim; only later embeddings are
//     replaced, and placeholders always point BACKWARD in conversation time.
//
// Runs AFTER DedupReplayedBlocks (exact dedup is cheaper and strictly subset)
// so we don't re-scan blocks already collapsed to a placeholder.
var (
	dedupSubstring         = envBool("GW_DEDUP_SUBSTRING", false)
	dedupSubstringMinBytes = envInt32("GW_DEDUP_SUBSTRING_MIN_BYTES", 1024)
)

// substringNeedle is an earlier large text block recorded as a candidate to
// search for inside later same-role turns.
type substringNeedle struct {
	role string
	text string
	turn int // 0-based index of the earlier turn it came from
}

// DedupSubstringBlocks returns a copy of contents in which any later same-role
// text part containing an earlier large block verbatim has that embedded copy
// replaced with a short back-pointing placeholder. The original slice and
// Content objects are not mutated.
//
// When GW_DEDUP_SUBSTRING is off this is a fast-path no-op.
func DedupSubstringBlocks(contents []*genai.Content) []*genai.Content {
	if !dedupSubstring || len(contents) < 2 {
		return contents
	}

	// Collect earlier large text blocks as needles, longest first so the
	// most valuable collapse wins when several apply.
	var needles []substringNeedle

	out := make([]*genai.Content, len(contents))
	totalSaved := 0
	replacedCount := 0
	for i, c := range contents {
		if c == nil {
			out[i] = c
			continue
		}
		nc := &genai.Content{Role: c.Role}
		if len(c.Parts) > 0 {
			nc.Parts = make([]*genai.Part, len(c.Parts))
		}
		for j, p := range c.Parts {
			if p == nil || p.Text == "" {
				if p != nil {
					np := *p
					nc.Parts[j] = &np
				} else {
					nc.Parts[j] = nil
				}
				continue
			}
			text := p.Text
			// Try to collapse the longest applicable earlier needle that is
			// embedded in this part (but is not the whole part — that case is
			// already handled by exact dedup).
			best := -1
			for k, n := range needles {
				if n.role != c.Role {
					continue
				}
				if len(n.text) >= len(text) {
					continue // not a strict substring (equal handled elsewhere)
				}
				if !strings.Contains(text, n.text) {
					continue
				}
				if best < 0 || len(n.text) > len(needles[best].text) {
					best = k
				}
			}
			if best >= 0 {
				n := needles[best]
				placeholder := fmt.Sprintf(
					"[%d bytes elided: identical block already shown in turn %d]",
					len(n.text), n.turn+1)
				newText := strings.Replace(text, n.text, placeholder, 1)
				np := *p
				np.Text = newText
				nc.Parts[j] = &np
				totalSaved += len(text) - len(newText)
				replacedCount++
				// Record the (now-shorter) rewritten block as a future needle
				// only if it still clears the threshold.
				if int32(len(newText)) >= dedupSubstringMinBytes {
					needles = append(needles, substringNeedle{role: c.Role, text: newText, turn: i})
				}
				continue
			}

			// No collapse: pass through and, if large enough, record as a
			// needle for later turns.
			np := *p
			nc.Parts[j] = &np
			if int32(len(text)) >= dedupSubstringMinBytes {
				needles = append(needles, substringNeedle{role: c.Role, text: text, turn: i})
			}
		}
		out[i] = nc
	}
	if replacedCount > 0 {
		logDedupSub.L().Debug("collapsed embedded block(s)",
			slog.Int("replaced_count", replacedCount),
			slog.Int("bytes_saved", totalSaved),
		)
		onCompressionSaved("dedup_substring", totalSaved)
	}
	return out
}

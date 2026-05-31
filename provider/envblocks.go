package provider

import (
	"fmt"
	"go.f0o.dev/cline-vertex-gw/logx"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logEnvblocks scopes pipeline-compression logs to component=envblocks (DEBUG: per-request diagnostics).
var logEnvblocks = logx.Scoped("envblocks")

// Cline injects an <environment_details>...</environment_details> block at
// the END of every user turn. The block carries the IDE's current state:
// open tabs, visible files, working directory, recursive file tree,
// terminals, current mode, and a few more housekeeping fields.
//
// Across many turns, the SAME (mostly-static) environment payload gets
// re-shipped on every request. Only the FINAL turn's snapshot matters for
// reasoning about "right now"; older snapshots are stale and dilute the
// prompt with bytes the model doesn't need.
//
// This file collapses stale <environment_details> blocks on every USER turn
// except the most recent one, replacing them with a tiny placeholder so the
// model still sees that an environment block was attached (some prompts may
// reference its prior presence) without paying for the full body.
//
//   - GW_COLLAPSE_ENV_BLOCKS (default: on)
//     Set to {0,false,off,no} to disable.
//   - GW_COLLAPSE_ENV_MIN_BYTES (default: 256)
//     A block smaller than this is left alone — collapsing tiny blocks
//     costs more bytes (the placeholder) than it saves.
//
// Like the other compressors this runs at the dispatch layer so every
// publisher benefits without per-adapter edits.
var (
	collapseEnvBlocks   = envBool("GW_COLLAPSE_ENV_BLOCKS", true)
	collapseEnvMinBytes = envInt32("GW_COLLAPSE_ENV_MIN_BYTES", 256)
)

// envOpenTag / envCloseTag are the literal markers Cline emits. They are
// matched case-sensitively because Cline's emitter always uses lowercase.
const (
	envOpenTag  = "<environment_details>"
	envCloseTag = "</environment_details>"
)

// CollapseEnvBlocks returns a new slice of *genai.Content in which any
// <environment_details>...</environment_details> blocks found inside USER
// turns (except the LAST user turn) are replaced with a short placeholder.
//
// Behavior contract:
//   - The latest user turn's env block is ALWAYS preserved verbatim. That
//     snapshot is the one the model needs for current-state reasoning.
//   - Only blocks ≥ GW_COLLAPSE_ENV_MIN_BYTES are touched. Tiny blocks
//     can't recoup the placeholder cost.
//   - Assistant turns are never modified.
//   - Original contents are NOT mutated; a shallow-copy slice is returned.
//   - Multiple env blocks within one turn are all handled (defensive — we
//     have not seen Cline emit more than one, but the parser is general).
//
// When the knob is disabled, the input slice is returned unchanged (fast
// path, no allocation).
func CollapseEnvBlocks(contents []*genai.Content) []*genai.Content {
	if !collapseEnvBlocks || len(contents) == 0 {
		return contents
	}

	// Find the index of the LAST user turn — that one is exempt.
	lastUserIdx := -1
	for i := len(contents) - 1; i >= 0; i-- {
		c := contents[i]
		if c == nil {
			continue
		}
		if c.Role == genai.RoleUser || c.Role == "user" {
			lastUserIdx = i
			break
		}
	}

	out := make([]*genai.Content, len(contents))
	totalSaved := 0
	collapsedCount := 0
	for i, c := range contents {
		if c == nil || i == lastUserIdx {
			out[i] = c
			continue
		}
		if c.Role != genai.RoleUser && c.Role != "user" {
			out[i] = c
			continue
		}
		nc, saved, n := collapseInContent(c)
		out[i] = nc
		totalSaved += saved
		collapsedCount += n
	}
	if collapsedCount > 0 {
		logEnvblocks.L().Debug("collapsed stale env block(s)",
			slog.Int("collapsed_count", collapsedCount),
			slog.Int("bytes_saved", totalSaved),
		)
		onCompressionSaved("envblocks", totalSaved)
	}
	return out
}

// collapseInContent returns a copy of c with env blocks collapsed in each
// text part, the total bytes saved, and the number of blocks collapsed.
// The original Content/Parts are not mutated.
func collapseInContent(c *genai.Content) (*genai.Content, int, int) {
	if c == nil || len(c.Parts) == 0 {
		return c, 0, 0
	}
	saved := 0
	count := 0
	nc := &genai.Content{Role: c.Role, Parts: make([]*genai.Part, len(c.Parts))}
	for i, p := range c.Parts {
		if p == nil {
			nc.Parts[i] = nil
			continue
		}
		if p.Text == "" {
			np := *p
			nc.Parts[i] = &np
			continue
		}
		newText, s, n := collapseEnvBlocksInText(p.Text)
		np := *p
		np.Text = newText
		nc.Parts[i] = &np
		saved += s
		count += n
	}
	return nc, saved, count
}

// collapseEnvBlocksInText scans `s` for env_details blocks and replaces any
// that exceed the min-bytes threshold with a one-line placeholder. Returns
// the rewritten string, bytes saved, and number of blocks collapsed.
//
// Parsing is a deliberately simple non-nested scanner: Cline doesn't nest
// env blocks. If a malformed (unterminated) open tag appears, we leave the
// remaining text intact rather than swallowing everything to EOF.
func collapseEnvBlocksInText(s string) (string, int, int) {
	if !strings.Contains(s, envOpenTag) {
		return s, 0, 0
	}
	var b strings.Builder
	b.Grow(len(s))
	saved := 0
	count := 0
	i := 0
	for i < len(s) {
		openIdx := strings.Index(s[i:], envOpenTag)
		if openIdx < 0 {
			b.WriteString(s[i:])
			break
		}
		openIdx += i
		// Emit everything up to the open tag verbatim.
		b.WriteString(s[i:openIdx])

		closeRel := strings.Index(s[openIdx+len(envOpenTag):], envCloseTag)
		if closeRel < 0 {
			// Unterminated open tag: emit the rest verbatim and stop.
			b.WriteString(s[openIdx:])
			break
		}
		closeIdx := openIdx + len(envOpenTag) + closeRel + len(envCloseTag)
		blockLen := closeIdx - openIdx

		if int32(blockLen) < collapseEnvMinBytes {
			// Below threshold — keep the block as-is.
			b.WriteString(s[openIdx:closeIdx])
		} else {
			placeholder := fmt.Sprintf(
				"%s[%d bytes elided: stale IDE snapshot from earlier turn]%s",
				envOpenTag, blockLen, envCloseTag)
			b.WriteString(placeholder)
			saved += blockLen - len(placeholder)
			count++
		}
		i = closeIdx
	}
	return b.String(), saved, count
}

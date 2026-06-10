package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"io"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logDedup scopes pipeline-compression logs to component=dedup (DEBUG: per-request diagnostics).
var logDedup = logx.Scoped("dedup")

// Cline replays identical content across turns constantly: the same
// `read_file` result, the same tool output, the same code paste from an
// earlier turn. The byte budget trimmer keeps the most-recent N turns
// intact, but those turns still contain duplicates of bytes already
// present in EARLIER kept turns.
//
// This compressor walks the kept-turn window and replaces verbatim repeats
// of large content blocks with a one-line placeholder that points at the
// earlier turn. The model still has the content available in the earlier
// position; the placeholder just says "see turn N".
//
//   - GW_DEDUP_REPLAY (default: on)
//   - GW_DEDUP_MIN_BYTES (default: 512)
//     Only blocks ≥ this size are eligible. The placeholder text itself is
//     ~80 bytes, so anything smaller couldn't recoup the overhead.
//
// Conservative on purpose:
//   - We only collapse FULL-block matches (entire text part identical to an
//     earlier full-block hash). Sub-string matching would be more powerful
//     but risks splitting structured content (XML/JSON fragments) in ways
//     the model can't reason about.
//   - Assistant turns are dedup candidates too, but only against EARLIER
//     assistant turns of the same conversation — we don't replace a user
//     turn's content with a pointer to an assistant turn, since the
//     speaker matters semantically.
//   - The FIRST occurrence is always kept verbatim. Only the 2nd, 3rd, …
//     occurrences are replaced. Because placeholders always point BACKWARD
//     in conversation time, the last turn is NOT exempt: if the user's
//     final question re-pastes a large file already shown earlier, the
//     copy in the final turn becomes the placeholder and the earlier
//     verbatim version remains.
//
// Like the other compressors this runs at the dispatch layer so all
// publishers benefit uniformly.

// dedupHash is a short hex prefix of SHA-256(text). 16 hex chars = 64 bits
// of address space, which gives ~5×10⁻²⁰ collision probability for any
// realistic conversation length — effectively zero. Using a short prefix
// keeps the placeholder readable.
const dedupHashLen = 16

// DedupReplayedBlocks returns a copy of contents in which any text OR image
// part whose entire body (hash) was already seen in an earlier turn of the
// SAME role is replaced with a short placeholder. The original slice and
// Content objects are not mutated.
//
// Image dedup is particularly valuable for vision-Cline workflows: the
// "take a screenshot every turn to confirm the change" loop ships
// near-identical PNGs across consecutive turns. SHA-256 of the decoded
// bytes catches exact-match dupes (which dominate in practice — the user
// usually doesn't move the IDE between snapshots). The placeholder
// replaces the image part with a small text part referencing the earlier
// turn; downstream adapters that don't support text-only image references
// (every adapter we ship does) would still see the image in its original
// position.
//
// When the GW_DEDUP_REPLAY knob is off this is a fast-path no-op.
//
// Should be called AFTER TrimContents: we only want to dedupe within the
// turns we're actually shipping. Running it before trim would waste cycles
// on content that's about to be dropped anyway.
func DedupReplayedBlocks(contents []*genai.Content) []*genai.Content {
	if !dedupReplay || len(contents) < 2 {
		return contents
	}

	// firstSeen[role+kind+hash] = "turn index of the first occurrence".
	// Keying on role keeps the placeholder semantically honest: a user
	// turn pointing at an earlier user turn for the same content is
	// meaningful; pointing at an assistant turn would be a category error.
	// The kind prefix ("text" vs "image") prevents the theoretical (if
	// astronomically unlikely) collision between a text block whose hash
	// happens to match an image block's hash.
	firstSeen := make(map[string]int)

	out := make([]*genai.Content, len(contents))
	totalSaved := 0
	replacedCount := 0
	replacedImages := 0
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
			if p == nil {
				nc.Parts[j] = nil
				continue
			}
			// Image-part dedup. Image parts hit the dedup path
			// unconditionally — even a tiny image (16×16 favicon, say)
			// shipped twice is worth eliding because the second copy
			// adds zero information.
			if p.InlineData != nil && len(p.InlineData.Data) > 0 {
				key := dedupKeyImage(c.Role, p.InlineData.MIMEType, p.InlineData.Data)
				if prevTurn, ok := firstSeen[key]; ok {
					placeholder := fmt.Sprintf(
						"[image elided: identical %s image (%d bytes, sha256=%s…) already shown in turn %d]",
						p.InlineData.MIMEType, len(p.InlineData.Data),
						key[len(c.Role)+len("|image|"):len(c.Role)+len("|image|")+dedupHashLen],
						prevTurn+1)
					nc.Parts[j] = &genai.Part{Text: placeholder}
					totalSaved += len(p.InlineData.Data) - len(placeholder)
					replacedCount++
					replacedImages++
					continue
				}
				firstSeen[key] = i
				np := *p
				nc.Parts[j] = &np
				continue
			}
			if p.Text == "" || int32(len(p.Text)) < dedupMinBytes {
				np := *p
				nc.Parts[j] = &np
				// Don't index sub-threshold parts; they're not candidates
				// for being pointed at either.
				continue
			}
			key := dedupKey(c.Role, p.Text)
			if prevTurn, ok := firstSeen[key]; ok {
				placeholder := fmt.Sprintf(
					"[%d bytes elided: identical content already shown in turn %d (sha256=%s…)]",
					len(p.Text), prevTurn+1,
					key[len(c.Role)+len("|text|"):len(c.Role)+len("|text|")+dedupHashLen])
				np := *p
				np.Text = placeholder
				nc.Parts[j] = &np
				totalSaved += len(p.Text) - len(placeholder)
				replacedCount++
				continue
			}
			// First occurrence — record and pass through verbatim.
			firstSeen[key] = i
			np := *p
			nc.Parts[j] = &np
		}
		out[i] = nc
	}
	if replacedCount > 0 {
		logDedup.L().Debug("replaced duplicate block(s)",
			slog.Int("replaced_count", replacedCount),
			slog.Int("replaced_images", replacedImages),
			slog.Int("bytes_saved", totalSaved),
		)
		onCompressionSaved("dedup", totalSaved)
	}
	return out
}

// dedupKey builds a role-scoped hash key for text content.
// Format: "<role>|text|<hexhash>". The "|text|" / "|image|" infix prevents
// hash-collision-induced cross-kind matches and gives the placeholder
// builder a known-offset extraction point for the hash slice.
func dedupKey(role, text string) string {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, text)
	h := hasher.Sum(nil)
	hex := hex.EncodeToString(h)[:dedupHashLen]
	var sb strings.Builder
	sb.Grow(len(role) + len("|text|") + len(hex))
	sb.WriteString(role)
	sb.WriteString("|text|")
	sb.WriteString(hex)
	return sb.String()
}

// dedupKeyImage builds a role-scoped hash key for an image. The MIME type
// is folded into the hash input so a PNG and a JPEG with byte-coincidental
// hashes (vanishingly unlikely but theoretically possible) wouldn't
// cross-match. Format: "<role>|image|<hexhash>".
func dedupKeyImage(role, mime string, data []byte) string {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, mime)
	hasher.Write([]byte{0}) // separator so "image/png" + "X" can't collide with "image/pn" + "gX"
	hasher.Write(data)
	sum := hasher.Sum(nil)
	hex := hex.EncodeToString(sum)[:dedupHashLen]
	var sb strings.Builder
	sb.Grow(len(role) + len("|image|") + len(hex))
	sb.WriteString(role)
	sb.WriteString("|image|")
	sb.WriteString(hex)
	return sb.String()
}

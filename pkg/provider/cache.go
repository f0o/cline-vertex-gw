package provider

import (
	"go.f0o.dev/cline-vertex-gw/pkg/pipeline"
	"google.golang.org/genai"
)

// Prompt-cache planning is the gateway's single, provider-agnostic policy for
// deciding WHICH prefix boundaries are worth caching. It lives here (not in any
// per-publisher adapter) so every publisher inherits the same economics, and a
// future publisher only has to translate a CachePlan into its own primitive.
//
// THE ECONOMICS (why we are selective):
//   - A cache WRITE costs ~125% of the base input-token price (a one-time 25%
//     premium for the tokens written into the cache).
//   - A cache READ costs ~10% of the base input-token price (a 90% saving).
//   - Break-even: a cached prefix must be re-read at least ~once before the
//     write premium pays for itself. Caching a prefix that is NEVER read back
//     is a pure 25% loss.
//
// Therefore the cardinal rule, enforced uniformly for every provider here:
//
//	NEVER place a cache breakpoint on a prefix unless we have high confidence
//	it will be read back by a later request — i.e. there is at least one turn
//	AFTER the cached prefix in THIS request (so a follow-up request that
//	repeats the prefix can hit it), AND the prefix is large enough to clear
//	the upstream's minimum cacheable size.
//
// "Caching everything" negates the savings; this planner deliberately caches
// only the few high-ROI, high-stability anchors.

// Cache planning knobs. All follow the project's GW_* opt-out convention and
// are evaluated at startup (toggling requires a process restart).
//
//   - GW_PROMPT_CACHE (default: on)
//     Master switch. When off, PlanCache returns an empty plan so no provider
//     emits cache markers and no explicit cache resources are created.
//
//   - GW_CACHE_MIN_BYTES (default: 4000)
//     Minimum byte length a prefix must reach before it is worth caching.
//     ~3.5 chars/token on English prose => 4000 chars ≈ 1100 tokens, above the
//     1024-token minimum cacheable prefix for Claude Sonnet/Opus. Using a byte
//     heuristic avoids bundling per-publisher tokenizers.
//
//   - GW_CACHE_TAIL_MIN_TURNS (default: 6)
//     Minimum number of conversation turns before the rolling "tail" breakpoint
//     is considered. Short conversations rarely re-read a tail prefix, so this
//     guards against wasted writes.
//
//   - GW_CACHE_TAIL_MIN_BYTES (default: 16000)
//     Minimum cumulative byte size of the conversation prefix up to (and
//     including) the tail anchor before the rolling breakpoint is placed.
var (
	promptCacheEnabled = envBool("GW_PROMPT_CACHE", true)
	cacheMinBytes      = envInt32("GW_CACHE_MIN_BYTES", 4000)
	cacheTailMinTurns  = envInt32("GW_CACHE_TAIL_MIN_TURNS", 6)
	cacheTailMinBytes  = envInt32("GW_CACHE_TAIL_MIN_BYTES", 16000)
)

// maxCacheBreakpoints caps how many breakpoints a plan may contain. Anthropic
// allows at most 4 inline cache_control markers; we cap at that across the
// board so a plan is valid for the most-constrained explicit primitive. Our
// plan never proposes more than 3 (system, first-user, tail) so this is a
// defensive ceiling rather than an active constraint today.
const maxCacheBreakpoints = 4

// CacheCapability describes how a publisher implements prompt caching. The
// CachePlan is computed identically for all of them; the capability tells the
// dispatch / adapter layer HOW to honor it.
type CacheCapability int

const (
	// CacheNone: the publisher has no prompt-caching primitive on Vertex AI
	// (e.g. Cohere). The plan is informational only — the conversation-shape
	// compression pipeline still helps, but there is nothing to "tag".
	CacheNone CacheCapability = iota
	// CacheExplicitInline: per-request inline breakpoint markers chosen by us
	// (Anthropic `cache_control:{ephemeral}`). The plan's anchors translate
	// directly into markers.
	CacheExplicitInline
	// CacheExplicitResource: a separately-created cache resource referenced by
	// name with a TTL (Gemini `CachedContent`). The plan's CacheSystem anchor
	// drives whether we mint/reuse such a resource.
	CacheExplicitResource
	// CacheImplicit: the provider caches stable prefixes automatically with no
	// caller control (OpenAI-compatible MaaS, DeepSeek). We cannot place
	// breakpoints; the best lever is prefix stability (already handled by the
	// compression pipeline) plus surfacing cached-token telemetry.
	CacheImplicit
)

// CacheCapabilityFor maps a publisher namespace to its caching capability.
func CacheCapabilityFor(publisher string) CacheCapability {
	switch {
	case publisher == "anthropic":
		return CacheExplicitInline
	case publisher == "google":
		return CacheExplicitResource
	case publisher == "cohere":
		return CacheNone
	case isOpenAICompatPublisher(publisher):
		return CacheImplicit

	default:
		return CacheNone
	}
}

// CachePlan is the provider-agnostic result of the caching policy. It describes
// WHERE breakpoints should go as semantic anchors (by conversation-content
// index), decoupled from any wire format. Each adapter applies the anchors it
// can honor using its own primitive.
//
// Index semantics refer to the position within the per-turn message list that
// an adapter builds from `contents` (consecutive same-role turns are merged by
// some adapters; the anchors are expressed in terms of the merged turn order,
// which PlanCache also uses internally — see mergedTurns).
type CachePlan struct {
	// CacheSystem is true when the system(+tools) prefix is large and stable
	// enough to cache AND there is conversation that will read it back.
	CacheSystem bool
	// FirstUserTurnIdx is the merged-turn index of the first user turn to cache
	// the prefix THROUGH, or -1 when the first user turn should not be cached.
	FirstUserTurnIdx int
	// TailTurnIdx is the merged-turn index of a rolling tail breakpoint for
	// long sessions, or -1 when no tail breakpoint is warranted.
	TailTurnIdx int
}

// IsEmpty reports whether the plan proposes no caching at all.
func (p CachePlan) IsEmpty() bool {
	return !p.CacheSystem && p.FirstUserTurnIdx < 0 && p.TailTurnIdx < 0
}

// BreakpointCount returns how many cache breakpoints the plan proposes.
func (p CachePlan) BreakpointCount() int {
	n := 0
	if p.CacheSystem {
		n++
	}
	if p.FirstUserTurnIdx >= 0 {
		n++
	}
	if p.TailTurnIdx >= 0 {
		n++
	}
	return n
}

// plannedTurn is a minimal, provider-neutral view of one merged conversation
// turn used by the planner. It mirrors the same-role merging that the
// per-publisher adapters perform so anchor indices line up.
type plannedTurn struct {
	role  string // "user" | "assistant"
	bytes int
}

// mergedTurns collapses consecutive same-role genai.Contents into single turns
// and measures each turn's approximate byte size, so the planner reasons about
// the SAME turn boundaries the adapters will produce. Empty turns are skipped
// (matching the adapters, which drop content blocks that yield nothing).
func mergedTurns(contents []*genai.Content) []plannedTurn {
	var turns []plannedTurn
	for _, c := range contents {
		if c == nil {
			continue
		}
		b := pipeline.ContentBytes(c)
		if b == 0 {
			continue
		}
		role := "user"
		if c.Role == genai.RoleModel || c.Role == "assistant" {
			role = "assistant"
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].bytes += b
			continue
		}
		turns = append(turns, plannedTurn{role: role, bytes: b})
	}
	return turns
}

// PlanCache is the single economic brain shared by every provider. It applies
// the selectivity rules described at the top of this file to decide which
// prefix anchors are worth caching for THIS request.
//
// The rules, in order:
//  1. Master switch / capability gate. If GW_PROMPT_CACHE is off, or the
//     publisher has no caching primitive at all (CacheNone), return an empty
//     plan — there is nothing to do.
//  2. System prefix: cache only when it clears GW_CACHE_MIN_BYTES AND there is
//     at least one conversation turn that will read it back. (A one-shot call
//     with no follow-up would pay the write premium for zero reads.)
//  3. First user turn: cache the prefix through it only when that turn clears
//     GW_CACHE_MIN_BYTES AND a LATER turn exists to hit the cache. This is the
//     stable session head for agentic clients (large tools array + first
//     environment snapshot sit in this prefix).
//  4. Rolling tail: for long sessions (>= GW_CACHE_TAIL_MIN_TURNS turns and a
//     prefix >= GW_CACHE_TAIL_MIN_BYTES up to the anchor) place ONE breakpoint
//     on the last stable turn before the current/final user turn, so the
//     growing middle of the conversation is cached for the next request.
//  5. Cap the total at maxCacheBreakpoints (defensive).
//
// The plan is computed against the post-compression contents so anchor indices
// match what the adapters serialize.
func PlanCache(contents []*genai.Content, systemPrompt string, publisher string) CachePlan {
	empty := CachePlan{FirstUserTurnIdx: -1, TailTurnIdx: -1}
	if !promptCacheEnabled {
		return empty
	}
	switch CacheCapabilityFor(publisher) {
	case CacheNone, CacheImplicit:
		// Nothing for us to place. (Implicit-cache providers benefit from
		// prefix stability handled elsewhere; explicit anchors are a no-op.)
		return empty
	}

	turns := mergedTurns(contents)
	if len(turns) == 0 {
		return empty
	}

	plan := empty
	minBytes := int(cacheMinBytes)

	// Rule 2: system prefix — only when there is a turn to read it back.
	sysBytes := len(systemPrompt)
	hasFollowupAfterSystem := len(turns) >= 2 // need a later turn to hit it
	if sysBytes >= minBytes && hasFollowupAfterSystem {
		plan.CacheSystem = true
	}

	// Rule 3: first user turn — only when a LATER turn exists to read it back.
	firstUserIdx := -1
	for i, t := range turns {
		if t.role == "user" {
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx >= 0 &&
		len(turns) > firstUserIdx+1 &&
		turns[firstUserIdx].bytes >= minBytes {
		plan.FirstUserTurnIdx = firstUserIdx
	}

	// Rule 4: rolling tail breakpoint for long sessions.
	plan.TailTurnIdx = planTailBreakpoint(turns, firstUserIdx)

	// Rule 5: cap total breakpoints. Drop the tail first (lowest confidence),
	// then the first-user anchor, keeping the system anchor (highest stability).
	for plan.BreakpointCount() > maxCacheBreakpoints {
		switch {
		case plan.TailTurnIdx >= 0:
			plan.TailTurnIdx = -1
		case plan.FirstUserTurnIdx >= 0:
			plan.FirstUserTurnIdx = -1
		default:
			plan.CacheSystem = false
		}
	}
	return plan
}

// planTailBreakpoint chooses an index for the rolling tail breakpoint, or -1
// when the session is too short/small to warrant one.
//
// The anchor is the last "stable" turn — the most recent turn that is NOT the
// current/final user turn (the final user turn is the new question and won't be
// re-read in its current position). We require the conversation to be long
// (>= GW_CACHE_TAIL_MIN_TURNS) and the prefix up to the anchor to be large
// (>= GW_CACHE_TAIL_MIN_BYTES) so we only cache history likely to be replayed.
//
// We also avoid colliding with the first-user anchor: if the only stable turn
// is the first user turn (already handled by Rule 3) we return -1.
func planTailBreakpoint(turns []plannedTurn, firstUserIdx int) int {
	if int32(len(turns)) < cacheTailMinTurns {
		return -1
	}
	// Anchor at the second-to-last turn (the last turn is the new question).
	anchor := len(turns) - 2
	if anchor <= firstUserIdx {
		// Nothing stable beyond the first-user head; Rule 3 already covers it.
		return -1
	}
	// Cumulative prefix bytes up to and including the anchor.
	prefix := 0
	for i := 0; i <= anchor; i++ {
		prefix += turns[i].bytes
	}
	if int32(prefix) < cacheTailMinBytes {
		return -1
	}
	return anchor
}

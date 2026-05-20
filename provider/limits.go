package provider

import (
	"log"
	"os"
	"strconv"
	"strings"
)

// Token-budget knobs control how aggressively the gateway clamps callers'
// requested generation budgets. The goal is cost predictability for Cline
// workloads that frequently leave max_tokens unset (allowing rambling) or
// set unreasonably large ceilings.
//
//   - GW_DEFAULT_MAX_OUTPUT_TOKENS (default: 0 = unset)
//     When a request omits a max-output-tokens hint, the gateway substitutes
//     this value. Use it to bound runaway generations from clients that
//     leave the field empty. Anthropic-class adapters already pick a
//     publisher-specific default (defaultAnthropicMaxTokens) when this is 0.
//
//   - GW_MAX_OUTPUT_TOKENS_HARD (default: 0 = unset)
//     A request-supplied max-output-tokens above this value is silently
//     clamped DOWN to it. Operators use this to enforce a per-deployment
//     cost ceiling regardless of what clients ask for. 0 disables the cap.
//
// Both knobs are evaluated at startup so toggling them requires a process
// restart. They affect every publisher (Anthropic / Gemini / OpenAI-compat /
// Cohere) uniformly via ApplyOutputCaps in the Generate dispatch path.
var (
	defaultMaxOutputTokens = envInt32("GW_DEFAULT_MAX_OUTPUT_TOKENS", 0)
	hardMaxOutputTokens    = envInt32("GW_MAX_OUTPUT_TOKENS_HARD", 0)
)

// envInt32 parses a non-negative int32 env var with a default. Logs and
// returns the default on garbage input so a typo in a deployment config
// can't silently disable the safety net.
func envInt32(name string, def int32) int32 {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		log.Printf("invalid %s=%q (want non-negative int); using default %d", name, v, def)
		return def
	}
	return int32(n)
}

// ApplyOutputCaps returns a (possibly new) *GenerationOptions reflecting the
// gateway's configured defaults and hard caps on MaxTokens. It is safe to
// call with nil opts; the returned value is non-nil iff a cap was applied.
//
// Semantics, in order:
//  1. If the caller LEFT MaxTokens UNSET and GW_DEFAULT_MAX_OUTPUT_TOKENS > 0,
//     substitute that default. The default is the operator's stated intent
//     for silent callers — it takes precedence over the hard cap, which
//     exists to bound EXPLICIT caller requests rather than to override the
//     configured default downward. (If default > hard, we additionally
//     clamp to hard below; this is the operator's misconfiguration to fix.)
//  2. Otherwise, if GW_MAX_OUTPUT_TOKENS_HARD > 0 and the resulting value
//     exceeds it, clamp DOWN to the hard cap.
//  3. Otherwise leave opts unchanged.
//
// We never raise a caller's explicit small value (clamping is one-way: down).
func ApplyOutputCaps(opts *GenerationOptions) *GenerationOptions {
	if defaultMaxOutputTokens == 0 && hardMaxOutputTokens == 0 {
		return opts // No knobs configured; fast path.
	}

	// Determine the effective MaxTokens the caller is asking for. nil means
	// "use upstream default".
	var curr int32 = -1 // sentinel: unset
	if opts != nil && opts.MaxTokens != nil {
		curr = *opts.MaxTokens
	}

	// Step 1: fill in default for silent callers.
	newMax := curr
	if newMax == -1 && defaultMaxOutputTokens > 0 {
		newMax = defaultMaxOutputTokens
	}

	// Step 2: clamp the (possibly defaulted) value down to the hard cap.
	// This also catches the case where the caller set MaxTokens explicitly
	// above the hard cap, AND the case where the operator misconfigured
	// default > hard.
	if hardMaxOutputTokens > 0 && newMax > hardMaxOutputTokens {
		newMax = hardMaxOutputTokens
	}
	// Step 2b: if there's a hard cap and the caller stayed silent AND no
	// default is configured, fall back to the hard cap (better than letting
	// the upstream choose an unbounded value when an operator explicitly set
	// a ceiling).
	if newMax == -1 && hardMaxOutputTokens > 0 {
		newMax = hardMaxOutputTokens
	}

	if newMax == curr || newMax == -1 {
		return opts // No change required.
	}

	// Apply the clamp without mutating the caller's struct (it may be shared).
	out := &GenerationOptions{}
	if opts != nil {
		*out = *opts
	}
	out.MaxTokens = &newMax
	return out
}

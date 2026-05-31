package provider

import (
	"go.f0o.dev/cline-vertex-gw/logx"
	"sort"
	"strconv"

	"google.golang.org/genai"
)

// logPruneTools scopes pipeline-compression logs to component=prune-tools (DEBUG: per-request diagnostics).
var logPruneTools = logx.Scoped("prune-tools")

// Agentic sessions re-inspect the same things repeatedly: the model calls
// `read_file` on main.go, edits it, then `read_file`s it again to confirm;
// it `list_files` the same directory after each change. Each call ships a
// full assistant FunctionCall turn + a (potentially huge) user
// FunctionResponse turn. The OLDER read of a file the model has since re-read
// is almost always dead weight — the newer read supersedes it.
//
// This compressor drops superseded READ-ONLY tool exchanges: when the same
// read-only tool is invoked later with the SAME arguments, the earlier
// call/response pair is removed (the latest read is authoritative).
//
//   - GW_PRUNE_STALE_TOOLS (default: OFF)
//     Opt-in. Removing turns changes history shape and, for some providers,
//     can disturb tool-call/response pairing if done carelessly, so it ships
//     disabled by default. Turn on with {1,true,on,yes}.
//
// Conservative on purpose:
//   - ONLY read-only/idempotent tools are eligible (see readOnlyTools). We
//     never prune a mutation (write_to_file, execute_command, …) because its
//     side effects and output are history the model must retain.
//   - We only prune a call when a STRICTLY LATER call to the same tool with
//     identical arguments exists — the later one supersedes it.
//   - The model turn (FunctionCall) and its paired user turn
//     (FunctionResponse) are removed TOGETHER so strict role alternation and
//     call/response pairing are preserved (mirrors the loopbreak safety
//     contract).
//   - The first turn (i == 0) is never removed.
//   - A turn that mixes a prunable call with any other part (text, a second
//     non-prunable call) is left intact — we only drop clean, single-purpose
//     tool exchanges.
//
// Original contents are never mutated; a new slice is returned.
var pruneStaleTools = envBool("GW_PRUNE_STALE_TOOLS", false)

// readOnlyTools are idempotent inspection tools whose older invocations can be
// safely dropped once superseded by a later identical call. Mutating tools
// (write_to_file, replace_in_file, execute_command, …) are deliberately
// excluded — their effects are load-bearing conversation history.
var readOnlyTools = map[string]bool{
	"read_file":                  true,
	"list_files":                 true,
	"search_files":               true,
	"list_code_definition_names": true,
}

// PruneStaleTools returns a copy of contents with superseded read-only tool
// call/response pairs removed. The original slice is never mutated.
//
// When GW_PRUNE_STALE_TOOLS is off this is a fast-path no-op.
func PruneStaleTools(contents []*genai.Content) []*genai.Content {
	if !pruneStaleTools || len(contents) < 3 {
		return contents
	}

	// 1. Index every clean read-only call turn by (toolName + argsKey) →
	//    sorted turn indices. A "clean" call turn has exactly one part: a
	//    FunctionCall for a read-only tool.
	type callRef struct {
		idx     int // model turn index holding the FunctionCall
		respIdx int // paired user FunctionResponse turn index, or -1
	}
	byKey := make(map[string][]callRef)
	for i, c := range contents {
		if c == nil || i == 0 {
			continue
		}
		name, key, ok := cleanReadOnlyCall(c)
		if !ok {
			continue
		}
		respIdx := pairedResponseIdx(contents, i, name)
		byKey[name+"\x00"+key] = append(byKey[name+"\x00"+key], callRef{idx: i, respIdx: respIdx})
	}

	// 2. For each key with ≥2 occurrences, mark all but the LAST for removal,
	//    along with their paired response turns.
	remove := make(map[int]bool)
	for _, refs := range byKey {
		if len(refs) < 2 {
			continue
		}
		// Keep the last (highest index); drop earlier ones.
		sort.Slice(refs, func(a, b int) bool { return refs[a].idx < refs[b].idx })
		for _, r := range refs[:len(refs)-1] {
			if r.respIdx < 0 {
				continue // unpaired call: leave it alone, can't drop cleanly
			}
			remove[r.idx] = true
			remove[r.respIdx] = true
		}
	}
	if len(remove) == 0 {
		return contents
	}

	out := make([]*genai.Content, 0, len(contents))
	for i, c := range contents {
		if remove[i] {
			if c != nil {
				logPruneTools.Debugf("dropped superseded read-only tool turn %d: role=%s", i, c.Role)
			}
			continue
		}
		out = append(out, c)
	}
	logPruneTools.Debugf("removed %d superseded read-only tool turn(s)", len(remove))
	return out
}

// cleanReadOnlyCall reports whether c is a single-part assistant turn holding
// exactly one read-only FunctionCall, returning the tool name and a stable
// key derived from its arguments. Any extra parts (text, second call) make it
// ineligible (ok == false) so we never prune a mixed-purpose turn.
func cleanReadOnlyCall(c *genai.Content) (name, argsKey string, ok bool) {
	if c == nil || len(c.Parts) != 1 {
		return "", "", false
	}
	p := c.Parts[0]
	if p == nil || p.FunctionCall == nil {
		return "", "", false
	}
	fc := p.FunctionCall
	if !readOnlyTools[fc.Name] {
		return "", "", false
	}
	return fc.Name, argsStableKey(fc.Args), true
}

// argsStableKey builds a deterministic key from a FunctionCall args map by
// sorting keys. Values are rendered with the default Go formatting via the
// caller; for our read-only tools the args are small flat string/number maps.
func argsStableKey(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b := make([]byte, 0, 64)
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, '=')
		b = appendValue(b, args[k])
		b = append(b, ';')
	}
	return string(b)
}

// appendValue renders a scalar arg value deterministically. Only the simple
// scalar shapes our read-only tools use are handled precisely; anything else
// falls back to a type-stable marker so distinct complex args don't collide
// into the same key (which would risk an unsafe prune).
func appendValue(b []byte, v any) []byte {
	switch t := v.(type) {
	case string:
		return append(b, t...)
	case bool:
		if t {
			return append(b, "true"...)
		}
		return append(b, "false"...)
	case float64:
		return strconv.AppendFloat(b, t, 'g', -1, 64)
	case nil:
		return append(b, "null"...)
	default:
		// Non-scalar (slice/map): use a non-matching unique-ish marker so it
		// never equals another arg set — prevents an unsafe collapse.
		return append(b, "\x01complex\x01"...)
	}
}

// pairedResponseIdx returns the index of the user FunctionResponse turn that
// immediately follows model call turn callIdx and answers the same tool, or
// -1 if the next turn isn't a clean matching single-part response.
func pairedResponseIdx(contents []*genai.Content, callIdx int, name string) int {
	j := callIdx + 1
	if j >= len(contents) {
		return -1
	}
	c := contents[j]
	if c == nil || len(c.Parts) != 1 {
		return -1
	}
	p := c.Parts[0]
	if p == nil || p.FunctionResponse == nil {
		return -1
	}
	if p.FunctionResponse.Name != name {
		return -1
	}
	return j
}

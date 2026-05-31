package pipeline

import (
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"sort"
	"strconv"
	"strings"

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

// toolExchange represents a single paired read-only tool call and response.
type toolExchange struct {
	callContentIdx int
	callPartIdx    int
	callName       string

	respContentIdx int
	respPartIdx    int
}

// PruneStaleTools returns a copy of contents with superseded read-only tool
// call/response parts replaced with lightweight placeholders. The original
// slice and its parts are never mutated.
//
// When GW_PRUNE_STALE_TOOLS is off this is a fast-path no-op.
func PruneStaleTools(contents []*genai.Content) []*genai.Content {
	if !pruneStaleTools || len(contents) < 3 {
		return contents
	}

	// 1. Find all paired read-only tool calls and their responses.
	exchangesByKey := make(map[string][]toolExchange)
	for i, c := range contents {
		if c == nil || i == 0 {
			continue
		}
		role := strings.ToLower(c.Role)
		if role != "model" && role != "assistant" {
			continue
		}
		// Look for read-only FunctionCalls inside this model turn.
		for pIdx, p := range c.Parts {
			if p == nil || p.FunctionCall == nil {
				continue
			}
			fc := p.FunctionCall
			if !readOnlyTools[fc.Name] {
				continue
			}
			// Find the paired response in the next turn (i+1).
			if i+1 >= len(contents) {
				break
			}
			nextC := contents[i+1]
			if nextC == nil {
				continue
			}
			nextRole := strings.ToLower(nextC.Role)
			if nextRole == "model" || nextRole == "assistant" {
				continue
			}
			// Search for a matching FunctionResponse in the next turn's parts.
			respPartIdx := -1
			for rIdx, rp := range nextC.Parts {
				if rp == nil || rp.FunctionResponse == nil {
					continue
				}
				fr := rp.FunctionResponse
				
				// Pair by ID first if both have non-empty IDs (highly robust for OpenAI/MaaS).
				if fc.ID != "" && fr.ID != "" {
					if fr.ID == fc.ID {
						respPartIdx = rIdx
						break
					}
					continue
				}
				
				// Fall back to name-based matching if IDs are missing (or either is empty).
				if fr.Name == fc.Name {
					respPartIdx = rIdx
					break
				}
			}
			if respPartIdx != -1 {
				key := fc.Name + "\x00" + argsStableKey(fc.Args)
				exchangesByKey[key] = append(exchangesByKey[key], toolExchange{
					callContentIdx: i,
					callPartIdx:    pIdx,
					callName:       fc.Name,
					respContentIdx: i + 1,
					respPartIdx:    respPartIdx,
				})
			}
		}
	}

	// 2. Identify which parts are superseded and mark them for replacement.
	type replacement struct {
		placeholder string
	}
	toReplace := make(map[int]map[int]replacement)
	for _, refs := range exchangesByKey {
		if len(refs) < 2 {
			continue
		}
		// Sort chronologically.
		sort.Slice(refs, func(a, b int) bool {
			return refs[a].callContentIdx < refs[b].callContentIdx
		})
		// Keep the last (newest); replace all earlier ones.
		for _, x := range refs[:len(refs)-1] {
			if toReplace[x.callContentIdx] == nil {
				toReplace[x.callContentIdx] = make(map[int]replacement)
			}
			toReplace[x.callContentIdx][x.callPartIdx] = replacement{
				placeholder: "(superseded " + x.callName + " call pruned)",
			}

			if toReplace[x.respContentIdx] == nil {
				toReplace[x.respContentIdx] = make(map[int]replacement)
			}
			toReplace[x.respContentIdx][x.respPartIdx] = replacement{
				placeholder: "(superseded " + x.callName + " output pruned)",
			}
		}
	}

	if len(toReplace) == 0 {
		return contents
	}

	// 3. Build a clean, unmutated copy of the contents with targeted placeholders.
	out := make([]*genai.Content, len(contents))
	replacedCount := 0
	totalSaved := 0
	for i, c := range contents {
		if c == nil {
			out[i] = nil
			continue
		}
		partsRepls, found := toReplace[i]
		if !found {
			out[i] = c
			continue
		}
		nc := &genai.Content{
			Role:  c.Role,
			Parts: make([]*genai.Part, len(c.Parts)),
		}
		for j, p := range c.Parts {
			if p == nil {
				nc.Parts[j] = nil
				continue
			}
			if repl, ok := partsRepls[j]; ok {
				nc.Parts[j] = &genai.Part{
					Text: repl.placeholder,
				}
				replacedCount++
				saved := partBytes(p) - len(repl.placeholder)
				if saved > 0 {
					totalSaved += saved
				}
				logPruneTools.Debugf("replaced superseded read-only tool part: turn=%d part=%d role=%s", i, j, c.Role)
			} else {
				nc.Parts[j] = p
			}
		}
		out[i] = nc
	}
	if replacedCount > 0 {
		logPruneTools.L().Debug("replaced superseded read-only tool call/response parts",
			slog.Int("replaced_count", replacedCount),
			slog.Int("bytes_saved", totalSaved),
		)
		onCompressionSaved("prune_tools", totalSaved)
	}
	return out
}

func partBytes(p *genai.Part) int {
	if p == nil {
		return 0
	}
	switch {
	case p.Text != "":
		return len(p.Text)
	case p.InlineData != nil:
		return len(p.InlineData.Data)
	case p.FunctionCall != nil:
		return 64
	case p.FunctionResponse != nil:
		sum := 0
		for _, v := range p.FunctionResponse.Response {
			if s, ok := v.(string); ok {
				sum += len(s)
			} else {
				sum += 16
			}
		}
		if sum == 0 {
			sum = 64
		}
		return sum
	default:
		return 0
	}
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

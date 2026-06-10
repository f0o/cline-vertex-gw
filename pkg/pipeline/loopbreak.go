package pipeline

import (
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logLoopbreak scopes pipeline-compression logs to component=loopbreak (DEBUG: per-request diagnostics).
var logLoopbreak = logx.Scoped("loopbreak")

// LLM loop-trap resolution knobs.
//
//   - GW_BREAK_LOOP_TRAP (default: on)
//     Master switch. Set to "0"/"false"/"off" to disable loop resolution.
//
//   - GW_LOOP_TRAP_NUDGE (default: on)
//     Enables appending a helpful, clear tool-use nudge to the latest user scolding turn.

// BreakLoopTrap processes the conversation history to detect and resolve
// LLM loop-traps caused by repetitive automated client scoldings
// and empty assistant responses.
func BreakLoopTrap(contents []*genai.Content) []*genai.Content {
	if !breakLoopTrapEnabled || len(contents) == 0 {
		return contents
	}

	// 1. Identify scolding turns
	lastNoToolIdx := -1
	lastTodoIdx := -1
	for i := 0; i < len(contents); i++ {
		c := contents[i]
		if c == nil {
			continue
		}
		isNoTool, isTodo := isScoldingTurn(c)
		if isNoTool {
			lastNoToolIdx = i
		}
		if isTodo {
			lastTodoIdx = i
		}
	}

	// If no scolding turns are found, nothing to do.
	if lastNoToolIdx == -1 && lastTodoIdx == -1 {
		return contents
	}

	// 2. Filter out duplicate scoldings and their trailing empty model turns.
	// To keep user/assistant alternation valid, if we drop a user turn we must also
	// drop one model turn. We drop the empty model turn that immediately follows the user turn.
	keep := make([]bool, len(contents))
	for i := range keep {
		keep[i] = true
	}

	droppedCount := 0
	for i := 0; i < len(contents); i++ {
		c := contents[i]
		if c == nil {
			continue
		}
		isNoTool, isTodo := isScoldingTurn(c)
		if !isNoTool && !isTodo {
			continue
		}

		// Check if this is an older duplicate scolding of its type
		isDupNoTool := isNoTool && i != lastNoToolIdx
		isDupTodo := isTodo && i != lastTodoIdx

		if isDupNoTool || isDupTodo {
			// Safety: Never drop the first user message, which contains initial instructions.
			if i == 0 {
				continue
			}

			// Safety: Only drop the subsequent model turn if it does not contain tool calls.
			// If it does, we must keep this scolding turn and the subsequent turn,
			// since they are part of a valid tool calling sequence.
			hasToolCall := false
			if i+1 < len(contents) && contents[i+1] != nil {
				for _, p := range contents[i+1].Parts {
					if p != nil && p.FunctionCall != nil {
						hasToolCall = true
						break
					}
				}
			}

			if !hasToolCall {
				keep[i] = false
				droppedCount++
				// Also drop the subsequent model turn to maintain alternation
				if i+1 < len(contents) {
					keep[i+1] = false
					droppedCount++
				}
			}
		}
	}

	totalSaved := 0
	var out []*genai.Content
	for i, k := range keep {
		if k {
			out = append(out, contents[i])
		} else if contents[i] != nil {
			totalSaved += contentBytes(contents[i])
			logLoopbreak.Debugf("dropped stale loop-trap turn %d: role=%s", i, contents[i].Role)
		}
	}

	if droppedCount > 0 {
		logLoopbreak.L().Debug("removed duplicate/empty loop-trap turns from history",
			slog.Int("dropped_count", droppedCount),
			slog.Int("bytes_saved", totalSaved),
		)
		if totalSaved > 0 {
			onCompressionSaved("loopbreak", totalSaved)
		}
	}

	// 3. Nudge the model on the last turn if it's a scolding turn
	if loopTrapNudgeEnabled && len(out) > 0 {
		lastTurn := out[len(out)-1]
		if isNoTool, isTodo := isScoldingTurn(lastTurn); isNoTool || isTodo {
			// Find the first text part and append nudge
			for _, p := range lastTurn.Parts {
				if p != nil && p.Text != "" {
					nudgeText := "\n\n" +
						"If you have no other tools to call, please execute 'pytest' in the terminal using the execute_command tool to verify the environment state and continue."
					// Check if we haven't already appended the nudge
					if !strings.Contains(p.Text, "If you have no other tools to call") {
						p.Text += nudgeText
						logLoopbreak.Debugf("appended loop-break action placeholder nudge to last user turn")
					}
					break
				}
			}
		}
	}

	return out
}

func isScoldingTurn(c *genai.Content) (isNoTool, isTodo bool) {
	if c == nil || (c.Role != genai.RoleUser && c.Role != "user") {
		return false, false
	}
	for _, p := range c.Parts {
		if p == nil {
			continue
		}
		// Safety: If the turn contains a function response, it's a tool output turn, NOT a scolding turn.
		if p.FunctionResponse != nil {
			return false, false
		}
		if p.Text == "" {
			continue
		}
		text := p.Text
		if strings.Contains(text, "You did not use a tool in your previous response!") || strings.Contains(text, "did not use a tool in your previous response") {
			isNoTool = true
		}
		if strings.Contains(text, "TODO LIST UPDATE REQUIRED") || strings.Contains(text, "You MUST include the task_progress parameter") {
			isTodo = true
		}
	}
	return isNoTool, isTodo
}

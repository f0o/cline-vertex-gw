package pipeline

import (
	"encoding/json"
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"strings"

	"google.golang.org/genai"
)

// logPruneActive scopes logs to component=active_tool_pruning.
var logPruneActive = logx.Scoped("active_tool_pruning")

// Configuration for Dynamic Active Tool Pruning.
// Default Whitelist includes only the absolute critical core system tools of Cline to maximize pruning utility.
var (
	activeToolPruningEnabled   = envBool("GW_ACTIVE_TOOL_PRUNING", false)
	activeToolPruningWindow    = envInt32("GW_ACTIVE_TOOL_PRUNING_WINDOW", 20)
	activeToolPruningWhitelist = envString("GW_ACTIVE_TOOL_PRUNING_WHITELIST", "write_to_file,replace_in_file,execute_command,read_file,ask_followup_question,attempt_completion,new_task")
)

// PruneActiveTools dynamically filters opts.Tools on the active turn.
// If enabled, it scans the history to see which tools have been called.
// If an auxiliary tool has been used in the past but has not been called in the
// last activeToolPruningWindow turns, it is dynamically stripped from the allowed
// active toolset. Essential core tools are whitelisted and never pruned.
func PruneActiveTools(contents []*genai.Content, opts *GenerationOptions) int {
	if !activeToolPruningEnabled || opts == nil || len(opts.Tools) == 0 || len(contents) <= int(activeToolPruningWindow) {
		return 0
	}

	// 1. Parse whitelist
	whitelist := strings.Split(strings.ToLower(activeToolPruningWhitelist), ",")
	isImmune := func(name string) bool {
		nameLower := strings.ToLower(name)
		for _, w := range whitelist {
			w = strings.TrimSpace(w)
			if w != "" && (strings.Contains(nameLower, w) || strings.Contains(w, nameLower)) {
				return true
			}
		}
		return false
	}

	// 2. Scan history for tool invocations
	usedTools := make(map[string]bool)
	recentlyUsedTools := make(map[string]bool)
	recentWindowLimit := len(contents) - int(activeToolPruningWindow)

	for i, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			var name string
			if p.FunctionCall != nil {
				name = p.FunctionCall.Name
			} else if p.FunctionResponse != nil {
				name = p.FunctionResponse.Name
			}

			if name != "" {
				usedTools[name] = true
				if i >= recentWindowLimit {
					recentlyUsedTools[name] = true
				}
			}
		}
	}

	// 3. Filter active tools
	var evaluated []string
	var pruned []string
	var kept []*genai.Tool
	var bytesSaved int

	for _, t := range opts.Tools {
		if t == nil {
			continue
		}
		// A tool is kept if it has no function declarations (e.g. built-ins)
		// or if any of its function declarations are immune, or have been used recently,
		// or have NEVER been used (so the agent can invoke them for the first time).
		keepTool := true
		for _, fd := range t.FunctionDeclarations {
			if fd == nil || fd.Name == "" {
				continue
			}
			name := fd.Name
			evaluated = append(evaluated, name)

			// Prune rule:
			// Must have been used in the past AND not used recently AND not immune.
			hasBeenUsed := usedTools[name]
			isRecent := recentlyUsedTools[name]
			if hasBeenUsed && !isRecent && !isImmune(name) {
				keepTool = false
				pruned = append(pruned, name)
				break
			}
		}

		if keepTool {
			kept = append(kept, t)
		} else {
			// Estimate bytes saved by JSON-marshaling the pruned tool.
			if b, err := json.Marshal(t); err == nil {
				bytesSaved += len(b)
			} else {
				// Fallback approximation: estimate based on Name and field length if marshaling fails
				bytesSaved += 500
			}
		}
	}

	if len(pruned) > 0 {
		logPruneActive.L().Debug(fmt.Sprintf("Evaluated Tools: [%s], Selected for Pruning: [%s]",
			strings.Join(evaluated, ", "),
			strings.Join(pruned, ", ")),
		)
		logPruneActive.L().Debug("dynamically pruned cold auxiliary tool(s) from active turn",
			slog.Int("bytes_saved", bytesSaved),
		)
		onCompressionSaved("active_tool_pruning", bytesSaved)
		opts.Tools = kept
	}

	return len(pruned)
}

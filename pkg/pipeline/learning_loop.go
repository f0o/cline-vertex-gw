package pipeline

import (
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"google.golang.org/genai"
)

var (
	logLearningLoop = logx.Scoped("learning_loop")
	learningMutex   sync.Mutex
)

// RecordLoopFailure analyzes scoldings and empty turns in a loop trap and appends
// corrective lessons to a persistent .clinerules-learned.md file to self-heal the agent.
func RecordLoopFailure(contents []*genai.Content) {
	if !learningLoopEnabled {
		return
	}

	learningMutex.Lock()
	defer learningMutex.Unlock()

	// Parse scoldings and find loops
	var loops []string
	for _, c := range contents {
		if c == nil {
			continue
		}
		isNoTool, isTodo := isScoldingTurn(c)
		if isNoTool || isTodo {
			for _, p := range c.Parts {
				if p != nil && p.Text != "" {
					trimmed := strings.TrimSpace(p.Text)
					if trimmed != "" && len(trimmed) < 256 {
						loops = append(loops, trimmed)
					}
				}
			}
		}
	}

	if len(loops) == 0 {
		return
	}

	// Dedup and extract unique loops
	uniqueLoops := make(map[string]bool)
	var lessons []string
	for _, lp := range loops {
		if !uniqueLoops[lp] {
			uniqueLoops[lp] = true
			// Formulate a simple rule from the scolding text
			lesson := parseScoldingToRule(lp)
			if lesson != "" {
				lessons = append(lessons, lesson)
			}
		}
	}

	if len(lessons) == 0 {
		return
	}

	// Write to .clinerules-learned.md
	filePath := filepath.Join(".", ".clinerules-learned.md")
	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		logLearningLoop.Errorf("failed to open .clinerules-learned.md: %v", err)
		return
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	header := fmt.Sprintf("\n# Self-Healed Learning Log - %s\n", timestamp)
	if _, err := f.WriteString(header); err != nil {
		logLearningLoop.Errorf("failed to write header to .clinerules-learned.md: %v", err)
		return
	}

	for _, lesson := range lessons {
		entry := fmt.Sprintf("- [Learned Rule] %s\n", lesson)
		if _, err := f.WriteString(entry); err != nil {
			logLearningLoop.Errorf("failed to write lesson to .clinerules-learned.md: %v", err)
			return
		}
		logLearningLoop.Infof("recorded persistent self-healed rule: %s", lesson)
	}
}

// parseScoldingToRule translates scolding messages into structured operational guidelines.
func parseScoldingToRule(scolding string) string {
	lower := strings.ToLower(scolding)
	if strings.Contains(lower, "no tool call") || strings.Contains(lower, "did not call a tool") || strings.Contains(lower, "did not use a tool") {
		return "When given tasks, always call relevant tools instead of outputting conversational text alone."
	}
	if strings.Contains(lower, "stuck") || strings.Contains(lower, "loop") {
		return "If you find yourself stuck in a loop repeating actions, pivot to checking environment state or logs."
	}
	if strings.Contains(lower, "todo") || strings.Contains(lower, "checklist") {
		return "Make sure to update the task checklists and tick off completed subtasks to maintain alignment."
	}
	if strings.Contains(lower, "error") || strings.Contains(lower, "failed") {
		return "If a shell command fails, inspect the configuration files or error traces first before retrying the same command."
	}

	// Return a sanitized version of the scolding itself as a default rule
	sanitized := strings.ReplaceAll(scolding, "\n", " ")
	if len(sanitized) > 100 {
		sanitized = sanitized[:97] + "..."
	}
	return fmt.Sprintf("Avoid triggering the scolding condition: '%s'", sanitized)
}

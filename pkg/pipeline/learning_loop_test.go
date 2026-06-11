package pipeline

import (
	"os"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func TestRecordLoopFailure(t *testing.T) {
	learningLoopEnabled = true

	// Ensure the file doesn't exist before test
	learnedFilePath := ".clinerules-learned.md"
	_ = os.Remove(learnedFilePath)
	defer func() {
		_ = os.Remove(learnedFilePath)
	}()

	// Mock scolding content
	contents := []*genai.Content{
		{
			Role: "user",
			Parts: []*genai.Part{
				{
					Text: "You did not use a tool in your previous response! Please call a tool.",
				},
			},
		},
	}

	RecordLoopFailure(contents)

	// Verify file was created
	if _, err := os.Stat(learnedFilePath); os.IsNotExist(err) {
		t.Fatalf("expected %s to be created, but it does not exist", learnedFilePath)
	}

	// Read content and check for expected rules
	data, err := os.ReadFile(learnedFilePath)
	if err != nil {
		t.Fatalf("failed to read %s: %v", learnedFilePath, err)
	}

	contentStr := string(data)
	if !strings.Contains(contentStr, "When given tasks, always call relevant tools") {
		t.Errorf("expected file to contain learned tool-use rule, got: %s", contentStr)
	}
}

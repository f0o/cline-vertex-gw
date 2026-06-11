package cache

import (
	"testing"
)

func TestSharedContextSwarmMemory(t *testing.T) {
	ClearSharedContext()

	content := "This is some repetitive multi-agent file reading content that should be shared across the swarm"
	hash := StoreSharedContext(content)

	if hash == "" {
		t.Fatalf("expected hash to be generated, got empty string")
	}

	retrieved, exists := RetrieveSharedContext(hash)
	if !exists {
		t.Errorf("expected hash %s to be found in shared context", hash)
	}

	if retrieved != content {
		t.Errorf("expected retrieved content to match stored content, got: %s", retrieved)
	}

	// Test clearing
	ClearSharedContext()
	_, existsAfterClear := RetrieveSharedContext(hash)
	if existsAfterClear {
		t.Errorf("expected hash to be removed after clearing")
	}
}

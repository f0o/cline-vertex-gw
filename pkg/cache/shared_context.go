package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

var (
	sharedContextMap = make(map[string]string)
	sharedContextMu  sync.RWMutex
)

// StoreSharedContext hashes content with SHA-256, stores it in the global,
// thread-safe in-memory swarm cache, and returns the hex SHA-256 hash.
func StoreSharedContext(content string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(content))
	hash := hex.EncodeToString(hasher.Sum(nil))

	sharedContextMu.Lock()
	sharedContextMap[hash] = content
	sharedContextMu.Unlock()

	return hash
}

// RetrieveSharedContext looks up the original uncompressed content of a shared
// swarm context using its hex SHA-256 hash. Reports whether the hash was found.
func RetrieveSharedContext(hash string) (string, bool) {
	sharedContextMu.RLock()
	content, exists := sharedContextMap[hash]
	sharedContextMu.RUnlock()

	return content, exists
}

// ClearSharedContext clears all stored shared context data to free memory.
func ClearSharedContext() {
	sharedContextMu.Lock()
	sharedContextMap = make(map[string]string)
	sharedContextMu.Unlock()
}

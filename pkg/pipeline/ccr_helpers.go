package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"go.f0o.dev/cline-vertex-gw/pkg/cache"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"google.golang.org/genai"
	"io"
	"strings"
)

var logCCR = logx.Scoped("ccr")

// SaveToElidedCache hashes s, saves it to the FSCache, and returns the hex SHA-256 hash.
func SaveToElidedCache(s string) string {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, s)
	sum := hasher.Sum(nil)
	hash := hex.EncodeToString(sum)

	fileName := "elided_" + hash + ".json"
	if err := cache.WriteFSCache(fileName, s); err != nil {
		logCCR.Errorf("failed to save elided content to cache: %v", err)
	}
	return hash
}

// InjectRetrieveElidedContentTool scans the final contents for elided content placeholder hashes
// and dynamically appends the retrieve_elided_content tool definition to opts.Tools.
func InjectRetrieveElidedContentTool(contents []*genai.Content, opts *GenerationOptions) {
	if opts == nil {
		logCCR.Debugf("GenerationOptions are nil; skipping retrieve_elided_content tool injection")
		return
	}

	// Scan contents to check if any elided placeholders are present
	hasElided := false
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			// Search for placeholder marker
			if p.Text != "" && strings.Contains(p.Text, "Retrieve full content: hash=") {
				hasElided = true
				break
			}
			if p.FunctionResponse != nil {
				for _, v := range p.FunctionResponse.Response {
					if s, ok := v.(string); ok && strings.Contains(s, "Retrieve full content: hash=") {
						hasElided = true
						break
					}
				}
			}
			if p.FunctionCall != nil {
				for _, v := range p.FunctionCall.Args {
					if s, ok := v.(string); ok && strings.Contains(s, "Retrieve full content: hash=") {
						hasElided = true
						break
					}
				}
			}
		}
		if hasElided {
			break
		}
	}

	if !hasElided {
		logCCR.Debugf("no elided content placeholders found; skipping retrieve_elided_content tool injection")
		return
	}

	// Check if tool is already present
	for _, t := range opts.Tools {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd != nil && fd.Name == "retrieve_elided_content" {
				logCCR.Debugf("retrieve_elided_content tool is already present; skipping injection")
				return
			}
		}
	}

	// Inject retrieve_elided_content tool definition
	toolDef := &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{{
			Name:        "retrieve_elided_content",
			Description: "Retrieve the full, uncompacted content of a previously truncated/elided historical turn or tool response using its cryptographic lookup hash.",
			ParametersJsonSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"hash": map[string]any{
						"type":        "string",
						"description": "The cryptographic SHA-256 lookup hash from the elision placeholder (e.g. from hash=abcdef...)",
					},
				},
				"required": []string{"hash"},
			},
		}},
	}

	opts.Tools = append(opts.Tools, toolDef)
	logCCR.Debugf("dynamically injected retrieve_elided_content tool into active turn")
}

// HasRetrievalToolCall scans contents to see if any retrieval tool calls or responses are present.
func HasRetrievalToolCall(contents []*genai.Content) bool {
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil && p.FunctionCall.Name == "retrieve_elided_content" {
				return true
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == "retrieve_elided_content" {
				return true
			}
		}
	}
	return false
}

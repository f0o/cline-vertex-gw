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
func SaveToElidedCache(s string) (string, error) {
	hasher := sha256.New()
	_, _ = io.WriteString(hasher, s)
	sum := hasher.Sum(nil)
	hash := hex.EncodeToString(sum)

	fileName := "elided_" + hash + ".json"
	if err := cache.WriteFSCache(fileName, s); err != nil {
		logCCR.Errorf("failed to save elided content to cache: %v", err)
		return "", err
	}
	return hash, nil
}

// InjectElisionPromptHint appends a strict guideline to the system prompt
// if any elided history is detected in contents, teaching the model how CCR works.
func InjectElisionPromptHint(systemPrompt string, contents []*genai.Content) string {
	hasElided := false
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
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
		return systemPrompt
	}

	hint := "\n\n[PROMPT COMPRESSION NOTICE: Some historical file-write and edit payloads have been elided to save context window tokens. They are represented in your history by placeholders containing a unique SHA-256 hash, for example: `[Content: <N> bytes written. Elided. Retrieve full content: hash=<hash>]`. If you ever need to view, modify, or rewrite these files, you must first call the `retrieve_elided_content` tool with the exact hash or read the file from the workspace using the `read_file` tool. Do NOT output the placeholder literal or its hash format in your own tool call arguments; instead, use `retrieve_elided_content` or `read_file` to get the actual raw content first.]"
	if !strings.Contains(systemPrompt, "[PROMPT COMPRESSION NOTICE") {
		return systemPrompt + hint
	}
	return systemPrompt
}

// RestoreOutboundToolCallPlaceholders intercepts model-generated tool calls for file writing
// and transparently restores original content from the cache if they contain a placeholder hash.
func RestoreOutboundToolCallPlaceholders(part *genai.Part) {
	if part == nil || part.FunctionCall == nil {
		return
	}
	fc := part.FunctionCall
	if fc.Name != "write_to_file" && fc.Name != "replace_in_file" {
		return
	}
	argKey := "content"
	if fc.Name == "replace_in_file" {
		argKey = "diff"
	}
	val, ok := fc.Args[argKey]
	if !ok {
		return
	}
	str, ok := val.(string)
	if !ok {
		return
	}

	if !strings.Contains(str, "Retrieve full content: hash=") {
		return
	}

	idx := strings.Index(str, "Retrieve full content: hash=")
	if idx == -1 {
		return
	}
	hashPart := str[idx+len("Retrieve full content: hash="):]
	if len(hashPart) < 64 {
		return
	}
	hash := hashPart[:64]

	var rawContent string
	fileName := "elided_" + hash + ".json"
	// Read original content from disk cache (ignores freshness TTL so we can always retrieve it)
	_, err := cache.ReadFSCache(fileName, &rawContent)
	if err == nil && rawContent != "" {
		fc.Args[argKey] = rawContent
		logCCR.Infof("automatically intercepted and restored placeholder in outbound tool-call %s using hash %s", fc.Name, hash)
	} else {
		logCCR.Warnf("failed to restore placeholder in outbound tool-call %s: cache lookup failed for hash %s (err=%v)", fc.Name, hash, err)
	}
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

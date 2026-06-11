package pipeline

import (
	"strings"

	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"google.golang.org/genai"
)

// logAlign scopes pipeline-compression logs to component=align.
var logAlign = logx.Scoped("align")

// AlignFunctionCallsAndResponses ensures that for every "model" turn with tool calls,
// the subsequent "user" turn has exactly matching FunctionResponse parts.
// This is required by Gemini/Vertex AI, which throws a 400 INVALID_ARGUMENT error
// if the number of function responses doesn't match the preceding function calls.
//
// When a mismatch is found (e.g. the user's next turn is missing some or all responses,
// or has extra responses), it:
//  1. Preserves all non-response parts (Text, InlineData, etc.).
//  2. Aligns responses by matching them to calls by ID, falling back to Name.
//  3. Synthesizes dummy responses with "omitted by client" for any unanswered calls.
//  4. Drops any unsolicited/extra responses that do not correspond to any call.
//
// It returns a new contents slice without mutating the input contents or their parts.
// isPrunedPlaceholder reports whether a part is a placeholder text
// introduced by PruneStaleTools for a pruned call or output.
func isPrunedPlaceholder(p *genai.Part) bool {
	if p == nil || p.Text == "" {
		return false
	}
	return strings.HasPrefix(p.Text, "(superseded ") && strings.HasSuffix(p.Text, " pruned)")
}

// extractToolNameFromPlaceholder parses the tool name from a pruned call placeholder.
func extractToolNameFromPlaceholder(s string) string {
	if !strings.HasPrefix(s, "(superseded ") || !strings.HasSuffix(s, " call pruned)") {
		return "tool"
	}
	t := strings.TrimPrefix(s, "(superseded ")
	t = strings.TrimSuffix(t, " call pruned)")
	return t
}

// AlignFunctionCallsAndResponses ensures that for every "model" turn with tool calls,
// the subsequent "user" turn has exactly matching FunctionResponse parts.
// This is required by Gemini/Vertex AI, which throws a 400 INVALID_ARGUMENT error
// if the number of function responses doesn't match the preceding function calls.
//
// When a mismatch is found (e.g. the user's next turn is missing some or all responses,
// or has extra responses), it:
//  1. Preserves all non-response parts (Text, InlineData, etc.).
//  2. Aligns responses by matching them to calls by ID, falling back to Name.
//  3. Synthesizes dummy responses with "omitted by client" for any unanswered calls.
//  4. Drops any unsolicited/extra responses that do not correspond to any call.
//
// It returns a new contents slice without mutating the input contents or their parts.
func AlignFunctionCallsAndResponses(contents []*genai.Content) []*genai.Content {
	if len(contents) == 0 {
		logAlign.Debugf("contents are empty; skipping alignment")
		return contents
	}

	out := make([]*genai.Content, len(contents))
	copy(out, contents)

	for i := 0; i < len(out); i++ {
		c := out[i]
		if c == nil {
			continue
		}

		// Look for a model turn containing function calls or pruned placeholders
		role := strings.ToLower(c.Role)
		if role != "model" && role != "assistant" {
			continue
		}

		var hasFC bool
		for _, p := range c.Parts {
			if p != nil && (p.FunctionCall != nil || isPrunedPlaceholder(p)) {
				hasFC = true
				break
			}
		}

		if !hasFC {
			continue
		}

		logAlign.Debugf("aligning function calls and responses for turn %d...", i)

		// If there is no next turn, we can't align. Vertex AI will reject it anyway
		// if the conversation doesn't alternate properly, but we preserve as-is.
		if i+1 >= len(out) {
			continue
		}

		nextC := out[i+1]
		if nextC == nil {
			continue
		}

		// The next turn must be a "user" turn
		nextRole := strings.ToLower(nextC.Role)
		if nextRole == "model" || nextRole == "assistant" {
			continue
		}

		// Collect all available responses, regular user text parts, and pruned placeholders
		var availableResponses []*genai.FunctionResponse
		var userTextParts []*genai.Part
		var placeholderParts []*genai.Part

		for _, p := range nextC.Parts {
			if p == nil {
				continue
			}
			if p.FunctionResponse != nil {
				availableResponses = append(availableResponses, p.FunctionResponse)
			} else if isPrunedPlaceholder(p) {
				placeholderParts = append(placeholderParts, p)
			} else {
				userTextParts = append(userTextParts, p)
			}
		}

		// Align parts to match the exact set of parts in the model turn position-by-position.
		var alignedParts []*genai.Part

		// Regular user text parts are placed first at the front of the turn (preserves user turn text).
		alignedParts = append(alignedParts, userTextParts...)

		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil {
				fc := p.FunctionCall
				var matchedResponse *genai.FunctionResponse
				matchedIdx := -1

				// 1. Try matching by ID first (only if fc.ID is non-empty)
				if fc.ID != "" {
					for idx, fr := range availableResponses {
						if fr.ID == fc.ID {
							matchedResponse = fr
							matchedIdx = idx
							break
						}
					}
				}

				// 2. Fall back to matching by Name
				if matchedResponse == nil {
					for idx, fr := range availableResponses {
						if fr.Name == fc.Name {
							matchedResponse = fr
							matchedIdx = idx
							break
						}
					}
				}

				if matchedResponse != nil {
					// Remove from availableResponses to prevent reuse
					availableResponses = append(availableResponses[:matchedIdx], availableResponses[matchedIdx+1:]...)
					alignedParts = append(alignedParts, &genai.Part{
						FunctionResponse: matchedResponse,
					})
				} else {
					// Synthesize dummy FunctionResponse
					logAlign.Debugf("synthesized dummy response for missing function call response: name=%s ID=%s in next turn %d", fc.Name, fc.ID, i+1)
					alignedParts = append(alignedParts, &genai.Part{
						FunctionResponse: &genai.FunctionResponse{
							ID:       fc.ID,
							Name:     fc.Name,
							Response: map[string]any{"output": "omitted by client"},
						},
					})
				}
			} else if isPrunedPlaceholder(p) {
				// Model turn has a pruned call placeholder.
				// Match it to a response placeholder in placeholderParts, or synthesize if none left.
				if len(placeholderParts) > 0 {
					alignedParts = append(alignedParts, placeholderParts[0])
					placeholderParts = placeholderParts[1:]
				} else {
					toolName := extractToolNameFromPlaceholder(p.Text)
					alignedParts = append(alignedParts, &genai.Part{
						Text: "(superseded " + toolName + " output pruned)",
					})
				}
			}
		}

		// Note: Unsolicited responses in availableResponses are deliberately dropped (not appended).
		// Leftover placeholder parts are also dropped as they are unsolicited.
		if len(availableResponses) > 0 {
			logAlign.Debugf("dropped %d unsolicited function response(s) in turn %d", len(availableResponses), i+1)
		}
		if len(placeholderParts) > 0 {
			logAlign.Debugf("dropped %d leftover pruned placeholder part(s) in turn %d", len(placeholderParts), i+1)
		}

		// Filter out nil parts to keep it clean
		var cleanAlignedParts []*genai.Part
		for _, p := range alignedParts {
			if p != nil {
				cleanAlignedParts = append(cleanAlignedParts, p)
			}
		}

		newNextC := &genai.Content{
			Role:  nextC.Role,
			Parts: cleanAlignedParts,
		}
		out[i+1] = newNextC
	}

	return out
}

package provider

import (
	"strings"

	"google.golang.org/genai"
)

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
		return contents
	}

	out := make([]*genai.Content, len(contents))
	copy(out, contents)

	for i := 0; i < len(out); i++ {
		c := out[i]
		if c == nil {
			continue
		}

		// Look for a model turn containing function calls
		role := strings.ToLower(c.Role)
		if role != "model" && role != "assistant" {
			continue
		}

		var callParts []*genai.FunctionCall
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil {
				callParts = append(callParts, p.FunctionCall)
			}
		}

		if len(callParts) == 0 {
			continue
		}

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

		// Collect all available responses and non-response parts
		var availableResponses []*genai.FunctionResponse
		var otherParts []*genai.Part

		for _, p := range nextC.Parts {
			if p == nil {
				continue
			}
			if p.FunctionResponse != nil {
				availableResponses = append(availableResponses, p.FunctionResponse)
			} else {
				otherParts = append(otherParts, p)
			}
		}

		// Align responses to match the exact set of calls in the model turn
		var alignedResponseParts []*genai.Part
		for _, fc := range callParts {
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

			// 2. Fall back to matching by Name (only if not already matched)
			if matchedResponse == nil {
				for idx, fr := range availableResponses {
					if fr.Name == fc.Name {
						matchedResponse = fr
						matchedIdx = idx
						break
					}
				}
			}

			// If matched, remove it from availableResponses so it's not reused
			if matchedResponse != nil {
				availableResponses = append(availableResponses[:matchedIdx], availableResponses[matchedIdx+1:]...)
				alignedResponseParts = append(alignedResponseParts, &genai.Part{
					FunctionResponse: matchedResponse,
				})
			} else {
				// 3. Synthesize dummy FunctionResponse for missing response
				alignedResponseParts = append(alignedResponseParts, &genai.Part{
					FunctionResponse: &genai.FunctionResponse{
						ID:       fc.ID,
						Name:     fc.Name,
						Response: map[string]any{"output": "omitted by client"},
					},
				})
			}
		}

		// Replace next turn with a new Content object containing preserved otherParts
		// plus our perfectly-aligned and ordered FunctionResponse parts.
		newNextC := &genai.Content{
			Role:  nextC.Role,
			Parts: append(otherParts, alignedResponseParts...),
		}
		out[i+1] = newNextC
	}

	return out
}

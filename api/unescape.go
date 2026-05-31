package api

import (
	"html"
	"strings"
)

// Configuration for unescaping Google/Gemini streaming responses.
// Enabled by default; set GW_GEMINI_UNESCAPE=off to disable.
var geminiUnescapeEnabled = envBoolAPI("GW_GEMINI_UNESCAPE", true)

type unescapeState int

const (
	stateNormal unescapeState = iota
	stateBufferingEntity
	stateBufferingTag
	stateBufferingTagEntity
)

// Cline XML tags that we care about unescaping.
var clineTags = map[string]bool{
	"execute_command":            true,
	"read_file":                  true,
	"write_to_file":              true,
	"replace_in_file":            true,
	"search_files":               true,
	"list_files":                 true,
	"list_code_definition_names": true,
	"browser_action":             true,
	"use_mcp_tool":               true,
	"access_mcp_resource":        true,
	"ask_followup_question":      true,
	"attempt_completion":         true,
	"generate_explanation":       true,
	"thought":                    true,
	"thinking":                   true,

	"command":       true,
	"path":          true,
	"content":       true,
	"diff":          true,
	"regex":         true,
	"file_pattern":  true,
	"recursive":     true,
	"server_name":   true,
	"tool_name":     true,
	"arguments":     true,
	"uri":           true,
	"question":      true,
	"options":       true,
	"result":        true,
	"task_progress": true,
	"explanation":   true,
	"from_ref":      true,
	"to_ref":        true,
	"title":         true,
	"action":        true,
	"url":           true,
	"coordinate":    true,
	"text":          true,
}

// Tool/block tags that control the "in tool block" context for unescaping characters like &
var toolBlockTags = map[string]bool{
	"execute_command":            true,
	"read_file":                  true,
	"write_to_file":              true,
	"replace_in_file":            true,
	"search_files":               true,
	"list_files":                 true,
	"list_code_definition_names": true,
	"browser_action":             true,
	"use_mcp_tool":               true,
	"access_mcp_resource":        true,
	"ask_followup_question":      true,
	"attempt_completion":         true,
	"generate_explanation":       true,
	"thought":                    true,
	"thinking":                   true,
}

// EntityUnescaper is a stateful selective HTML entity unescaper.
// It parses the reply on-the-fly and selectively unescapes XML tags and parameters
// used by Cline, as well as entities like &, ", &#39; when inside a tool block.
type EntityUnescaper struct {
	state          unescapeState
	entityBuf      []rune // buffers active entity in normal state
	tagRawBuf      []rune // buffers raw tag characters as they arrived
	tagParsedBuf   []rune // buffers parsed/unescaped tag characters
	tagEntityBuf   []rune // buffers active entity inside tag buffering
	toolBlockCount int    // tracks active nested tool/thinking blocks
}

func NewEntityUnescaper() *EntityUnescaper {
	return &EntityUnescaper{
		state: stateNormal,
	}
}

// Process takes a chunk of text, processes it, and returns the selectively unescaped text.
func (u *EntityUnescaper) Process(chunk string) string {
	if !geminiUnescapeEnabled {
		return chunk
	}
	var out strings.Builder

	for _, r := range chunk {
		u.processRune(r, &out)
	}

	return out.String()
}

// Flush outputs any buffered characters at the end of the stream.
func (u *EntityUnescaper) Flush() string {
	if !geminiUnescapeEnabled {
		return ""
	}
	var out strings.Builder
	if len(u.entityBuf) > 0 {
		out.WriteString(string(u.entityBuf))
		u.entityBuf = nil
	}
	if len(u.tagEntityBuf) > 0 {
		u.tagRawBuf = append(u.tagRawBuf, u.tagEntityBuf...)
		u.tagEntityBuf = nil
	}
	if len(u.tagRawBuf) > 0 {
		out.WriteString(string(u.tagRawBuf))
		u.tagRawBuf = nil
	}
	u.state = stateNormal
	return out.String()
}

func (u *EntityUnescaper) processRune(r rune, out *strings.Builder) {
	switch u.state {
	case stateNormal:
		if r == '&' {
			u.state = stateBufferingEntity
			u.entityBuf = []rune{r}
		} else if r == '<' {
			u.state = stateBufferingTag
			u.tagRawBuf = []rune{r}
			u.tagParsedBuf = []rune{r}
		} else {
			out.WriteRune(r)
		}

	case stateBufferingEntity:
		if r == '&' {
			// Flush existing entity raw and start a new one
			out.WriteString(string(u.entityBuf))
			u.entityBuf = []rune{r}
		} else if r == ';' {
			u.entityBuf = append(u.entityBuf, r)
			rawEntity := string(u.entityBuf)
			decoded := html.UnescapeString(rawEntity)
			u.entityBuf = nil

			if decoded == "<" {
				// Potential start of a Cline XML tag
				u.state = stateBufferingTag
				u.tagRawBuf = []rune(rawEntity)
				u.tagParsedBuf = []rune("<")
			} else if decoded == ">" {
				// Lone > outside of a tag buffer, output raw
				out.WriteString(rawEntity)
				u.state = stateNormal
			} else {
				// Other entity like &, ", &#39;
				if u.toolBlockCount > 0 {
					out.WriteString(decoded)
				} else {
					out.WriteString(rawEntity)
				}
				u.state = stateNormal
			}
		} else if isEntityChar(r) && len(u.entityBuf) < 10 {
			u.entityBuf = append(u.entityBuf, r)
		} else {
			// Invalid entity character or too long. Flush raw.
			out.WriteString(string(u.entityBuf))
			out.WriteRune(r)
			u.entityBuf = nil
			u.state = stateNormal
		}

	case stateBufferingTag:
		if r == '>' {
			u.tagRawBuf = append(u.tagRawBuf, r)
			u.tagParsedBuf = append(u.tagParsedBuf, r)
			u.resolveBufferedTag(out)
		} else if r == '&' {
			u.state = stateBufferingTagEntity
			u.tagEntityBuf = []rune{r}
		} else if isTagChar(r) && len(u.tagRawBuf) < 40 {
			u.tagRawBuf = append(u.tagRawBuf, r)
			u.tagParsedBuf = append(u.tagParsedBuf, r)
		} else {
			// Invalid character for a tag name or too long. Abort buffering and flush raw.
			out.WriteString(string(u.tagRawBuf))
			out.WriteRune(r)
			u.tagRawBuf = nil
			u.tagParsedBuf = nil
			u.state = stateNormal
		}

	case stateBufferingTagEntity:
		if r == '&' {
			// Flush incomplete entity to raw/parsed tag buffers, start new entity buffering
			u.tagRawBuf = append(u.tagRawBuf, u.tagEntityBuf...)
			u.tagParsedBuf = append(u.tagParsedBuf, u.tagEntityBuf...)
			u.tagEntityBuf = []rune{r}
		} else if r == ';' {
			u.tagEntityBuf = append(u.tagEntityBuf, r)
			rawEntity := string(u.tagEntityBuf)
			decoded := html.UnescapeString(rawEntity)
			u.tagEntityBuf = nil

			if decoded == ">" {
				// Completed closing of the tag (e.g. >)
				u.tagRawBuf = append(u.tagRawBuf, []rune(rawEntity)...)
				u.tagParsedBuf = append(u.tagParsedBuf, '>')
				u.resolveBufferedTag(out)
			} else {
				// Other entity inside a tag (invalid for tag names, but we accumulate it)
				u.tagRawBuf = append(u.tagRawBuf, []rune(rawEntity)...)
				u.tagParsedBuf = append(u.tagParsedBuf, []rune(rawEntity)...)
				u.state = stateBufferingTag
			}
		} else if isEntityChar(r) && len(u.tagEntityBuf) < 10 {
			u.tagEntityBuf = append(u.tagEntityBuf, r)
		} else {
			// Invalid entity. Append everything back to tag buffers.
			u.tagRawBuf = append(u.tagRawBuf, u.tagEntityBuf...)
			u.tagParsedBuf = append(u.tagParsedBuf, u.tagEntityBuf...)
			u.tagRawBuf = append(u.tagRawBuf, r)
			u.tagParsedBuf = append(u.tagParsedBuf, r)
			u.tagEntityBuf = nil
			u.state = stateBufferingTag
		}
	}
}

// resolveBufferedTag is called when we hit a '>' (or decoded '>') while in stateBufferingTag.
func (u *EntityUnescaper) resolveBufferedTag(out *strings.Builder) {
	parsedStr := string(u.tagParsedBuf)
	rawStr := string(u.tagRawBuf)

	u.tagParsedBuf = nil
	u.tagRawBuf = nil
	u.state = stateNormal

	tagName, isClose, ok := parseClineTagName(parsedStr)
	if ok {
		// Valid Cline tag! Output the parsed (unescaped) tag
		out.WriteString(parsedStr)

		// If this is a tool/block tag, update our state count
		if toolBlockTags[tagName] {
			if isClose {
				u.toolBlockCount--
				if u.toolBlockCount < 0 {
					u.toolBlockCount = 0
				}
			} else {
				u.toolBlockCount++
			}
		}
	} else {
		// Not a Cline tag. Output the raw (escaped) tag
		out.WriteString(rawStr)
	}
}

// parseClineTagName parses tag name, close indicator and validates against clineTags.
func parseClineTagName(s string) (name string, isClose bool, ok bool) {
	if !strings.HasPrefix(s, "<") || !strings.HasSuffix(s, ">") {
		return "", false, false
	}
	content := s[1 : len(s)-1]
	if strings.HasPrefix(content, "/") {
		isClose = true
		content = content[1:]
	}
	if clineTags[content] {
		return content, isClose, true
	}
	return "", false, false
}

func isEntityChar(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '#' || r == 'x' || r == 'X'
}

func isTagChar(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '_' || r == '/'
}

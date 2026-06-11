package pipeline

import (
	"fmt"
	"go.f0o.dev/cline-vertex-gw/pkg/logx"
	"log/slog"
	"regexp"
	"strings"
)

var logSyntax = logx.Scoped("syntax_compressor")

// syntaxCompressorMinBytes is the minimum size of a code block to be eligible for syntax compression.
const syntaxCompressorMinBytes = 64

// CompressCodeBlocks scans a string for Markdown code blocks (e.g. ```go ... ```)
// and applies language-specific syntax compression on historical/cold code blocks.
// This is lossless because the original uncompressed text block is saved to FSCache first.
func CompressCodeBlocks(s string) (string, int) {
	if !syntaxCompressorEnabled {
		return s, 0
	}
	if len(s) < syntaxCompressorMinBytes {
		return s, 0
	}

	// Pattern to match markdown code blocks
	codeBlockRegex := regexp.MustCompile("(?s)```(\\w*)\\n(.*?)\\n```")
	matches := codeBlockRegex.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		// If there are no markdown code blocks but the entire string looks like source code,
		// we can try compressing it as a raw file. Let's restrict to markdown blocks for reliability first.
		return s, 0
	}

	// We'll build the new string using a Builder.
	// Since we are replacing matching regions, we copy non-matching prefix, then processed code block, etc.
	var sb strings.Builder
	sb.Grow(len(s))
	lastIdx := 0
	totalSaved := 0

	for _, match := range matches {
		start := match[0]
		end := match[1]
		langStart, langEnd := match[2], match[3]
		bodyStart, bodyEnd := match[4], match[5]

		lang := ""
		if langStart != -1 && langEnd != -1 {
			lang = strings.ToLower(s[langStart:langEnd])
		}
		body := s[bodyStart:bodyEnd]

		// Copy the text between last match and current match
		sb.WriteString(s[lastIdx:start])

		// Apply syntactic compression on the body of the code block if eligible
		if len(body) >= 128 {
			compressedBody, saved := CompressRawCode(body, lang)
			if saved > 0 {
				totalSaved += saved
				sb.WriteString("```")
				sb.WriteString(lang)
				sb.WriteString("\n")
				sb.WriteString(compressedBody)
				sb.WriteString("\n```")
				lastIdx = end
				continue
			}
		}

		// If no compression occurred or not eligible, copy original match verbatim
		sb.WriteString(s[start:end])
		lastIdx = end
	}

	sb.WriteString(s[lastIdx:])
	return sb.String(), totalSaved
}

// CompressRawCode compresses raw code string according to language syntax rules.
func CompressRawCode(code string, lang string) (string, int) {
	lang = strings.TrimSpace(strings.ToLower(lang))
	if len(code) < 128 {
		return code, 0
	}

	// Save original uncompressed code block first
	hash, err := SaveToElidedCache(code)
	if err != nil {
		logSyntax.Errorf("failed to save code block to elided cache: %v", err)
		return code, 0
	}

	var compressed string
	switch lang {
	case "go", "golang", "js", "javascript", "ts", "typescript", "java", "cpp", "c++", "c", "rust", "rs", "cs", "csharp":
		compressed = compressBraceLanguage(code, hash)
	case "py", "python":
		compressed = compressPython(code, hash)
	default:
		// Unknown or generic language: we can collapse imports block anyway
		compressed = compressGenericImports(code)
	}

	// Verify we actually saved something
	saved := len(code) - len(compressed)
	if saved <= 0 {
		return code, 0
	}

	logSyntax.L().Debug("syntactically compressed code block",
		slog.String("lang", lang),
		slog.Int("bytes_saved", saved),
		slog.String("hash", hash),
	)

	// Append a metadata comment to let the LLM know CCR is available
	marker := fmt.Sprintf("\n// [SYNTAX COMPRESSED - original cached with hash=%s. Use retrieve_elided_content if needed]\n", hash)
	if lang == "py" || lang == "python" {
		marker = fmt.Sprintf("\n# [SYNTAX COMPRESSED - original cached with hash=%s. Use retrieve_elided_content if needed]\n", hash)
	}

	return marker + compressed, saved
}

// compressBraceLanguage compresses Go, JS, TS, C++, Rust, etc. by collapsing imports and brace-based function bodies.
func compressBraceLanguage(code string, hash string) string {
	// 1. Collapse imports block first
	code = collapseBraceImports(code)

	// 2. Collapse brace-delimited function/method bodies
	lines := strings.Split(code, "\n")
	var result []string
	i := 0
	n := len(lines)

	// Function signature pattern: matches functions and methods with argument lists followed by `{`
	// Excludes struct, interface, and class definitions that do not have parameter parentheses.
	funcPattern := regexp.MustCompile(`\w+\s*\([^)]*\)[^{]*\{`)

	for i < n {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if funcPattern.MatchString(trimmed) && strings.HasSuffix(trimmed, "{") {
			// Found a potential function start. Let's find its matching brace
			// To be robust, we need to balance braces starting from the '{' on this line.
			bodyLines, nextIdx := extractBraceBody(lines, i)
			if len(bodyLines) > 3 {
				// Compress the function body
				indent := getIndentation(line)
				result = append(result, line)
				result = append(result, indent+"\t/* body elided (retrieve_elided_content: hash="+hash+") */")
				result = append(result, indent+"}")
				i = nextIdx
				continue
			}
		}

		result = append(result, line)
		i++
	}

	return strings.Join(result, "\n")
}

// extractBraceBody finds the matching closing brace and returns the body lines and the next line index to resume.
func extractBraceBody(lines []string, startLineIdx int) ([]string, int) {
	var bodyLines []string
	braceCount := 1
	n := len(lines)
	
	// Start counting braces from the line after startLineIdx
	i := startLineIdx + 1
	for i < n {
		line := lines[i]
		for _, ch := range line {
			if ch == '{' {
				braceCount++
			} else if ch == '}' {
				braceCount--
			}
		}

		if braceCount == 0 {
			// Found matching brace
			return bodyLines, i + 1
		}

		bodyLines = append(bodyLines, line)
		i++
	}

	// If match not found, return empty/fallback so we don't corrupt the file
	return nil, startLineIdx + 1
}

// compressPython compresses Python indentation-based blocks
func compressPython(code string, hash string) string {
	code = collapsePythonImports(code)

	lines := strings.Split(code, "\n")
	var result []string
	i := 0
	n := len(lines)

	// Pattern for python function/method definitions (excludes 'class' to preserve method signatures)
	pyFuncPattern := regexp.MustCompile(`^\s*def\s+\w+.*:\s*$`)

	for i < n {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		if pyFuncPattern.MatchString(line) && !strings.HasPrefix(trimmed, "#") {
			// Found a function/class definition.
			// The body includes all subsequent lines that have greater indentation than this definition,
			// excluding empty lines and comments.
			defIndent := len(line) - len(strings.TrimLeft(line, " \t"))
			
			// Scan subsequent lines
			j := i + 1
			bodyLinesCount := 0
			hasBody := false

			for j < n {
				subLine := lines[j]
				subTrimmed := strings.TrimSpace(subLine)
				
				if subTrimmed == "" || strings.HasPrefix(subTrimmed, "#") {
					j++
					continue
				}

				subIndent := len(subLine) - len(strings.TrimLeft(subLine, " \t"))
				if subIndent <= defIndent {
					// Indentation returned to definition level or shallower. Body ended.
					break
				}

				bodyLinesCount++
				hasBody = true
				j++
			}

			if hasBody && bodyLinesCount > 2 {
				// Compress the body
				indent := getIndentation(line)
				result = append(result, line)
				result = append(result, indent+"    pass # body elided (retrieve_elided_content: hash="+hash+")")
				i = j
				continue
			}
		}

		result = append(result, line)
		i++
	}

	return strings.Join(result, "\n")
}

// collapseBraceImports collapses Go imports block.
func collapseBraceImports(code string) string {
	goImportRegex := regexp.MustCompile(`(?s)import\s+\(\s*.*?\s*\)`)
	return goImportRegex.ReplaceAllString(code, "import ( /* collapsed imports (retrieve_elided_content) */ )")
}

// collapsePythonImports collapses Python contiguous imports.
func collapsePythonImports(code string) string {
	lines := strings.Split(code, "\n")
	var result []string
	inImportBlock := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		isImport := strings.HasPrefix(trimmed, "import ") || strings.HasPrefix(trimmed, "from ") && strings.Contains(trimmed, " import ")

		if isImport {
			if !inImportBlock {
				result = append(result, "import ... # collapsed python imports (retrieve_elided_content)")
				inImportBlock = true
			}
			continue
		} else {
			inImportBlock = false
		}
		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// compressGenericImports collapses generic imports in other files.
func compressGenericImports(code string) string {
	return collapsePythonImports(code)
}

// getIndentation returns the whitespace prefix of a string.
func getIndentation(s string) string {
	var indent []rune
	for _, r := range s {
		if r == ' ' || r == '\t' {
			indent = append(indent, r)
		} else {
			break
		}
	}
	return string(indent)
}

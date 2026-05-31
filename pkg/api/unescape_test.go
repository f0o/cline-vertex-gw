package api

import (
	"strings"
	"testing"
)

func TestEntityUnescaper_Basic(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "plain text passes through",
			input:    "hello world",
			expected: "hello world",
		},
		{
			name:     "valid execute_command tag unescapes",
			input:    "\x26lt;execute_command\x26gt;",
			expected: "<execute_command>",
		},
		{
			name:     "valid closing tag unescapes",
			input:    "\x26lt;/execute_command\x26gt;",
			expected: "</execute_command>",
		},
		{
			name:     "unsupported tag does not unescape",
			input:    "\x26lt;div\x26gt;hello\x26lt;/div\x26gt;",
			expected: "\x26lt;div\x26gt;hello\x26lt;/div\x26gt;",
		},
		{
			name:     "ampersand outside tool block does not unescape",
			input:    "hello \x26amp;\x26amp; world",
			expected: "hello \x26amp;\x26amp; world",
		},
		{
			name:     "ampersand inside tool block unescapes",
			input:    "\x26lt;execute_command\x26gt;hello \x26amp;\x26amp; world\x26lt;/execute_command\x26gt;",
			expected: "<execute_command>hello && world</execute_command>",
		},
		{
			name:     "quotes and other entities inside tool block unescape",
			input:    "\x26lt;execute_command\x26gt;\x26quot;hello\x26#39;s\x26quot;\x26lt;/execute_command\x26gt;",
			expected: "<execute_command>\"hello's\"</execute_command>",
		},
		{
			name:     "mixed supported and unsupported tags",
			input:    "\x26lt;execute_command\x26gt;\x26lt;p\x26gt;test\x26lt;/p\x26gt;\x26lt;/execute_command\x26gt;",
			expected: "<execute_command>\x26lt;p\x26gt;test\x26lt;/p\x26gt;</execute_command>",
		},
		{
			name:     "valid parameter tag",
			input:    "\x26lt;command\x26gt;ls -la\x26lt;/command\x26gt;",
			expected: "<command>ls -la</command>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := NewEntityUnescaper()
			got := u.Process(tt.input) + u.Flush()
			if got != tt.expected {
				t.Errorf("got %q, expected %q", got, tt.expected)
			}
		})
	}
}

func TestEntityUnescaper_Streaming(t *testing.T) {
	tests := []struct {
		name     string
		chunks   []string
		expected string
	}{
		{
			name:     "tag split across chunk boundary",
			chunks:   []string{"\x26l", "t;execute_command\x26gt;"},
			expected: "<execute_command>",
		},
		{
			name:     "tag name split across chunk boundary",
			chunks:   []string{"\x26lt;execute_", "command\x26gt;"},
			expected: "<execute_command>",
		},
		{
			name:     "closing entity split across chunk boundary",
			chunks:   []string{"\x26lt;execute_command\x26g", "t;"},
			expected: "<execute_command>",
		},
		{
			name:     "unsupported tag split doesn't get unescaped",
			chunks:   []string{"\x26lt;di", "v\x26gt;"},
			expected: "\x26lt;div\x26gt;",
		},
		{
			name:     "entity inside tool block split",
			chunks:   []string{"\x26lt;execute_command\x26gt;hello \x26am", "p;\x26amp; world\x26lt;/execute_command\x26gt;"},
			expected: "<execute_command>hello && world</execute_command>",
		},
		{
			name:     "interrupted/incomplete tag flushes raw",
			chunks:   []string{"\x26lt;execute_"},
			expected: "\x26lt;execute_",
		},
		{
			name:     "interrupted entity flushes raw",
			chunks:   []string{"\x26l"},
			expected: "\x26l",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := NewEntityUnescaper()
			var got strings.Builder
			for _, chunk := range tt.chunks {
				got.WriteString(u.Process(chunk))
			}
			got.WriteString(u.Flush())
			if got.String() != tt.expected {
				t.Errorf("got %q, expected %q", got.String(), tt.expected)
			}
		})
	}
}

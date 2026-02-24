package structuredoutput

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Validate checks that raw JSON is valid and non-empty.
// For deeper schema validation, callers can unmarshal into a typed struct.
func Validate(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("empty JSON output")
	}
	if !json.Valid(raw) {
		return fmt.Errorf("invalid JSON: %s", truncateStr(string(raw), 200))
	}
	return nil
}

// ExtractJSONFromContent extracts a JSON object from text that may contain
// markdown fences or surrounding prose. It is string-aware: braces inside
// quoted JSON strings are ignored.
func ExtractJSONFromContent(content string) (json.RawMessage, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return nil, fmt.Errorf("empty content")
	}

	// Try direct parse first.
	if json.Valid([]byte(s)) && (s[0] == '{' || s[0] == '[') {
		return json.RawMessage(s), nil
	}

	// Extract from markdown fences or surrounding text.
	extracted := extractJSONObject(s)
	if extracted == "" {
		return nil, fmt.Errorf("no JSON object found in content: %s", truncateStr(s, 200))
	}

	if !json.Valid([]byte(extracted)) {
		return nil, fmt.Errorf("extracted JSON is invalid: %s", truncateStr(extracted, 200))
	}

	return json.RawMessage(extracted), nil
}

// extractJSONObject pulls a JSON object from text. String-aware: braces inside
// quoted strings are ignored so markdown content doesn't break extraction.
func extractJSONObject(s string) string {
	start := -1
	for i := range s {
		if s[i] == '{' {
			start = i
			break
		}
	}
	if start == -1 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return s[start:]
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

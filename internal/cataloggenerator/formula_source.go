package cataloggenerator

import (
	"bytes"
	"errors"
	"regexp"
)

var bottleBlockStart = regexp.MustCompile(`^([\t ]*)bottle[\t ]+do(?:[\t ]*#.*)?\r?$`)

// normalizedBottleFormula removes the exact top-level bottle DSL block that
// Homebrew intentionally omits from the Formula copy embedded in a bottle. It
// is conservative: ambiguous or unterminated blocks fail instead of accepting
// a source relation that cannot be established.
func normalizedBottleFormula(source []byte) ([]byte, error) {
	lines := bytes.SplitAfter(source, []byte("\n"))
	start, end := -1, -1
	var indentation []byte
	for i, line := range lines {
		trimmed := bytes.TrimSuffix(line, []byte("\n"))
		match := bottleBlockStart.FindSubmatch(trimmed)
		if match == nil {
			continue
		}
		if start >= 0 {
			return nil, errors.New("Formula contains multiple bottle blocks")
		}
		start = i
		indentation = append([]byte(nil), match[1]...)
	}
	if start < 0 {
		return append([]byte(nil), source...), nil
	}
	endLine := append(append([]byte(nil), indentation...), []byte("end")...)
	for i := start + 1; i < len(lines); i++ {
		trimmed := bytes.TrimSuffix(lines[i], []byte("\n"))
		trimmed = bytes.TrimSuffix(trimmed, []byte("\r"))
		if bytes.Equal(trimmed, endLine) {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, errors.New("Formula bottle block is unterminated or ambiguous")
	}
	// Homebrew's embedded Formula omits the blank separator after the bottle
	// block as well. Preserve every other byte exactly.
	after := end + 1
	if after < len(lines) && len(bytes.TrimSpace(lines[after])) == 0 {
		after++
	}
	result := make([]byte, 0, len(source))
	for _, line := range lines[:start] {
		result = append(result, line...)
	}
	for _, line := range lines[after:] {
		result = append(result, line...)
	}
	return result, nil
}

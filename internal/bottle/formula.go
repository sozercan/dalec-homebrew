package bottle

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var formulaSubclassPattern = regexp.MustCompile(`(?m)^[\t ]*class[\t ]+([A-Z][A-Za-z0-9]*)[\t ]*<[\t ]*Formula(?:[\t ]*(?:#.*)?)?\r?$`)

func validateFormulaSource(name string, source []byte) (string, error) {
	if len(source) == 0 {
		return "", fmt.Errorf("Formula source is empty")
	}
	if !utf8.Valid(source) || strings.IndexByte(string(source), 0) >= 0 {
		return "", fmt.Errorf("Formula source is not valid text")
	}

	expected := formulaClassName(name)
	matches := formulaSubclassPattern.FindAllSubmatch(source, -1)
	if len(matches) != 1 {
		return "", fmt.Errorf("expected exactly one Formula subclass, found %d", len(matches))
	}
	actual := string(matches[0][1])
	if actual != expected {
		return "", fmt.Errorf("Formula class %q does not match expected %q", actual, expected)
	}
	hasEnd := false
	for _, line := range strings.Split(string(source), "\n") {
		if strings.TrimSpace(strings.TrimSuffix(line, "\r")) == "end" {
			hasEnd = true
			break
		}
	}
	if !hasEnd {
		return "", fmt.Errorf("Formula class has no closing end")
	}
	return actual, nil
}

// formulaClassName mirrors Homebrew Formulary.class_s for the ASCII Formula
// names admitted by validateExpectation. It is used only as a static identity
// check; the Ruby source is never evaluated.
func formulaClassName(name string) string {
	if name == "" {
		return ""
	}
	s := strings.ToLower(name)
	if word, ok := digitWord(s[0]); ok {
		s = word + s[1:]
	} else {
		s = strings.ToUpper(s[:1]) + s[1:]
	}

	var b strings.Builder
	b.Grow(len(s))
	capitalizeNext := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' || c == '_' || c == '.' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			capitalizeNext = true
			continue
		}
		if capitalizeNext && ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			if c >= 'a' && c <= 'z' {
				c -= 'a' - 'A'
			}
			capitalizeNext = false
		}
		if c == '+' {
			c = 'x'
		}
		b.WriteByte(c)
	}
	s = b.String()
	for i := 1; i+1 < len(s); i++ {
		if s[i] == '@' && s[i+1] >= '0' && s[i+1] <= '9' {
			return s[:i] + "AT" + s[i+1:]
		}
	}
	return s
}

func digitWord(value byte) (string, bool) {
	words := map[byte]string{'0': "Zero", '1': "One", '2': "Two", '3': "Three", '4': "Four", '5': "Five", '6': "Six", '7': "Seven", '8': "Eight", '9': "Nine"}
	word, ok := words[value]
	return word, ok
}

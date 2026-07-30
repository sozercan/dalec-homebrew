package testrunner

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

func checkOutput(subject string, actual []byte, check CheckOutput) error {
	var failures []error

	if check.Equals != "" && !bytes.Equal(actual, []byte(check.Equals)) {
		failures = append(failures, fmt.Errorf("equals: expected %s, got %s", quotePreview([]byte(check.Equals)), quotePreview(actual)))
	}
	for _, expected := range check.Contains {
		if !bytes.Contains(actual, []byte(expected)) {
			failures = append(failures, fmt.Errorf("contains: expected output to contain %q, got %s", expected, quotePreview(actual)))
		}
	}
	for _, pattern := range check.Matches {
		re, err := regexp.Compile(pattern)
		if err != nil {
			failures = append(failures, fmt.Errorf("matches %q: %w", pattern, err))
			continue
		}
		if !re.Match(actual) {
			failures = append(failures, fmt.Errorf("matches: expected output to match %q, got %s", pattern, quotePreview(actual)))
		}
	}
	if check.StartsWith != "" && !bytes.HasPrefix(actual, []byte(check.StartsWith)) {
		failures = append(failures, fmt.Errorf("starts_with: expected prefix %q, got %s", check.StartsWith, quotePreview(actual)))
	}
	if check.EndsWith != "" && !bytes.HasSuffix(actual, []byte(check.EndsWith)) {
		failures = append(failures, fmt.Errorf("ends_with: expected suffix %q, got %s", check.EndsWith, quotePreview(actual)))
	}
	if check.Empty && len(actual) != 0 {
		failures = append(failures, fmt.Errorf("empty: expected empty output, got %s", quotePreview(actual)))
	}

	if err := errors.Join(failures...); err != nil {
		return fmt.Errorf("%s: %w", subject, err)
	}
	return nil
}

const maxPreviewBytes = 4096

func quotePreview(data []byte) string {
	if len(data) <= maxPreviewBytes {
		return strconv.Quote(string(data))
	}
	return strconv.Quote(string(data[:maxPreviewBytes])) + fmt.Sprintf("... (%d bytes total)", len(data))
}

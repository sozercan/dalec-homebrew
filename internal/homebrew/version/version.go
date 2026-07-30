// Package version implements the small, deterministic subset of Homebrew's
// Version/PkgVersion ordering needed to validate current bottle dependency
// minimums. It deliberately does not implement range solving (a V2 feature).
package version

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var (
	revisionSuffix = regexp.MustCompile(`^(.*)_([0-9]+)$`)
	safeName       = regexp.MustCompile(`^[a-z0-9][a-z0-9+_.@-]*$`)
)

func PkgVersion(stable string, revision int) string {
	if revision > 0 {
		return fmt.Sprintf("%s_%d", stable, revision)
	}
	return stable
}

func SplitPkgVersion(v string) (stable string, revision int, err error) {
	if m := revisionSuffix.FindStringSubmatch(v); m != nil {
		n, parseErr := strconv.Atoi(m[2])
		if parseErr != nil {
			return "", 0, parseErr
		}
		return m[1], n, nil
	}
	if strings.TrimSpace(v) == "" {
		return "", 0, fmt.Errorf("empty package version")
	}
	return v, 0, nil
}

func ImageTag(pkgVersion string, bottleRebuild int) string {
	if bottleRebuild > 0 {
		return fmt.Sprintf("%s-%d", pkgVersion, bottleRebuild)
	}
	return pkgVersion
}

func OCIFormulaPath(name string) (string, error) {
	if !safeName.MatchString(name) || strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("invalid Formula name %q", name)
	}
	return strings.NewReplacer("@", "/", "+", "x").Replace(name), nil
}

func BottleFilename(name, pkgVersion, bottleTag string, rebuild int) (string, error) {
	if !safeName.MatchString(name) || strings.ContainsAny(name, `/\\`) {
		return "", fmt.Errorf("invalid Formula name %q", name)
	}
	for label, v := range map[string]string{"package version": pkgVersion, "bottle tag": bottleTag} {
		if v == "" || path.Base(v) != v || strings.ContainsAny(v, `/\\`) {
			return "", fmt.Errorf("invalid %s %q", label, v)
		}
	}
	suffix := ""
	if rebuild > 0 {
		suffix = fmt.Sprintf(".%d", rebuild)
	}
	return fmt.Sprintf("%s--%s.%s.bottle%s.tar.gz", name, pkgVersion, bottleTag, suffix), nil
}

// Compare returns -1, 0, or 1. Separators are insignificant, numeric runs are
// compared numerically, and trailing alphabetic runs are treated as
// prereleases (so 1.0 > 1.0rc1). This matches the dependency versions emitted
// by Homebrew bottle tabs without claiming to be a complete range solver.
func Compare(a, b string) int {
	ta, tb := tokenize(a), tokenize(b)
	for i := 0; i < len(ta) || i < len(tb); i++ {
		if i >= len(ta) {
			return compareEndToRemainder(tb[i:])
		}
		if i >= len(tb) {
			return -compareEndToRemainder(ta[i:])
		}
		x, y := ta[i], tb[i]
		if x.numeric && y.numeric {
			if c := compareNumeric(x.value, y.value); c != 0 {
				return c
			}
			continue
		}
		if x.numeric != y.numeric {
			if x.numeric {
				return 1
			}
			return -1
		}
		if c := compareAlpha(x.value, y.value); c != 0 {
			return c
		}
	}
	return 0
}

func AtLeast(selectedVersion string, selectedRevision int, minimumVersion string, minimumRevision int) bool {
	if c := Compare(selectedVersion, minimumVersion); c != 0 {
		return c > 0
	}
	return selectedRevision >= minimumRevision
}

type token struct {
	value   string
	numeric bool
}

func tokenize(v string) []token {
	var out []token
	var b strings.Builder
	kind := 0
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out = append(out, token{value: b.String(), numeric: kind == 1})
		b.Reset()
	}
	for _, r := range strings.ToLower(v) {
		k := 0
		if unicode.IsDigit(r) {
			k = 1
		} else if unicode.IsLetter(r) {
			k = 2
		}
		if k == 0 {
			flush()
			kind = 0
			continue
		}
		if kind != 0 && kind != k {
			flush()
		}
		kind = k
		b.WriteRune(r)
	}
	flush()
	return out
}

func compareNumeric(a, b string) int {
	a = strings.TrimLeft(a, "0")
	b = strings.TrimLeft(b, "0")
	if a == "" {
		a = "0"
	}
	if b == "" {
		b = "0"
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

func compareEndToRemainder(rest []token) int {
	for _, t := range rest {
		if t.numeric {
			if compareNumeric(t.value, "0") != 0 {
				return -1
			}
			continue
		}
		rank := alphaRank(t.value)
		if rank < 0 {
			return 1
		}
		return -1
	}
	return 0
}

func alphaRank(value string) int {
	order := map[string]int{"dev": -5, "alpha": -4, "a": -4, "beta": -3, "b": -3, "pre": -2, "preview": -2, "rc": -1, "p": 1, "patch": 1, "pl": 1}
	if rank, ok := order[value]; ok {
		return rank
	}
	return 1
}

func compareAlpha(a, b string) int {
	if a == b {
		return 0
	}
	oa, ob := alphaRank(a), alphaRank(b)
	if oa != ob {
		if oa < ob {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}

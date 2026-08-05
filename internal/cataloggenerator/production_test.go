package cataloggenerator

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

func TestCopyTapCommitsRejectsInvalidPins(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tooMany := make(map[catalog.TapID]string, catalog.MaxTaps+1)
	for i := 0; i <= catalog.MaxTaps; i++ {
		tooMany[catalog.TapID(fmt.Sprintf("acme/tap%d", i))] = commit
	}
	tests := []struct {
		name    string
		pins    map[catalog.TapID]string
		message string
	}{
		{name: "malformed tap", pins: map[catalog.TapID]string{"Acme/tools": commit}, message: "invalid Homebrew tap"},
		{name: "core tap", pins: map[catalog.TapID]string{"homebrew/core": commit}, message: "homebrew/core"},
		{name: "malformed commit", pins: map[catalog.TapID]string{"acme/tools": strings.Repeat("A", 40)}, message: "lowercase 40-hex"},
		{name: "too many", pins: tooMany, message: fmt.Sprintf("limit %d", catalog.MaxTaps)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := copyTapCommits(test.pins)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCopyTapCommitsReturnsIndependentMap(t *testing.T) {
	tap, _ := catalog.ParseTapID("acme/tools")
	commit := strings.Repeat("a", 40)
	pins := map[catalog.TapID]string{tap: commit}
	copied, err := copyTapCommits(pins)
	if err != nil {
		t.Fatal(err)
	}
	pins[tap] = strings.Repeat("b", 40)
	if got := copied[tap]; got != commit {
		t.Fatalf("copied pin=%q want=%q", got, commit)
	}
}

func TestProductionCacheIdentityIncludesCanonicalTapCommitSet(t *testing.T) {
	alpha, _ := catalog.ParseTapID("acme/alpha")
	zeta, _ := catalog.ParseTapID("acme/zeta")
	alphaCommit := strings.Repeat("a", 40)
	zetaCommit := strings.Repeat("b", 40)
	first := map[catalog.TapID]string{}
	first[zeta] = zetaCommit
	first[alpha] = alphaCommit
	second := map[catalog.TapID]string{}
	second[alpha] = alphaCommit
	second[zeta] = zetaCommit
	wantSet := "acme/alpha=" + alphaCommit + "\nacme/zeta=" + zetaCommit
	if got := canonicalTapCommitSet(first); got != wantSet {
		t.Fatalf("canonical set=%q want=%q", got, wantSet)
	}
	firstKey := cacheKey(productionCacheIdentity("verification", "policy", strings.Repeat("c", 40), first))
	secondKey := cacheKey(productionCacheIdentity("verification", "policy", strings.Repeat("c", 40), second))
	if firstKey != secondKey {
		t.Fatalf("equivalent pin sets produced different cache identities: %s != %s", firstKey, secondKey)
	}
	changed := map[catalog.TapID]string{alpha: strings.Repeat("d", 40), zeta: zetaCommit}
	changedKey := cacheKey(productionCacheIdentity("verification", "policy", strings.Repeat("c", 40), changed))
	if firstKey == changedKey {
		t.Fatal("changed tap commit reused the prior cache identity")
	}
	unpinnedKey := cacheKey(productionCacheIdentity("verification", "policy", strings.Repeat("c", 40), nil))
	if firstKey == unpinnedKey {
		t.Fatal("pinned and unpinned configurations share a cache identity")
	}
}

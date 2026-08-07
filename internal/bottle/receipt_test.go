package bottle

import (
	"strings"
	"testing"
)

func TestNormalizeDiscoveredReceiptDependenciesAllowsAlphabeticVersions(t *testing.T) {
	for _, version := range []string{"1.2.3-beta", "nightly", "trn"} {
		t.Run(version, func(t *testing.T) {
			dependency := ReceiptDependency{FullName: "acme/tools/dep", Version: version, PkgVersion: version}
			got, err := normalizeDiscoveredReceiptDependencies([]ReceiptDependency{dependency})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Version != version {
				t.Fatalf("normalized dependencies=%+v", got)
			}
		})
	}
}

func TestNormalizeDiscoveredReceiptDependenciesRejectsPathAndWhitespaceVersions(t *testing.T) {
	for _, version := range []string{"1/2", `1\2`, "1 2", "1\t2", "1\r2", "1\n2"} {
		t.Run(strings.ReplaceAll(version, "\n", "newline"), func(t *testing.T) {
			dependency := ReceiptDependency{FullName: "acme/tools/dep", Version: version, PkgVersion: version}
			if _, err := normalizeDiscoveredReceiptDependencies([]ReceiptDependency{dependency}); err == nil {
				t.Fatalf("accepted invalid version %q", version)
			}
		})
	}
}

package version

import "testing"

func TestPkgVersionAndTags(t *testing.T) {
	if got := PkgVersion("3.13.14", 1); got != "3.13.14_1" {
		t.Fatalf("got %q", got)
	}
	if got := ImageTag("3.13.14_1", 1); got != "3.13.14_1-1" {
		t.Fatalf("got %q", got)
	}
	if got, _ := OCIFormulaPath("python@3.13"); got != "python/3.13" {
		t.Fatalf("got %q", got)
	}
	if got, _ := OCIFormulaPath("libc++"); got != "libcxx" {
		t.Fatalf("got %q", got)
	}
	if got, _ := BottleFilename("python@3.13", "3.13.14_1", "arm64_linux", 1); got != "python@3.13--3.13.14_1.arm64_linux.bottle.1.tar.gz" {
		t.Fatalf("got %q", got)
	}
}

func TestCompare(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1.10", "1.9", 1}, {"1.0", "1.0", 0}, {"1.0rc1", "1.0", -1},
		{"3.13.14", "3.13.5", 1}, {"2026-07-16", "2025-12-31", 1}, {"1.0.0", "1.0", 0},
	} {
		got := Compare(tc.a, tc.b)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != tc.want {
			t.Errorf("Compare(%q,%q)=%d want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

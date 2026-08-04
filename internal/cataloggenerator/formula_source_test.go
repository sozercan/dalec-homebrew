package cataloggenerator

import "testing"

func TestNormalizedBottleFormulaRemovesOnlyBottleBlock(t *testing.T) {
	source := []byte("class Widget < Formula\n  url \"https://example.test\"\n\n  bottle do\n    root_url \"https://bottles.test\"\n    sha256 x86_64_linux: \"abc\"\n  end\n\n  def install\n  end\nend\n")
	want := "class Widget < Formula\n  url \"https://example.test\"\n\n  def install\n  end\nend\n"
	got, err := normalizedBottleFormula(source)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("normalized:\n%s\nwant:\n%s", got, want)
	}
}

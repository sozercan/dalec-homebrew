package catalog

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestPrebuiltArchiveDeclarationValidationAndFormulaUnion(t *testing.T) {
	t.Parallel()
	declaration := validPrebuiltArchiveDeclaration()
	if err := ValidatePrebuiltArchiveDeclaration(declaration); err != nil {
		t.Fatalf("valid declaration: %v", err)
	}

	formula := validTapCatalog().Formulae[0]
	formula.Bottle = nil
	if err := validateFormula(formula, "acme/tools"); err != nil {
		t.Fatalf("Formula without an artifact declaration: %v", err)
	}
	formulaDeclaration := cloneForTest(t, declaration)
	formula.PrebuiltArchive = &formulaDeclaration
	if err := validateFormula(formula, "acme/tools"); err != nil {
		t.Fatalf("Formula with prebuilt declaration: %v", err)
	}
	formula.Bottle = validTapCatalog().Formulae[0].Bottle
	if err := validateFormula(formula, "acme/tools"); err == nil || !strings.Contains(err.Error(), "both a native bottle and prebuilt archive") {
		t.Fatalf("overlapping native/prebuilt tag err = %v", err)
	}
	formula.PrebuiltArchive.Files = []PrebuiltArchiveFile{formula.PrebuiltArchive.Files[1]}
	formula.Bottle.Files = []BottleFile{formula.Bottle.Files[0]}
	if err := validateFormula(formula, "acme/tools"); err != nil {
		t.Fatalf("non-overlapping native/prebuilt declarations: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*PrebuiltArchiveDeclaration)
		want   string
	}{
		{
			name:   "missing files",
			mutate: func(value *PrebuiltArchiveDeclaration) { value.Files = nil },
			want:   "non-empty array",
		},
		{
			name: "duplicate tag",
			mutate: func(value *PrebuiltArchiveDeclaration) {
				value.Files[1].Tag = value.Files[0].Tag
			},
			want: "duplicate prebuilt archive tag",
		},
		{
			name:   "architecture independent tag",
			mutate: func(value *PrebuiltArchiveDeclaration) { value.Files[0].Tag = "all" },
			want:   "unsupported tag",
		},
		{
			name:   "non HTTPS",
			mutate: func(value *PrebuiltArchiveDeclaration) { value.Files[0].URL = "http://github.com/archive.tar.gz" },
			want:   "absolute HTTPS",
		},
		{
			name:   "unknown format",
			mutate: func(value *PrebuiltArchiveDeclaration) { value.Files[0].Format = "zip" },
			want:   "unsupported archive format",
		},
		{
			name:   "format extension mismatch",
			mutate: func(value *PrebuiltArchiveDeclaration) { value.Files[0].URL = "https://github.com/archive.zip" },
			want:   "does not match",
		},
		{
			name:   "invalid digest",
			mutate: func(value *PrebuiltArchiveDeclaration) { value.Files[0].SHA256 = "sha256:bad" },
			want:   "lowercase hexadecimal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := cloneForTest(t, declaration)
			tt.mutate(&value)
			if err := ValidatePrebuiltArchiveDeclaration(value); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCanonicalPrebuiltArchiveDeclaration(t *testing.T) {
	t.Parallel()
	left := validPrebuiltArchiveDeclaration()
	right := cloneForTest(t, left)
	slices.Reverse(right.Files)
	original := cloneForTest(t, right)

	leftBytes, err := CanonicalPrebuiltArchiveDeclaration(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := CanonicalPrebuiltArchiveDeclaration(right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("canonical declarations differ:\n%s\n%s", leftBytes, rightBytes)
	}
	if !slices.Equal(right.Files, original.Files) {
		t.Fatal("canonicalization mutated the declaration")
	}
}

func TestPrebuiltDerivationValidationAndBindings(t *testing.T) {
	t.Parallel()
	artifact := validPrebuiltArtifact()
	if err := ValidatePrebuiltDerivation(*artifact.PrebuiltDerivation); err != nil {
		t.Fatalf("valid derivation: %v", err)
	}
	if err := ValidateBottleArtifact(artifact); err != nil {
		t.Fatalf("valid derived artifact: %v", err)
	}
	if err := ValidatePrebuiltDerivationSource(validPrebuiltArchiveDeclaration(), artifact.Tag, *artifact.PrebuiltDerivation); err != nil {
		t.Fatalf("declaration binding: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*BottleArtifact)
		want   string
	}{
		{
			name: "source transport missing",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.Source.Transport = Transport{}
			},
			want: "exactly one",
		},
		{
			name: "source transport union",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.Source.Transport.OCI = validOCIArtifact().Transport.OCI
			},
			want: "exactly one",
		},
		{
			name: "source OCI",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.Source.Transport = Transport{OCI: validOCIArtifact().Transport.OCI}
			},
			want: "must use HTTPS",
		},
		{
			name: "source filename",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.Source.Filename = "other.tar.gz"
			},
			want: "does not match source URL basename",
		},
		{
			name: "ELF machine",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.ELF.Machine = PrebuiltELFMachineAArch64
			},
			want: "does not match platform",
		},
		{
			name: "dynamic ELF",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.ELF.StaticallyLinked = false
			},
			want: "statically_linked",
		},
		{
			name: "implicit needed libraries",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.ELF.NeededLibraries = nil
			},
			want: "explicit empty array",
		},
		{
			name: "Formula source tap",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.FormulaSource.Transport.Tap.ID = "other/tools"
				value.PrebuiltDerivation.FormulaSource.Transport.Tap.Repository = "https://github.com/other/homebrew-tools"
			},
			want: "tap does not match",
		},
		{
			name: "Formula source path",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.FormulaSource.Transport.Path = "Formula/other.rb"
			},
			want: "does not match Formula",
		},
		{
			name: "Formula source digest",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.FormulaSource.SHA256 = testDigest('1')
			},
			want: "Formula source digest",
		},
		{
			name: "derived digest",
			mutate: func(value *BottleArtifact) {
				value.PrebuiltDerivation.DerivedBottle.SHA256 = testDigest('1')
			},
			want: "does not match selected artifact",
		},
		{
			name: "non receiptless",
			mutate: func(value *BottleArtifact) {
				value.Tab.Receiptless = false
			},
			want: "must be marked receiptless",
		},
		{
			name: "native source waiver",
			mutate: func(value *BottleArtifact) {
				value.BottleSourceWaiver = HTTPSBottleSourceWaiver
			},
			want: "cannot use the native HTTPS bottle source waiver",
		},
		{
			name: "payload not executable",
			mutate: func(value *BottleArtifact) {
				value.ExecutablePaths = nil
			},
			want: "absent from selected executable paths",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			value := cloneForTest(t, artifact)
			tt.mutate(&value)
			if err := ValidateBottleArtifact(value); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPrebuiltDerivationDeclarationBindingRejectsMixAndMatch(t *testing.T) {
	t.Parallel()
	artifact := validPrebuiltArtifact()
	derivation := *artifact.PrebuiltDerivation
	declaration := validPrebuiltArchiveDeclaration()

	declaration.Files[0].SHA256 = testDigest('1')
	if err := ValidatePrebuiltDerivationSource(declaration, artifact.Tag, derivation); err == nil || !strings.Contains(err.Error(), "digest does not match") {
		t.Fatalf("digest substitution err = %v", err)
	}
	declaration = validPrebuiltArchiveDeclaration()
	if err := ValidatePrebuiltDerivationSource(declaration, "all", derivation); err == nil || !strings.Contains(err.Error(), "no prebuilt archive") {
		t.Fatalf("tag substitution err = %v", err)
	}
}

func TestCanonicalPrebuiltDerivation(t *testing.T) {
	t.Parallel()
	left := cloneForTest(t, *validPrebuiltArtifact().PrebuiltDerivation)
	right := cloneForTest(t, left)
	slices.Reverse(right.Source.Transport.HTTPS.AllowedRedirectHosts)
	original := slices.Clone(right.Source.Transport.HTTPS.AllowedRedirectHosts)

	leftBytes, err := CanonicalPrebuiltDerivation(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := CanonicalPrebuiltDerivation(right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("canonical derivations differ:\n%s\n%s", leftBytes, rightBytes)
	}
	if !slices.Equal(right.Source.Transport.HTTPS.AllowedRedirectHosts, original) {
		t.Fatal("canonicalization mutated derivation redirect hosts")
	}
	for _, required := range []string{`"needed_libraries":[]`, `"rpaths":[]`} {
		if !bytes.Contains(leftBytes, []byte(required)) {
			t.Fatalf("canonical derivation is missing %s: %s", required, leftBytes)
		}
	}
}

func TestDecodeTapCatalogRejectsUnknownPrebuiltFields(t *testing.T) {
	t.Parallel()
	catalog := validTapCatalog()
	catalog.Formulae[0].PrebuiltArchive = func() *PrebuiltArchiveDeclaration {
		value := validPrebuiltArchiveDeclaration()
		return &value
	}()
	catalog.Formulae[0].Bottle = nil
	canonical, err := CanonicalTapCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	withUnknown := bytes.Replace(canonical, []byte(`"prebuilt_archive":{"files":`), []byte(`"prebuilt_archive":{"unknown":true,"files":`), 1)
	if bytes.Equal(withUnknown, canonical) {
		t.Fatalf("prebuilt archive marker not found in %s", canonical)
	}
	if _, err := DecodeTapCatalog(withUnknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field err = %v", err)
	}
}

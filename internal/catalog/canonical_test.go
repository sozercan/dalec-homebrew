package catalog

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func TestCanonicalRequestOrderingAndPerPlatformBinding(t *testing.T) {
	t.Parallel()
	left := validRequest()
	left.Targets = []PlatformRequest{
		{Platform: Platform{OS: "linux", Architecture: "arm64"}, ExternalRoots: []FormulaID{"beta/extra/tool"}},
		{Platform: Platform{OS: "linux", Architecture: "amd64"}, ExternalRoots: []FormulaID{"beta/extra/tool", "acme/tools/widget"}},
	}
	right := cloneForTest(t, *left)
	slices.Reverse(right.Targets)
	for i := range right.Targets {
		slices.Reverse(right.Targets[i].ExternalRoots)
	}

	leftOriginal := cloneForTest(t, left.Targets)
	leftBytes, err := CanonicalRequest(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := CanonicalRequest(&right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("canonical requests differ:\n%s\n%s", leftBytes, rightBytes)
	}
	if !slices.EqualFunc(left.Targets, leftOriginal, func(a, b PlatformRequest) bool {
		return a.Platform == b.Platform && slices.Equal(a.ExternalRoots, b.ExternalRoots)
	}) {
		t.Fatal("CanonicalRequest mutated its input")
	}
	leftDigest, err := RequestDigest(left)
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := RequestDigest(&right)
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("request digests differ: %s != %s", leftDigest, rightDigest)
	}

	swapped := cloneForTest(t, *left)
	swapped.Targets[0].ExternalRoots, swapped.Targets[1].ExternalRoots = swapped.Targets[1].ExternalRoots, swapped.Targets[0].ExternalRoots
	swappedDigest, err := RequestDigest(&swapped)
	if err != nil {
		t.Fatal(err)
	}
	if swappedDigest == leftDigest {
		t.Fatal("moving external roots between platforms did not change the canonical request digest")
	}

	decoded, err := DecodeRequest(leftBytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Targets) != 2 || decoded.Targets[0].Platform.Architecture != "amd64" || decoded.Targets[0].ExternalRoots[0] != "acme/tools/widget" {
		t.Fatalf("decoded request is not canonical: %+v", decoded)
	}
	if bytes.Contains(leftBytes, []byte(`"platforms"`)) {
		t.Fatalf("canonical request retained unscoped platforms: %s", leftBytes)
	}
}

func TestCanonicalTapCatalogOrdering(t *testing.T) {
	t.Parallel()
	left := validTapCatalog()
	right := cloneForTest(t, *left)
	slices.Reverse(right.Formulae)
	slices.Reverse(right.Aliases)
	slices.Reverse(right.Renames)
	slices.Reverse(right.Migrations)
	for i := range right.Formulae {
		slices.Reverse(right.Formulae[i].Dependencies)
		slices.Reverse(right.Formulae[i].Variations)
		slices.Reverse(right.Formulae[i].VersionedFormulae)
		for j := range right.Formulae[i].Variations {
			slices.Reverse(right.Formulae[i].Variations[j].Dependencies)
		}
		if right.Formulae[i].Bottle != nil {
			slices.Reverse(right.Formulae[i].Bottle.Files)
		}
	}

	leftBytes, err := CanonicalTapCatalog(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := CanonicalTapCatalog(&right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("canonical tap catalogs differ:\n%s\n%s", leftBytes, rightBytes)
	}
	decoded, err := DecodeTapCatalog(leftBytes)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Formulae[0].ID != "acme/tools/helper" || decoded.Formulae[len(decoded.Formulae)-1].ID != "acme/tools/widget@1" {
		t.Fatalf("unexpected canonical Formula ordering: %+v", decoded.Formulae)
	}
}

func TestCanonicalCatalogSetOrdering(t *testing.T) {
	t.Parallel()
	left, _, _ := validPayload(t)
	arm := cloneForTest(t, left.Results[0])
	arm.Platform = Platform{OS: "linux", Architecture: "arm64"}
	for i := range arm.Artifacts {
		arm.Artifacts[i].Platform = arm.Platform
		arm.Artifacts[i].Tag = "arm64_linux"
		arm.Artifacts[i].Tab.Arch = "arm64"
		if arm.Artifacts[i].Transport.OCI != nil {
			arm.Artifacts[i].Transport.OCI.Manifest.Platform = &arm.Platform
		}
	}
	for i := range arm.Artifacts {
		artifact := &arm.Artifacts[i]
		artifact.Platform = arm.Platform
		artifact.Tag = "arm64_linux"
		artifact.Filename = strings.ReplaceAll(artifact.Filename, "x86_64_linux", "arm64_linux")
		if artifact.Transport.HTTPS != nil {
			artifact.Transport.HTTPS.Filename = artifact.Filename
		}
		if artifact.Transport.OCI != nil {
			platform := arm.Platform
			artifact.Transport.OCI.Manifest.Platform = &platform
		}
	}
	left.Results = append(left.Results, arm)
	right := cloneForTest(t, *left)
	slices.Reverse(right.Results)
	for i := range right.Results {
		slices.Reverse(right.Results[i].Closure.Nodes)
		slices.Reverse(right.Results[i].Artifacts)
		for j := range right.Results[i].Closure.Nodes {
			slices.Reverse(right.Results[i].Closure.Nodes[j].Dependencies)
		}
		for j := range right.Results[i].Artifacts {
			artifact := &right.Results[i].Artifacts[j]
			if artifact.Transport.HTTPS != nil {
				slices.Reverse(artifact.Transport.HTTPS.AllowedRedirectHosts)
			}
			if artifact.Transport.OCI != nil {
				slices.Reverse(artifact.Transport.OCI.Manifest.Annotations)
			}
		}
	}

	leftBytes, err := CanonicalCatalogSetPayload(left)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := CanonicalCatalogSetPayload(&right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftBytes, rightBytes) {
		t.Fatalf("canonical catalog sets differ:\n%s\n%s", leftBytes, rightBytes)
	}
	decoded, err := DecodeCatalogSetPayload(leftBytes)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Results[0].Platform.Architecture != "amd64" || decoded.Results[1].Platform.Architecture != "arm64" {
		t.Fatalf("unexpected result order: %+v", decoded.Results)
	}
}

func TestDecodeRejectsDuplicateJSONMembersAtAnyDepth(t *testing.T) {
	t.Parallel()
	data := []byte(`{
		"schema_version":"dalec-homebrew-catalog-request/v1",
		"targets":[{"platform":{"os":"linux","architecture":"amd64","architecture":"arm64"},"external_roots":["acme/tools/widget"]}],
		"homebrew_commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"core_snapshot_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}`)
	_, err := DecodeRequest(data)
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON member") || !strings.Contains(err.Error(), "targets[0].platform") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeRejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	t.Parallel()
	canonical, err := CanonicalRequest(validRequest())
	if err != nil {
		t.Fatal(err)
	}
	withUnknown := bytes.Replace(canonical, []byte(`"schema_version":`), []byte(`"unknown":true,"schema_version":`), 1)
	if _, err := DecodeRequest(withUnknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown-field err = %v", err)
	}
	if _, err := DecodeRequest(append(canonical, []byte(` {}`)...)); err == nil || (!strings.Contains(err.Error(), "trailing") && !strings.Contains(err.Error(), "multiple")) {
		t.Fatalf("trailing-value err = %v", err)
	}
}

func TestReferencedTapCatalogVerification(t *testing.T) {
	t.Parallel()
	catalog := validTapCatalog()
	canonical, err := CanonicalTapCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	reference := catalogReferenceForBytes(catalog, canonical)
	decoded, err := DecodeReferencedTapCatalog(reference, canonical)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Tap.ID != catalog.Tap.ID {
		t.Fatalf("tap = %q", decoded.Tap.ID)
	}
	if err := ValidateCatalogReferenceOrigin(reference, "https://catalog.example.com"); err != nil {
		t.Fatal(err)
	}

	noncanonical := append([]byte(" \n"), canonical...)
	noncanonicalReference := catalogReferenceForBytes(catalog, noncanonical)
	if _, err := DecodeReferencedTapCatalog(noncanonicalReference, noncanonical); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("noncanonical err = %v", err)
	}

	mismatch := cloneForTest(t, reference)
	mismatch.Size++
	if _, err := DecodeReferencedTapCatalog(mismatch, canonical); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("size mismatch err = %v", err)
	}
	if err := ValidateCatalogReferenceOrigin(reference, "https://other.example.com"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("origin mismatch err = %v", err)
	}
}

func TestDecodeRejectsBareFormulaIDInProtocolDocument(t *testing.T) {
	t.Parallel()
	canonical, err := CanonicalRequest(validRequest())
	if err != nil {
		t.Fatal(err)
	}
	bare := bytes.Replace(canonical, []byte(`"acme/tools/widget"`), []byte(`"widget"`), 1)
	if _, err := DecodeRequest(bare); err == nil || !strings.Contains(err.Error(), "fully qualified") {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestNormalizedTargetsAreCallerOwned(t *testing.T) {
	t.Parallel()
	request := validRequest()
	request.Targets = append(request.Targets, PlatformRequest{Platform: Platform{OS: "linux", Architecture: "arm64"}, ExternalRoots: []FormulaID{}})
	targets, err := request.NormalizedTargets()
	if err != nil {
		t.Fatal(err)
	}
	if targets[0].Platform.Architecture != "amd64" || targets[1].Platform.Architecture != "arm64" {
		t.Fatalf("targets = %+v", targets)
	}
	targets[0].ExternalRoots[0] = "other/tap/mutated"
	if request.Targets[0].ExternalRoots[0] != "acme/tools/widget" {
		t.Fatal("NormalizedTargets returned request-owned root storage")
	}
}

func TestDuplicateJSONValidationBoundsNesting(t *testing.T) {
	t.Parallel()
	data := []byte(strings.Repeat("[", MaxJSONDepth+2) + "0" + strings.Repeat("]", MaxJSONDepth+2))
	if err := validateUniqueJSON(data); err == nil || !strings.Contains(err.Error(), "nesting exceeds") {
		t.Fatalf("err = %v", err)
	}
}

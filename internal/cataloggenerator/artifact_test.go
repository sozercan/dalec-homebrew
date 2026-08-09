package cataloggenerator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

type artifactResolver struct{}

func (artifactResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}

type artifactRoundTripper struct{ source string }

func (t artifactRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	header := make(http.Header)
	header.Set("Content-Length", stringInt(len(t.source)))
	body := ""
	if request.Method == http.MethodGet {
		body = t.source
	}
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(t.source)), Request: request}, nil
}

func stringInt(value int) string {
	if value == 0 {
		return "0"
	}
	var data [20]byte
	i := len(data)
	for value > 0 {
		i--
		data[i] = byte('0' + value%10)
		value /= 10
	}
	return string(data[i:])
}

func TestVerifyAnnotatedFormulaSourceBindsEmbeddedBytes(t *testing.T) {
	source := "class Widget < Formula\nend\n"
	sum := sha256.Sum256([]byte(source))
	bounded, err := fetcher.New(fetcher.Config{Resolver: artifactResolver{}, Transport: artifactRoundTripper{source: source}})
	if err != nil {
		t.Fatal(err)
	}
	builder := &ProductionArtifactBuilder{fetcher: bounded}
	inspection := &bottle.CatalogInspection{Result: &bottle.Result{Formula: bottle.FormulaEvidence{SHA256: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(len(source))}}}
	if err := builder.verifyAnnotatedFormulaSource(t.Context(), "https://github.com/acme/homebrew-tools", strings.Repeat("a", 40), "Formula/widget.rb", inspection); err != nil {
		t.Fatal(err)
	}
	inspection.Formula.SHA256 = "sha256:" + strings.Repeat("f", 64)
	if err := builder.verifyAnnotatedFormulaSource(t.Context(), "https://github.com/acme/homebrew-tools", strings.Repeat("a", 40), "Formula/widget.rb", inspection); err == nil || !strings.Contains(err.Error(), "embedded Formula") {
		t.Fatalf("err=%v", err)
	}
}

func TestCatalogBottleTabCanonicalizesEmptySlices(t *testing.T) {
	converted, err := catalogBottleTab(resolution.BottleTab{ChangedFiles: []string{}, Dependencies: []resolution.RuntimeDependency{}})
	if err != nil {
		t.Fatal(err)
	}
	if converted.ChangedFiles != nil || converted.Dependencies != nil {
		t.Fatalf("empty slices were not canonicalized: %+v", converted)
	}
}

func TestInspectionExpectationProjectsExactCertifiSharedCARule(t *testing.T) {
	node := catalog.Node{
		ID: "homebrew/core/certifi", Tap: "homebrew/core", Name: "certifi", HomebrewFullName: "homebrew/core/certifi",
		Dependencies: []catalog.Requirement{{ID: "homebrew/core/ca-certificates", DeclaredDirectly: true}},
	}
	expected := inspectionExpectation(node, "x86_64_linux", "sha256:"+strings.Repeat("a", 64), 1, strings.Repeat("a", 64), resolution.BottleTab{})
	if len(expected.AllowedExternalSymlinkRules) != 1 || expected.AllowedExternalSymlinkRules[0] != bottle.ExternalSymlinkRuleCertifiSharedCA || len(expected.AllowedExternalSymlinkFormulae) != 1 || expected.AllowedExternalSymlinkFormulae[0] != "ca-certificates" {
		t.Fatalf("expectation = %+v", expected)
	}
	for name, mutate := range map[string]func(*catalog.Node){
		"non-core owner": func(value *catalog.Node) {
			value.ID, value.Tap, value.HomebrewFullName = "acme/tools/certifi", "acme/tools", "acme/tools/certifi"
		},
		"spoofed owner identity": func(value *catalog.Node) { value.HomebrewFullName = "acme/tools/certifi" },
		"indirect dependency":    func(value *catalog.Node) { value.Dependencies[0].DeclaredDirectly = false },
		"non-core dependency":    func(value *catalog.Node) { value.Dependencies[0].ID = "acme/tools/ca-certificates" },
	} {
		t.Run(name, func(t *testing.T) {
			value := node
			value.Dependencies = append([]catalog.Requirement(nil), node.Dependencies...)
			mutate(&value)
			got := inspectionExpectation(value, "x86_64_linux", "sha256:"+strings.Repeat("a", 64), 1, strings.Repeat("a", 64), resolution.BottleTab{})
			if len(got.AllowedExternalSymlinkRules) != 0 {
				t.Fatalf("unexpected certifi rules = %v", got.AllowedExternalSymlinkRules)
			}
		})
	}
}

func TestDescriptorCanonicalizesEmptyAnnotations(t *testing.T) {
	converted := descriptor(ocispec.Descriptor{Digest: digest.FromString("x"), Size: 1, MediaType: "application/test", Annotations: map[string]string{}})
	if converted.Annotations != nil {
		t.Fatalf("empty annotations were not canonicalized: %+v", converted.Annotations)
	}
}

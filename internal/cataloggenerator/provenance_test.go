package cataloggenerator

import (
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/root"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func TestProvenanceAnnotationsRequireBoundPair(t *testing.T) {
	url, digest, present, err := provenanceAnnotations(nil, nil)
	if err != nil || present || url != "" || digest != "" {
		t.Fatalf("empty annotations = %q %q %v %v", url, digest, present, err)
	}
	_, _, _, err = provenanceAnnotations(map[string]string{annotationSigstoreBundleURL: "https://example.test/bundle"}, nil)
	if err == nil || !strings.Contains(err.Error(), "URL and digest") {
		t.Fatalf("err=%v", err)
	}
	url, digest, present, err = provenanceAnnotations(map[string]string{annotationSigstoreBundleURL: "https://example.test/bundle", annotationSigstoreBundleDigest: "sha256:" + strings.Repeat("a", 64)}, nil)
	if err != nil || !present || url == "" || digest == "" {
		t.Fatalf("annotations = %q %q %v %v", url, digest, present, err)
	}
}

func TestReleaseSigstoreTrustedRootLoads(t *testing.T) {
	data, err := policyv2.SigstoreTrustedRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.NewTrustedRootFromJSON(data); err != nil {
		t.Fatal(err)
	}
}

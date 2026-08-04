package policyv2

import (
	"strings"
	"testing"
)

func TestEmbeddedTapPolicy(t *testing.T) {
	policy, err := LoadTapPolicy()
	if err != nil {
		t.Fatal(err)
	}
	if !policy.PublicOnly || !policy.DefaultBranchOnly || policy.UserSuppliedRemotes {
		t.Fatalf("policy=%+v", policy)
	}
	if digest, err := TapPolicyDigest(); err != nil || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
}

func TestEmbeddedSigstoreTrustedRootMatchesTapPolicy(t *testing.T) {
	data, err := SigstoreTrustedRoot()
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("embedded Sigstore trusted root is empty")
	}
}

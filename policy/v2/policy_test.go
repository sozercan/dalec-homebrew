package policyv2

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestEmbeddedPolicyValidAndStable(t *testing.T) {
	p, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if p.SchemaVersion != SchemaVersion || p.ResolverPolicyVersion != ResolverPolicyVersion {
		t.Fatalf("policy=%+v", p)
	}
	a, err := Canonical()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Canonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("canonical policy bytes changed")
	}
	if digest, err := Digest(); err != nil || !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		t.Fatalf("digest=%q err=%v", digest, err)
	}
	profile := p.MinimalRuntimeProfile()
	if profile.Name != RuntimeProfileMinimalV1 || !slices.Equal(profile.Rules, MinimalV1RuntimePruneRules()) {
		t.Fatalf("runtime profile=%+v", profile)
	}
	profile.Rules[0] = "mutated"
	if p.RuntimeProfile.Rules[0] == "mutated" {
		t.Fatal("runtime profile accessor returned mutable policy storage")
	}
}

func TestCapabilitiesRequireExactFormulaID(t *testing.T) {
	p, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if caps, ok := p.ForFormula("homebrew/core/node"); !ok || len(caps.GeneratedGlobalPaths) == 0 {
		t.Fatalf("core node capabilities=%+v ok=%v", caps, ok)
	}
	for _, spoof := range []string{"acme/tools/node", "node", "homebrew/corex/node"} {
		if caps, ok := p.ForFormula(spoof); ok || len(caps.SharedEtc)+len(caps.GeneratedGlobalPaths)+len(caps.GeneratedKegPaths) != 0 {
			t.Fatalf("spoof %q received capabilities %+v", spoof, caps)
		}
	}
}

func TestValidateRejectsShortCapabilityKey(t *testing.T) {
	p, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p.PackageCapabilities["node"] = Capabilities{GeneratedGlobalPaths: []string{"lib/node_modules/npm"}}
	if err := Validate(p); err == nil {
		t.Fatal("short capability key accepted")
	}
}

func TestUniqueJSONRejectsDuplicateMembers(t *testing.T) {
	var raw map[string]any
	if err := json.Unmarshal(embedded, &raw); err != nil {
		t.Fatal(err)
	}
	if err := validateUniqueJSON([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("duplicate member accepted")
	}
}

func TestRuntimeRulesAreExactFormulaIDs(t *testing.T) {
	policy, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !policy.HasRule("homebrew/core/python@3.14", "python-venv-template") ||
		!policy.HasRule("homebrew/core/llvm@21", "runtime-aux-llvm") ||
		!policy.HasRule("homebrew/core/libpsl", "optional-libpsl-tooling") ||
		!policy.HasRule("homebrew/core/libpsl", "runtime-aux-libpsl") ||
		!policy.HasRule("homebrew/core/certifi", "certifi-shared-ca-link-v1") {
		t.Fatal("expected exact core runtime rules are absent")
	}
	for _, id := range []string{"acme/tools/python@3.14", "acme/tools/llvm@21", "homebrew/core/llvm@22", "acme/tools/libpsl", "acme/tools/certifi"} {
		if policy.HasRule(id, "runtime-aux-llvm") || policy.HasRule(id, "python-venv-template") || policy.HasRule(id, "optional-libpsl-tooling") || policy.HasRule(id, "runtime-aux-libpsl") || policy.HasRule(id, "certifi-shared-ca-link-v1") {
			t.Fatalf("unexpected rule for %s", id)
		}
	}
}

func TestGeneratedGlobalPathOwnersAreExactFormulaIDs(t *testing.T) {
	policy, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	const loadersCache = "lib/gdk-pixbuf-2.0/2.10.0/loaders.cache"
	if !policy.HasGeneratedGlobalPath("homebrew/core/gdk-pixbuf", loadersCache) {
		t.Fatal("gdk-pixbuf does not own its generated loader cache")
	}
	for _, id := range []string{"homebrew/core/librsvg", "homebrew/core/webp-pixbuf-loader", "acme/tools/gdk-pixbuf"} {
		if policy.HasGeneratedGlobalPath(id, loadersCache) {
			t.Fatalf("%s unexpectedly owns the generated loader cache", id)
		}
	}
}

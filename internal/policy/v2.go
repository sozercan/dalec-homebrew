package policy

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

// V2RuntimePolicy returns a freshly decoded copy of the release-bound V2
// executable policy. Keeping the embedded policy behind this package gives the
// frontend, resolver, materializer, and release verifier one authority.
func V2RuntimePolicy() (*policyv2.Policy, error) {
	return policyv2.Load()
}

// V2RuntimePolicyDigest is the canonical digest bound into capable component
// tuples.
func V2RuntimePolicyDigest() (string, error) {
	return policyv2.Digest()
}

func V2TapPolicyDigest() (string, error) {
	return policyv2.TapPolicyDigest()
}

// V2FormulaCapabilities returns capabilities only for an exact canonical
// Formula ID. There is deliberately no short-name fallback.
func V2FormulaCapabilities(id string) (policyv2.Capabilities, bool, error) {
	p, err := V2RuntimePolicy()
	if err != nil {
		return policyv2.Capabilities{}, false, err
	}
	caps, ok := p.ForFormula(id)
	return caps, ok, nil
}

// RuntimeAllowlistV2 derives the executable V2 allowlist from exact Formula
// identities while using rack names only for filesystem paths.
func RuntimeAllowlistV2(record *resolution.RecordV2) (runtimefs.Allowlist, []string, error) {
	projected, _, err := resolution.ProjectV2ForRuntime(record)
	if err != nil {
		return runtimefs.Allowlist{}, nil, err
	}
	allow, writable := RuntimeAllowlist(projected)
	return allow, writable, nil
}

func runtimePolicyBindingsV2(record *resolution.RecordV2) (runtimefs.Allowlist, []string, string, error) {
	if record == nil {
		return runtimefs.Allowlist{}, nil, "", fmt.Errorf("nil V2 resolution")
	}
	if err := VerifyReleaseBindingsV2(record); err != nil {
		return runtimefs.Allowlist{}, nil, "", err
	}
	localPolicyDigest, err := V2RuntimePolicyDigest()
	if err != nil {
		return runtimefs.Allowlist{}, nil, "", err
	}
	if record.Components.ExecutableRuntimePolicyDigest != localPolicyDigest {
		return runtimefs.Allowlist{}, nil, "", fmt.Errorf("V2 executable runtime policy digest %s does not match embedded policy %s", record.Components.ExecutableRuntimePolicyDigest, localPolicyDigest)
	}
	projected, _, err := resolution.ProjectV2ForRuntime(record)
	if err != nil {
		return runtimefs.Allowlist{}, nil, "", err
	}
	allow, writable := RuntimeAllowlist(projected)
	digest, err := runtimefs.PolicyDigest(allow, runtimefs.DefaultInstallPrefix, projected.Nodes)
	if err != nil {
		return runtimefs.Allowlist{}, nil, "", err
	}
	return allow, writable, digest, nil
}

// VerifyReleaseBindingsV2 checks the embedded policy authorities for every V2
// consumer and, when the binary carries release linker bindings, also checks
// those exact release values. Component self-references remain validated by
// the BuildKit orchestration and pinned-reference validation.
func VerifyReleaseBindingsV2(record *resolution.RecordV2) error {
	if record == nil {
		return fmt.Errorf("nil V2 resolution")
	}
	tapDigest, err := policyv2.TapPolicyDigest()
	if err != nil {
		return err
	}
	runtimeDigest, err := V2RuntimePolicyDigest()
	if err != nil {
		return err
	}
	var errs []error
	if record.Components.TapPolicyDigest != tapDigest {
		errs = append(errs, fmt.Errorf("V2 tap policy digest %s does not match embedded policy %s", record.Components.TapPolicyDigest, tapDigest))
	}
	if record.Components.ExecutableRuntimePolicyDigest != runtimeDigest {
		errs = append(errs, fmt.Errorf("V2 executable runtime policy digest %s does not match embedded policy %s", record.Components.ExecutableRuntimePolicyDigest, runtimeDigest))
	}
	exact := func(name string, got, want []string) {
		left, right := slices.Clone(got), slices.Clone(want)
		slices.Sort(left)
		slices.Sort(right)
		if !slices.Equal(left, right) {
			errs = append(errs, fmt.Errorf("V2 supported %s policy versions %v do not match %v", name, left, right))
		}
	}
	exact("catalog", record.Components.SupportedCatalogPolicyVersions, []string{catalog.TapCatalogPolicyVersion})
	exact("fetch", record.Components.SupportedFetchPolicyVersions, []string{fetcher.FetchPolicyVersion})
	exact("provenance", record.Components.SupportedProvenancePolicyVersions, []string{catalog.VerifiedProvenancePolicy, catalog.ChecksumProvenanceWaiver, catalog.HTTPSBottleSourceWaiver, resolution.PrebuiltProvenanceWaiverPolicyV1, resolution.CoreProvenanceWaiverPolicyV1})

	compare := func(name, got, want string) {
		if want != "" && got != want {
			errs = append(errs, fmt.Errorf("V2 %s %q does not match release binding %q", name, got, want))
		}
	}
	compare("catalog service origin", record.Components.CatalogServiceOrigin, config.CatalogServiceOrigin)
	compare("ingestion JWS key-policy digest", record.Components.IngestionJWSKeyPolicyDigest, config.IngestionJWSKeyPolicyDigest)
	compare("tap policy digest", record.Components.TapPolicyDigest, config.TapPolicyDigest)
	compare("executable runtime policy digest", record.Components.ExecutableRuntimePolicyDigest, config.ExecutableRuntimePolicyDigest)
	compare("Homebrew commit", record.Components.HomebrewCommit, config.HomebrewCommit)
	compare("verification-key digest", record.Components.VerificationKeys, config.VerificationKeysDigest)
	compare("portable Ruby version", record.Components.RubyRuntime, config.PortableRubyVersion)
	compare("bottle fetcher reference", record.Components.BottleFetcherRef, config.BottleFetcherRef)
	compiledLists := []struct {
		name string
		raw  string
		got  []string
	}{
		{"catalog", config.SupportedCatalogPolicyVersions, record.Components.SupportedCatalogPolicyVersions},
		{"fetch", config.SupportedFetchPolicyVersions, record.Components.SupportedFetchPolicyVersions},
		{"provenance", config.SupportedProvenancePolicyVersions, record.Components.SupportedProvenancePolicyVersions},
	}
	for _, binding := range compiledLists {
		if binding.raw != "" {
			exact(binding.name+" release", binding.got, strings.Split(binding.raw, ","))
		}
	}
	if config.IngestionJWSKeyPolicyBase64 != "" {
		if _, err := config.CompiledCatalogKeyPolicy(); err != nil {
			errs = append(errs, fmt.Errorf("compiled catalog key policy: %w", err))
		}
	}
	return errors.Join(errs...)
}

// BindRuntimePolicyV2 binds writable paths and the pruning policy digest to the
// V2 record before canonical serialization.
func BindRuntimePolicyV2(record *resolution.RecordV2) (runtimefs.Allowlist, error) {
	allow, writable, digest, err := runtimePolicyBindingsV2(record)
	if err != nil {
		return runtimefs.Allowlist{}, err
	}
	if len(record.Runtime.WritablePaths) > 0 && !slices.Equal(record.Runtime.WritablePaths, writable) {
		return runtimefs.Allowlist{}, fmt.Errorf("runtime writable paths do not match V2 policy")
	}
	if record.PruningPolicyDigest != "" && record.PruningPolicyDigest != digest {
		return runtimefs.Allowlist{}, fmt.Errorf("V2 pruning policy digest mismatch")
	}
	record.Runtime.WritablePaths = slices.Clone(writable)
	record.PruningPolicyDigest = digest
	return allow, nil
}

// VerifyRuntimePolicyV2 verifies the release-bound runtime-policy fields of an
// already serialized V2 record without changing the record. Frontend binding
// may populate these fields before serialization; replay and materialization
// must require their exact presence and value.
func VerifyRuntimePolicyV2(record *resolution.RecordV2) (runtimefs.Allowlist, error) {
	allow, writable, digest, err := runtimePolicyBindingsV2(record)
	if err != nil {
		return runtimefs.Allowlist{}, err
	}
	if len(writable) > 0 && len(record.Runtime.WritablePaths) == 0 {
		return runtimefs.Allowlist{}, fmt.Errorf("V2 runtime writable paths are missing")
	}
	if !slices.Equal(record.Runtime.WritablePaths, writable) {
		return runtimefs.Allowlist{}, fmt.Errorf("runtime writable paths do not match V2 policy")
	}
	if record.PruningPolicyDigest == "" {
		return runtimefs.Allowlist{}, fmt.Errorf("V2 pruning policy digest is missing")
	}
	if record.PruningPolicyDigest != digest {
		return runtimefs.Allowlist{}, fmt.Errorf("V2 pruning policy digest %s does not match policy %s", record.PruningPolicyDigest, digest)
	}
	return allow, nil
}

// VerifyMaterializerRuntimePolicyV2 requires the complete non-circular tuple
// compiled into release materializer images before accepting V2 replay input.
// Generic/local binaries without the materializer role marker fail closed.
func VerifyMaterializerRuntimePolicyV2(record *resolution.RecordV2) (runtimefs.Allowlist, error) {
	if config.MaterializerV2BindingsRequired != "1" {
		return runtimefs.Allowlist{}, fmt.Errorf("materializer was not built with required V2 release bindings")
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"bottle fetcher reference", config.BottleFetcherRef},
		{"catalog service origin", config.CatalogServiceOrigin},
		{"ingestion JWS key-policy digest", config.IngestionJWSKeyPolicyDigest},
		{"tap policy digest", config.TapPolicyDigest},
		{"executable runtime policy digest", config.ExecutableRuntimePolicyDigest},
		{"Homebrew commit", config.HomebrewCommit},
		{"verification-key digest", config.VerificationKeysDigest},
		{"portable Ruby version", config.PortableRubyVersion},
		{"supported catalog policies", config.SupportedCatalogPolicyVersions},
		{"supported fetch policies", config.SupportedFetchPolicyVersions},
		{"supported provenance policies", config.SupportedProvenancePolicyVersions},
	} {
		if field.value == "" {
			return runtimefs.Allowlist{}, fmt.Errorf("materializer V2 release binding %s is empty", field.name)
		}
	}
	return VerifyRuntimePolicyV2(record)
}

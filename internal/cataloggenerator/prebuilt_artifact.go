package cataloggenerator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogartifactstore"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/prebuilt"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

const (
	derivedBottleCellar = "/home/linuxbrew/.linuxbrew/Cellar"

	// Keep this explicit until the catalog schema grows a distinct prebuilt
	// provenance-waiver constant. The current catalog validator accepts only the
	// checksum/JWS waiver for an artifact without verified provenance.
	prebuiltDerivedBottleProvenanceWaiver = catalog.PrebuiltProvenanceWaiver
)

type artifactFetcher interface {
	Probe(context.Context, string) (fetcher.ProbeResult, error)
	ProbeOptional(context.Context, string) (fetcher.ProbeResult, bool, error)
	Fetch(context.Context, fetcher.Request, io.Writer) (fetcher.Evidence, error)
	FetchObserved(context.Context, string, int64, string, []string, io.Writer) (fetcher.Evidence, error)
}

type generatedArtifactStore interface {
	Put(digest.Digest, int64, io.Reader) error
	Verify(digest.Digest, int64) error
}

type catalogArtifactStoreAdapter struct {
	store *catalogartifactstore.Store
}

func (a catalogArtifactStoreAdapter) Put(expected digest.Digest, size int64, source io.Reader) error {
	return a.store.Put(expected, size, source)
}

func (a catalogArtifactStoreAdapter) Verify(expected digest.Digest, size int64) error {
	artifact, err := a.store.Open(expected)
	if err != nil {
		return err
	}
	defer artifact.Close()
	if artifact.Size() != size {
		return fmt.Errorf("stored generated artifact size %d does not match expected size %d", artifact.Size(), size)
	}
	return nil
}

type prebuiltDeriver func(io.Reader, []byte, prebuilt.Profile) (*prebuilt.Result, error)
type bottleInspector func(io.Reader, bottle.Expectation, bottle.Options) (*bottle.CatalogInspection, error)

type selectedPrebuilt struct {
	policy      policyv2.PrebuiltArchivePolicy
	declaration catalog.PrebuiltArchiveFile
	profile     prebuilt.Profile
}

func (b *ProductionArtifactBuilder) buildExternalPrebuilt(ctx context.Context, request *catalog.Request, document *catalog.TapCatalog, formula catalog.Formula, node catalog.Node, platform catalog.Platform) (catalog.BottleArtifact, error) {
	if b.serviceOrigin == "" || b.artifactStore == nil {
		return catalog.BottleArtifact{}, fmt.Errorf("Formula %s has no supported native bottle and prebuilt artifact storage is unavailable", node.ID)
	}
	if b.derivePrebuilt == nil || b.inspectBottle == nil || b.tapPolicy == nil {
		return catalog.BottleArtifact{}, errors.New("prebuilt artifact generator is unavailable")
	}

	selected, err := b.selectPrebuilt(request, document, formula, node, platform, 0)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}

	sourceProbe, err := b.fetcher.Probe(ctx, selected.declaration.URL)
	if err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("probe prebuilt archive: %w", err)
	}
	if sourceProbe.Size <= 0 || sourceProbe.Size > selected.policy.Archive.MaxCompressedBytes {
		return catalog.BottleArtifact{}, fmt.Errorf("prebuilt archive size %d is outside 1..%d", sourceProbe.Size, selected.policy.Archive.MaxCompressedBytes)
	}
	sourceFilename, err := bottleFilename(selected.declaration.URL)
	if err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("prebuilt archive filename: %w", err)
	}
	sourceHosts := uniqueSorted(sourceProbe.RedirectHostSequence)
	sourceRequest := fetcher.Request{
		SchemaVersion:        fetcher.RequestSchemaVersion,
		FetchPolicyVersion:   fetcher.FetchPolicyVersion,
		ArtifactID:           string(node.ID) + ":prebuilt-source",
		URL:                  selected.declaration.URL,
		ExpectedSize:         sourceProbe.Size,
		SHA256:               strings.TrimPrefix(selected.declaration.SHA256, "sha256:"),
		Filename:             sourceFilename,
		AllowedRedirectHosts: sourceHosts,
	}
	if err := fetcher.ValidateRequest(sourceRequest); err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("prebuilt archive fetch request: %w", err)
	}
	sourceFile, err := os.CreateTemp("", "dalec-homebrew-prebuilt-source-")
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	sourcePath := sourceFile.Name()
	defer os.Remove(sourcePath)
	defer sourceFile.Close()
	sourceFetchEvidence, err := b.fetcher.Fetch(ctx, sourceRequest, sourceFile)
	if err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("fetch prebuilt archive: %w", err)
	}
	if err := fetcher.VerifyEvidence(sourceFetchEvidence, sourceRequest); err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("verify prebuilt archive fetch evidence: %w", err)
	}

	formulaSource, err := b.fetchExactTapFormula(ctx, document.Tap, formula)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	selected.profile.Source = prebuilt.SourceExpectation{Size: sourceProbe.Size, SHA256: selected.declaration.SHA256}
	selected.profile.SourceDateEpoch = document.PublishedAt.Unix()
	profileBytes, err := prebuilt.CanonicalProfile(selected.profile)
	if err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("canonicalize prebuilt profile: %w", err)
	}
	profileDigest := sha256Digest(profileBytes)

	if _, err := sourceFile.Seek(0, io.SeekStart); err != nil {
		return catalog.BottleArtifact{}, err
	}
	derived, err := b.derivePrebuilt(sourceFile, formulaSource, selected.profile)
	if err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("derive receiptless bottle: %w", err)
	}
	if err := validateDerivedResult(derived, selected, formulaSource, profileDigest); err != nil {
		return catalog.BottleArtifact{}, err
	}

	derivedDigest := sha256Digest(derived.Bottle)
	derivedSize := int64(len(derived.Bottle))
	tag := bottleTag(platform)
	derivedFilename := fmt.Sprintf("%s--%s.%s.bottle.tar.gz", node.Name, node.PkgVersion, tag)
	inspection, err := b.inspectBottle(bytes.NewReader(derived.Bottle), inspectionExpectation(node, tag, derivedDigest, derivedSize, strings.TrimPrefix(derivedDigest, "sha256:"), resolution.BottleTab{}), bottle.Options{})
	if err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("inspect derived bottle: %w", err)
	}
	if inspection.Formula.SHA256 != formula.SourceDigest || inspection.Formula.Size != int64(len(formulaSource)) {
		return catalog.BottleArtifact{}, errors.New("derived bottle Formula does not match authenticated tap source")
	}

	verification := catalog.BottleVerification{
		PolicyVersion:   catalog.BottleVerificationPolicy,
		InventoryDigest: inspection.InventorySHA256,
		EntryCount:      len(inspection.Inventory),
		ExpandedSize:    inspection.ExpandedSize,
	}
	tab := catalog.BottleTab{Receiptless: true, Arch: "x86_64"}
	if platform.Architecture == "arm64" {
		tab.Arch = "arm64"
	}
	provenance := catalog.Provenance{Waiver: &catalog.ProvenanceWaiver{Policy: prebuiltDerivedBottleProvenanceWaiver}}
	artifact := baseArtifact(node, platform, formula.SourceDigest, "", "", "", derivedFilename, tag, derivedBottleCellar, derivedDigest, derivedSize, inspection, tab, executablePathsFromInventory(inspection.Inventory), provenance)
	artifact.Transport.HTTPS = &catalog.HTTPSTransport{
		URL:                  b.derivedArtifactURL(derivedDigest),
		ExpectedSize:         derivedSize,
		SHA256:               derivedDigest,
		Filename:             derivedFilename,
		AllowedRedirectHosts: []string{serviceOriginHost(b.serviceOrigin)},
		FetchPolicyVersion:   catalog.HTTPSFetchPolicyVersion,
	}
	artifact.PrebuiltDerivation = mapPrebuiltDerivation(selected, b.tapPolicyDigest, document.Tap, formula, sourceFilename, sourceProbe.Size, sourceHosts, derivedFilename, verification, derived)
	if err := catalog.ValidatePrebuiltDerivationSource(*formula.PrebuiltArchive, tag, *artifact.PrebuiltDerivation); err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("validate prebuilt derivation source binding: %w", err)
	}
	if err := catalog.ValidateBottleArtifact(artifact); err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("validate derived bottle artifact: %w", err)
	}
	sourceDigest, err := digest.Parse(selected.declaration.SHA256)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	if _, err := sourceFile.Seek(0, io.SeekStart); err != nil {
		return catalog.BottleArtifact{}, err
	}
	if err := b.artifactStore.Put(sourceDigest, sourceProbe.Size, sourceFile); err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("persist verified prebuilt source archive: %w", err)
	}
	parsedDigest, err := digest.Parse(derivedDigest)
	if err != nil {
		return catalog.BottleArtifact{}, err
	}
	if err := b.artifactStore.Put(parsedDigest, derivedSize, bytes.NewReader(derived.Bottle)); err != nil {
		return catalog.BottleArtifact{}, fmt.Errorf("persist derived bottle: %w", err)
	}
	return artifact, nil
}

func (b *ProductionArtifactBuilder) selectPrebuilt(request *catalog.Request, document *catalog.TapCatalog, formula catalog.Formula, node catalog.Node, platform catalog.Platform, sourceSize int64) (selectedPrebuilt, error) {
	if document == nil || b.tapPolicy == nil {
		return selectedPrebuilt{}, errors.New("tap catalog and release policy are required")
	}
	policy, ok := b.tapPolicy.PrebuiltArchiveForFormula(string(node.ID))
	if !ok {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s has no supported native bottle or release-authorized prebuilt archive", node.ID)
	}
	if formula.PrebuiltArchive == nil {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s has no authenticated prebuilt archive declaration", node.ID)
	}
	if err := catalog.ValidatePrebuiltArchiveDeclaration(*formula.PrebuiltArchive); err != nil {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s prebuilt declaration: %w", node.ID, err)
	}
	if policy.FormulaID != string(node.ID) || policy.Version != formula.StableVersion || policy.FormulaSourceDigest != formula.SourceDigest || policy.License != formula.License {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s does not match its release-bound prebuilt policy", node.ID)
	}
	if policy.RequireNoBottle && formula.Bottle != nil {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s prebuilt policy requires no bottle declaration", node.ID)
	}
	if policy.RootOnly {
		root, err := requestHasExternalRoot(request, node.ID, platform)
		if err != nil {
			return selectedPrebuilt{}, err
		}
		if !root {
			return selectedPrebuilt{}, fmt.Errorf("Formula %s prebuilt policy permits direct requested roots only", node.ID)
		}
	}
	if err := validatePrebuiltDependencies(policy.Dependencies, formula, node, bottleTag(platform)); err != nil {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s: %w", node.ID, err)
	}
	if node.FormulaVersion != formula.StableVersion || node.FormulaRevision != formula.Revision || node.VersionScheme != formula.VersionScheme || node.PkgVersion == "" || node.BottleRebuild != 0 {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s node metadata does not match the no-bottle catalog declaration", node.ID)
	}

	platformKey := platform.OS + "/" + platform.Architecture
	var policyPlatform *policyv2.PrebuiltArchivePlatformPolicy
	for i := range policy.Platforms {
		if policy.Platforms[i].Platform == platformKey {
			policyPlatform = &policy.Platforms[i]
			break
		}
	}
	if policyPlatform == nil {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s prebuilt policy does not support %s", node.ID, platformKey)
	}
	tag := bottleTag(platform)
	var declaration *catalog.PrebuiltArchiveFile
	for i := range formula.PrebuiltArchive.Files {
		if formula.PrebuiltArchive.Files[i].Tag == tag {
			declaration = &formula.PrebuiltArchive.Files[i]
			break
		}
	}
	if declaration == nil {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s prebuilt archive is unavailable for %s", node.ID, platformKey)
	}
	if declaration.URL != policyPlatform.URL || declaration.SHA256 != policyPlatform.SHA256 || declaration.Format != policy.Archive.Format {
		return selectedPrebuilt{}, fmt.Errorf("Formula %s prebuilt declaration does not match release policy for %s", node.ID, platformKey)
	}

	profile, err := profileFromPolicy(policy, node, platform, sourceSize)
	if err != nil {
		return selectedPrebuilt{}, err
	}
	return selectedPrebuilt{policy: policy, declaration: *declaration, profile: profile}, nil
}

func profileFromPolicy(policy policyv2.PrebuiltArchivePolicy, node catalog.Node, platform catalog.Platform, sourceSize int64) (prebuilt.Profile, error) {
	limits := prebuilt.DefaultLimits()
	limits.MaxCompressedBytes = policy.Archive.MaxCompressedBytes
	limits.MaxExpandedBytes = policy.Archive.MaxExpandedBytes
	limits.MaxExpansionRatio = int64(policy.Archive.MaxExpansionRatio)
	limits.MaxEntries = policy.Archive.MaxEntries
	limits.MaxFileBytes = policy.Archive.MaxFileBytes
	limits.MaxPathBytes = policy.Archive.MaxPathBytes
	limits.MaxDepth = policy.Archive.MaxDepth
	entries := make([]prebuilt.EntryProfile, 0, len(policy.Archive.Members))
	for _, member := range policy.Archive.Members {
		mode, err := parsePrebuiltMode(member.Mode)
		if err != nil {
			return prebuilt.Profile{}, fmt.Errorf("prebuilt policy member %q mode: %w", member.Path, err)
		}
		entries = append(entries, prebuilt.EntryProfile{Path: member.Path, Mode: uint32(mode)})
	}
	installMode, err := parsePrebuiltMode(policy.Install.Mode)
	if err != nil {
		return prebuilt.Profile{}, fmt.Errorf("prebuilt policy install mode: %w", err)
	}
	if policy.Install.Destination != "bin/"+node.Name || installMode != 0o555 {
		return prebuilt.Profile{}, errors.New("prebuilt policy installation is not representable by the fixed receiptless-bottle derivation")
	}
	if policy.Binary.CGOEnabled == nil {
		return prebuilt.Profile{}, errors.New("prebuilt policy has no explicit CGO setting")
	}
	return prebuilt.Profile{
		PolicyVersion: policy.PolicyVersion,
		Name:          node.Name,
		PkgVersion:    node.PkgVersion,
		Target:        prebuilt.Target{OS: platform.OS, Arch: platform.Architecture},
		Source:        prebuilt.SourceExpectation{Size: sourceSize},
		FormulaSHA256: policy.FormulaSourceDigest,
		Entries:       entries,
		PayloadPath:   policy.Install.Source,
		GoBuild:       prebuilt.GoBuildProfile{ModulePath: policy.Binary.GoModule, CGOEnabled: *policy.Binary.CGOEnabled},
		Limits:        limits,
	}, nil
}

func (b *ProductionArtifactBuilder) fetchExactTapFormula(ctx context.Context, tap catalog.TapSource, formula catalog.Formula) ([]byte, error) {
	parsed, err := url.Parse(tap.Repository)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("tap Formula repository is not canonical GitHub HTTPS")
	}
	rawURL := "https://raw.githubusercontent.com" + parsed.Path + "/" + tap.Commit + "/" + formula.SourcePath
	probe, err := b.fetcher.Probe(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("probe authenticated tap Formula source: %w", err)
	}
	maxFormulaBytes := prebuilt.DefaultLimits().MaxFormulaBytes
	if probe.Size <= 0 || probe.Size > maxFormulaBytes {
		return nil, fmt.Errorf("tap Formula source size %d is outside 1..%d", probe.Size, maxFormulaBytes)
	}
	filename := path.Base(formula.SourcePath)
	request := fetcher.Request{
		SchemaVersion:        fetcher.RequestSchemaVersion,
		FetchPolicyVersion:   fetcher.FetchPolicyVersion,
		ArtifactID:           string(formula.ID) + ":formula-source",
		URL:                  rawURL,
		ExpectedSize:         probe.Size,
		SHA256:               strings.TrimPrefix(formula.SourceDigest, "sha256:"),
		Filename:             filename,
		AllowedRedirectHosts: uniqueSorted(probe.RedirectHostSequence),
	}
	if err := fetcher.ValidateRequest(request); err != nil {
		return nil, fmt.Errorf("tap Formula source request: %w", err)
	}
	var source bytes.Buffer
	evidence, err := b.fetcher.Fetch(ctx, request, &source)
	if err != nil {
		return nil, fmt.Errorf("fetch authenticated tap Formula source: %w", err)
	}
	if err := fetcher.VerifyEvidence(evidence, request); err != nil {
		return nil, fmt.Errorf("verify tap Formula source fetch evidence: %w", err)
	}
	if actual := sha256Digest(source.Bytes()); actual != formula.SourceDigest {
		return nil, fmt.Errorf("authenticated tap Formula source digest %s does not match catalog %s", actual, formula.SourceDigest)
	}
	return source.Bytes(), nil
}

func validateDerivedResult(result *prebuilt.Result, selected selectedPrebuilt, formulaSource []byte, profileDigest string) error {
	if result == nil || len(result.Bottle) == 0 {
		return errors.New("prebuilt derivation returned no bottle bytes")
	}
	evidence := result.Evidence
	if evidence.PolicyVersion != selected.policy.PolicyVersion || evidence.ProfileSHA256 != profileDigest {
		return errors.New("prebuilt derivation evidence does not match the selected policy/profile")
	}
	if evidence.Source.SHA256 != selected.declaration.SHA256 || evidence.Source.Size != selected.profile.Source.Size {
		return errors.New("prebuilt derivation source evidence does not match the fetched archive")
	}
	if evidence.Formula.SHA256 != selected.profile.FormulaSHA256 || evidence.Formula.Size != int64(len(formulaSource)) {
		return errors.New("prebuilt derivation Formula evidence does not match authenticated source")
	}
	if evidence.Source.PayloadPath != selected.policy.Install.Source || evidence.Derivation.ExecutablePath != selected.policy.Install.Destination || !evidence.Derivation.Receiptless {
		return errors.New("prebuilt derivation output does not match the fixed installation policy")
	}
	if evidence.Derivation.SHA256 != sha256Digest(result.Bottle) || evidence.Derivation.Size != int64(len(result.Bottle)) {
		return errors.New("prebuilt derivation bottle evidence does not match output bytes")
	}
	return nil
}

func mapPrebuiltDerivation(selected selectedPrebuilt, policyDigest string, tap catalog.TapSource, formula catalog.Formula, sourceFilename string, sourceSize int64, sourceHosts []string, derivedFilename string, verification catalog.BottleVerification, result *prebuilt.Result) *catalog.PrebuiltDerivation {
	evidence := result.Evidence
	archiveMode := uint32(0)
	for _, entry := range evidence.Source.Inventory {
		if entry.Path == selected.policy.Install.Source {
			archiveMode = entry.Mode
			break
		}
	}
	derivedMode, _ := parsePrebuiltMode(selected.policy.Install.Mode)
	machine := catalog.PrebuiltELFMachineX8664
	if selected.profile.Target.Arch == "arm64" {
		machine = catalog.PrebuiltELFMachineAArch64
	}
	return &catalog.PrebuiltDerivation{
		PolicyVersion: selected.policy.PolicyVersion,
		PolicyDigest:  policyDigest,
		Source: catalog.PrebuiltSourceArtifact{
			Filename: sourceFilename,
			Size:     sourceSize,
			SHA256:   selected.declaration.SHA256,
			Format:   selected.declaration.Format,
			Transport: catalog.Transport{HTTPS: &catalog.HTTPSTransport{
				URL:                  selected.declaration.URL,
				ExpectedSize:         sourceSize,
				SHA256:               selected.declaration.SHA256,
				Filename:             sourceFilename,
				AllowedRedirectHosts: slices.Clone(sourceHosts),
				FetchPolicyVersion:   catalog.HTTPSFetchPolicyVersion,
			}},
		},
		SourceInventory: catalog.PrebuiltSourceInventory{InventoryDigest: evidence.Source.InventorySHA256, EntryCount: len(evidence.Source.Inventory), ExpandedSize: evidence.Source.ExpandedSize},
		Payload: catalog.PrebuiltPayloadEvidence{
			SourcePath:      selected.policy.Install.Source,
			DestinationPath: selected.policy.Install.Destination,
			SHA256:          evidence.Source.PayloadSHA256,
			Size:            evidence.Source.PayloadSize,
			ArchiveMode:     archiveMode,
			DerivedMode:     uint32(derivedMode),
		},
		ELF: catalog.PrebuiltELFEvidence{
			Format:                     catalog.PrebuiltELFFormatELF64,
			Machine:                    machine,
			StaticallyLinked:           true,
			Interpreter:                evidence.ELF.Interpreter,
			NeededLibraries:            slices.Clone(evidence.ELF.ImportedLibraries),
			RPaths:                     []string{},
			WritableExecutableSegments: evidence.ELF.WritableExecutableSegments != 0,
		},
		FormulaSource: catalog.PrebuiltFormulaSourceEvidence{
			Transport: catalog.TapFormulaSourceTransport{Tap: tap, Path: formula.SourcePath},
			SHA256:    evidence.Formula.SHA256,
			Size:      evidence.Formula.Size,
		},
		RecipeDigest: evidence.ProfileSHA256,
		DerivedBottle: catalog.PrebuiltDerivedBottleRelation{
			Tag:                 bottleTag(selected.profilePlatform()),
			Filename:            derivedFilename,
			SHA256:              evidence.Derivation.SHA256,
			Size:                evidence.Derivation.Size,
			Verification:        verification,
			FormulaSourceDigest: evidence.Formula.SHA256,
		},
	}
}

func (s selectedPrebuilt) profilePlatform() catalog.Platform {
	return catalog.Platform{OS: s.profile.Target.OS, Architecture: s.profile.Target.Arch}
}

func (b *ProductionArtifactBuilder) derivedArtifactURL(value string) string {
	return b.serviceOrigin + catalogartifactstore.HTTPPathPrefix + strings.TrimPrefix(value, "sha256:")
}

func serviceOriginHost(origin string) string {
	parsed, _ := url.Parse(origin)
	return parsed.Hostname()
}

func requestHasExternalRoot(request *catalog.Request, id catalog.FormulaID, platform catalog.Platform) (bool, error) {
	targets, err := request.NormalizedTargets()
	if err != nil {
		return false, fmt.Errorf("normalize catalog request for prebuilt root policy: %w", err)
	}
	for _, target := range targets {
		if target.Platform != platform {
			continue
		}
		return slices.Contains(target.ExternalRoots, id), nil
	}
	return false, fmt.Errorf("catalog request has no target %s/%s", platform.OS, platform.Architecture)
}

func validatePrebuiltDependencies(expected []string, formula catalog.Formula, node catalog.Node, tag string) error {
	dependencies := formula.Dependencies
	for _, variation := range formula.Variations {
		if variation.Tag == tag {
			if variation.Unavailable {
				return fmt.Errorf("stable Formula is unavailable for %s", tag)
			}
			if variation.OverridesDependencies {
				dependencies = variation.Dependencies
			}
			break
		}
	}
	actual := make([]string, len(dependencies))
	for i, dependency := range dependencies {
		actual[i] = string(dependency.ID)
	}
	nodeDependencies := make([]string, len(node.Dependencies))
	for i, dependency := range node.Dependencies {
		nodeDependencies[i] = string(dependency.ID)
	}
	expected = slices.Clone(expected)
	slices.Sort(expected)
	slices.Sort(actual)
	slices.Sort(nodeDependencies)
	if !slices.Equal(actual, expected) || !slices.Equal(nodeDependencies, expected) {
		return fmt.Errorf("dependencies do not match release policy: catalog=%v node=%v policy=%v", actual, nodeDependencies, expected)
	}
	return nil
}

func parsePrebuiltMode(value string) (uint64, error) {
	if len(value) != 4 || value[0] != '0' {
		return 0, errors.New("mode must contain four canonical octal digits")
	}
	mode, err := strconv.ParseUint(value, 8, 12)
	if err != nil || mode > 0o777 {
		return 0, errors.New("mode contains invalid permission bits")
	}
	return mode, nil
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (b *ProductionArtifactBuilder) artifactCacheBinding() ([]byte, error) {
	return json.Marshal(struct {
		SchemaVersion    string `json:"schema_version"`
		ServiceOrigin    string `json:"service_origin,omitempty"`
		TapPolicyDigest  string `json:"tap_policy_digest"`
		DerivationPolicy string `json:"derivation_policy"`
		ProvenanceWaiver string `json:"provenance_waiver"`
	}{
		SchemaVersion:    "dalec-homebrew-production-artifact-builder/v2",
		ServiceOrigin:    b.serviceOrigin,
		TapPolicyDigest:  b.tapPolicyDigest,
		DerivationPolicy: prebuilt.DerivationPolicyVersion,
		ProvenanceWaiver: prebuiltDerivedBottleProvenanceWaiver,
	})
}

func (b *ProductionArtifactBuilder) validateCachedArtifact(request *catalog.Request, catalogs map[catalog.TapID]*catalog.TapCatalog, node catalog.Node, platform catalog.Platform, artifact catalog.BottleArtifact) error {
	if artifact.PrebuiltDerivation == nil {
		return nil
	}
	document := catalogs[node.ID.Tap()]
	if document == nil {
		return fmt.Errorf("tap catalog %s is unavailable", node.ID.Tap())
	}
	var formula *catalog.Formula
	for i := range document.Formulae {
		if document.Formulae[i].ID == node.ID {
			formula = &document.Formulae[i]
			break
		}
	}
	if formula == nil {
		return fmt.Errorf("Formula %s is unavailable", node.ID)
	}
	if formula.Bottle != nil {
		if _, ok := selectBottleFile(formula.Bottle.Files, bottleTag(platform)); ok {
			return errors.New("cached prebuilt artifact cannot replace an available native bottle")
		}
	}
	if _, err := b.selectPrebuilt(request, document, *formula, node, platform, artifact.PrebuiltDerivation.Source.Size); err != nil {
		return err
	}
	if err := catalog.ValidatePrebuiltDerivationSource(*formula.PrebuiltArchive, bottleTag(platform), *artifact.PrebuiltDerivation); err != nil {
		return err
	}
	if b.serviceOrigin == "" || b.artifactStore == nil || artifact.Transport.HTTPS == nil || artifact.Transport.HTTPS.URL != b.derivedArtifactURL(artifact.SHA256) {
		return errors.New("cached prebuilt artifact is not bound to the configured catalog service store")
	}
	sourceDigest, err := digest.Parse(artifact.PrebuiltDerivation.Source.SHA256)
	if err != nil {
		return err
	}
	if err := b.artifactStore.Verify(sourceDigest, artifact.PrebuiltDerivation.Source.Size); err != nil {
		return fmt.Errorf("verify cached prebuilt source archive: %w", err)
	}
	parsed, err := digest.Parse(artifact.SHA256)
	if err != nil {
		return err
	}
	return b.artifactStore.Verify(parsed, artifact.Size)
}

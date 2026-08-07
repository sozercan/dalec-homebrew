package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/distribution/reference"
)

// ValidateRequest validates a catalog-set request without performing network
// access.
func ValidateRequest(request *Request) error {
	if request == nil {
		return errors.New("nil catalog request")
	}
	var errs []error
	if request.SchemaVersion != RequestSchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q", request.SchemaVersion))
	}
	targets, targetErr := normalizedRequestTargets(request)
	if targetErr != nil {
		errs = append(errs, targetErr)
	}
	if len(targets) == 0 {
		errs = append(errs, errors.New("targets must be a non-empty array"))
	}
	if len(targets) > 2 {
		errs = append(errs, fmt.Errorf("targets has %d entries, limit is 2", len(targets)))
	}
	seenPlatforms := make(map[string]struct{}, len(targets))
	seenRoots := make(map[FormulaID]struct{})
	seenTaps := make(map[TapID]struct{})
	rootAssociations := 0
	for targetIndex, target := range targets {
		if err := target.Platform.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("targets[%d].platform: %w", targetIndex, err))
		} else {
			key := target.Platform.key()
			if _, duplicate := seenPlatforms[key]; duplicate {
				errs = append(errs, fmt.Errorf("duplicate target platform %s", key))
			}
			seenPlatforms[key] = struct{}{}
		}
		if target.ExternalRoots == nil {
			errs = append(errs, fmt.Errorf("targets[%d].external_roots must be an array", targetIndex))
		}
		targetRoots := make(map[FormulaID]struct{}, len(target.ExternalRoots))
		for rootIndex, id := range target.ExternalRoots {
			rootAssociations++
			if err := id.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("targets[%d].external_roots[%d]: %w", targetIndex, rootIndex, err))
				continue
			}
			if id.IsCore() {
				errs = append(errs, fmt.Errorf("targets[%d].external_roots[%d] %q belongs to homebrew/core", targetIndex, rootIndex, id))
			}
			if _, duplicate := targetRoots[id]; duplicate {
				errs = append(errs, fmt.Errorf("duplicate external root %q for platform %s", id, target.Platform.key()))
			}
			targetRoots[id] = struct{}{}
			seenRoots[id] = struct{}{}
			seenTaps[id.Tap()] = struct{}{}
		}
		coreRoots := make(map[FormulaID]struct{}, len(target.CoreRoots))
		for rootIndex, id := range target.CoreRoots {
			if err := id.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("targets[%d].core_roots[%d]: %w", targetIndex, rootIndex, err))
				continue
			}
			if !id.IsCore() {
				errs = append(errs, fmt.Errorf("targets[%d].core_roots[%d] %q is not homebrew/core", targetIndex, rootIndex, id))
			}
			if _, duplicate := coreRoots[id]; duplicate {
				errs = append(errs, fmt.Errorf("duplicate core root %q for platform %s", id, target.Platform.key()))
			}
			coreRoots[id] = struct{}{}
			seenRoots[id] = struct{}{}
		}
	}
	if rootAssociations == 0 {
		errs = append(errs, errors.New("request has no external roots for any target"))
	}
	if len(seenRoots) > MaxClosureNodes {
		errs = append(errs, fmt.Errorf("request has %d unique roots, limit is %d", len(seenRoots), MaxClosureNodes))
	}
	if len(seenTaps) > MaxTaps {
		errs = append(errs, fmt.Errorf("request reaches %d root taps, limit is %d", len(seenTaps), MaxTaps))
	}
	if err := validateCommit(request.HomebrewCommit); err != nil {
		errs = append(errs, fmt.Errorf("homebrew_commit: %w", err))
	}
	if err := validateSHA256Digest(request.CoreSnapshotDigest); err != nil {
		errs = append(errs, fmt.Errorf("core_snapshot_digest: %w", err))
	}
	return errors.Join(errs...)
}

// NormalizedTargets returns a caller-owned, deterministically ordered copy of
// the request's per-platform external-root bindings.
func (request *Request) NormalizedTargets() ([]PlatformRequest, error) {
	if err := ValidateRequest(request); err != nil {
		return nil, err
	}
	targets, err := normalizedRequestTargets(request)
	if err != nil {
		return nil, err
	}
	for i := range targets {
		slices.Sort(targets[i].ExternalRoots)
		slices.Sort(targets[i].CoreRoots)
	}
	slices.SortFunc(targets, func(left, right PlatformRequest) int {
		return comparePlatform(left.Platform, right.Platform)
	})
	return targets, nil
}

func normalizedRequestTargets(request *Request) ([]PlatformRequest, error) {
	if request == nil {
		return nil, errors.New("nil catalog request")
	}
	if len(request.Targets) > 0 {
		if len(request.Platforms) > 0 || len(request.ExternalRoots) > 0 {
			return nil, errors.New("request cannot mix targets with legacy platforms/external_roots fields")
		}
		return clonePlatformRequests(request.Targets), nil
	}
	if len(request.Platforms) == 0 && len(request.ExternalRoots) == 0 {
		return nil, nil
	}
	if len(request.Platforms) == 0 || len(request.ExternalRoots) == 0 {
		return nil, errors.New("legacy request compatibility requires both platforms and external_roots")
	}
	targets := make([]PlatformRequest, len(request.Platforms))
	for i, platform := range request.Platforms {
		targets[i] = PlatformRequest{Platform: platform, ExternalRoots: slices.Clone(request.ExternalRoots)}
	}
	return targets, nil
}

func clonePlatformRequests(targets []PlatformRequest) []PlatformRequest {
	cloned := make([]PlatformRequest, len(targets))
	for i, target := range targets {
		cloned[i] = PlatformRequest{Platform: target.Platform, ExternalRoots: slices.Clone(target.ExternalRoots), CoreRoots: slices.Clone(target.CoreRoots)}
	}
	return cloned
}

// Validate reports whether platform is one of the two V2 Linux targets.
func (platform Platform) Validate() error {
	if platform.OS != "linux" || (platform.Architecture != "amd64" && platform.Architecture != "arm64") || platform.Variant != "" {
		return fmt.Errorf("unsupported platform %s/%s variant %q", platform.OS, platform.Architecture, platform.Variant)
	}
	return nil
}

func (platform Platform) key() string { return platform.OS + "/" + platform.Architecture }

func (platform Platform) bottleTag() string {
	if platform.OS != "linux" {
		return ""
	}
	switch platform.Architecture {
	case "amd64":
		return "x86_64_linux"
	case "arm64":
		return "arm64_linux"
	default:
		return ""
	}
}

// ValidateTapCatalog validates a complete exact-commit tap catalog.
func ValidateTapCatalog(catalog *TapCatalog) error {
	if catalog == nil {
		return errors.New("nil tap catalog")
	}
	var errs []error
	if catalog.SchemaVersion != TapCatalogSchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q", catalog.SchemaVersion))
	}
	if err := validateTapSource(catalog.Tap); err != nil {
		errs = append(errs, fmt.Errorf("tap: %w", err))
	}
	if catalog.PublishedAt.IsZero() {
		errs = append(errs, errors.New("published_at is required"))
	}
	if catalog.Sequence == 0 {
		errs = append(errs, errors.New("sequence must be positive"))
	}
	if catalog.Formulae == nil || len(catalog.Formulae) == 0 {
		errs = append(errs, errors.New("formulae must be a non-empty array"))
	}

	formulae := make(map[FormulaID]struct{}, len(catalog.Formulae))
	racks := make(map[string]FormulaID, len(catalog.Formulae))
	for i, formula := range catalog.Formulae {
		if err := validateFormula(formula, catalog.Tap.ID); err != nil {
			errs = append(errs, fmt.Errorf("formulae[%d]: %w", i, err))
		}
		if _, duplicate := formulae[formula.ID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate Formula %q", formula.ID))
		}
		formulae[formula.ID] = struct{}{}
		if prior, duplicate := racks[formula.Name]; duplicate && prior != formula.ID {
			errs = append(errs, fmt.Errorf("Formulae %q and %q share rack name %q", prior, formula.ID, formula.Name))
		}
		racks[formula.Name] = formula.ID
	}

	mappingSources := make(map[FormulaID]string)
	validateMappings := func(kind string, mappings []ScopedMapping) {
		for i, mapping := range mappings {
			if err := validateScopedMapping(mapping, catalog.Tap.ID, formulae); err != nil {
				errs = append(errs, fmt.Errorf("%s[%d]: %w", kind, i, err))
			}
			if prior, duplicate := mappingSources[mapping.From]; duplicate {
				errs = append(errs, fmt.Errorf("mapping source %q appears in %s and %s", mapping.From, prior, kind))
			}
			mappingSources[mapping.From] = kind
		}
	}
	validateMappings("aliases", catalog.Aliases)
	validateMappings("renames", catalog.Renames)
	for i, migration := range catalog.Migrations {
		if err := validateMigration(migration, catalog.Tap.ID, formulae); err != nil {
			errs = append(errs, fmt.Errorf("migrations[%d]: %w", i, err))
		}
		if prior, duplicate := mappingSources[migration.From]; duplicate {
			errs = append(errs, fmt.Errorf("mapping source %q appears in %s and migrations", migration.From, prior))
		}
		mappingSources[migration.From] = "migrations"
	}
	for _, formula := range catalog.Formulae {
		seen := make(map[FormulaID]struct{}, len(formula.VersionedFormulae))
		for _, versioned := range formula.VersionedFormulae {
			if _, duplicate := seen[versioned]; duplicate {
				errs = append(errs, fmt.Errorf("Formula %q has duplicate versioned Formula %q", formula.ID, versioned))
			}
			seen[versioned] = struct{}{}
			if versioned == formula.ID {
				errs = append(errs, fmt.Errorf("Formula %q lists itself as versioned", formula.ID))
			}
			if versioned.Tap() != catalog.Tap.ID {
				errs = append(errs, fmt.Errorf("Formula %q versioned target %q leaves tap", formula.ID, versioned))
			}
			if _, ok := formulae[versioned]; !ok {
				errs = append(errs, fmt.Errorf("Formula %q versioned target %q is missing", formula.ID, versioned))
			}
		}
	}
	return errors.Join(errs...)
}

func validateTapSource(source TapSource) error {
	var errs []error
	if err := source.ID.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("id: %w", err))
	} else if source.ID.IsCore() {
		errs = append(errs, errors.New("homebrew/core is authenticated separately and cannot be a tap catalog source"))
	}
	if want := source.ID.DefaultGitHubRepository(); source.Repository != want {
		errs = append(errs, fmt.Errorf("repository %q does not match default GitHub repository %q", source.Repository, want))
	}
	if err := validateCommit(source.Commit); err != nil {
		errs = append(errs, fmt.Errorf("commit: %w", err))
	}
	if err := validateSHA256Digest(source.TreeDigest); err != nil {
		errs = append(errs, fmt.Errorf("tree_digest: %w", err))
	}
	if err := validateSHA256Digest(source.ArchiveDigest); err != nil {
		errs = append(errs, fmt.Errorf("archive_digest: %w", err))
	}
	return errors.Join(errs...)
}

func validateFormula(formula Formula, tap TapID) error {
	var errs []error
	if err := formula.ID.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("id: %w", err))
	}
	if formula.ID.Tap() != tap {
		errs = append(errs, fmt.Errorf("Formula %q does not belong to tap %q", formula.ID, tap))
	}
	if err := validateFormulaName(formula.Name); err != nil {
		errs = append(errs, fmt.Errorf("name: %w", err))
	}
	if formula.Name != formula.ID.Name() {
		errs = append(errs, fmt.Errorf("name %q does not match Formula ID %q", formula.Name, formula.ID))
	}
	if formula.HomebrewFullName != string(formula.ID) {
		errs = append(errs, fmt.Errorf("homebrew_full_name %q does not match Formula ID %q", formula.HomebrewFullName, formula.ID))
	}
	if err := validateFormulaSourcePath(formula.SourcePath, formula.Name); err != nil {
		errs = append(errs, fmt.Errorf("source_path: %w", err))
	}
	if err := validateSHA256Digest(formula.SourceDigest); err != nil {
		errs = append(errs, fmt.Errorf("source_digest: %w", err))
	}
	if strings.TrimSpace(formula.StableVersion) == "" || hasControl(formula.StableVersion) {
		errs = append(errs, errors.New("stable_version is required and cannot contain control characters"))
	}
	if formula.Revision < 0 || formula.VersionScheme < 0 {
		errs = append(errs, errors.New("revision and version_scheme must be non-negative"))
	}
	if hasControl(formula.License) {
		errs = append(errs, errors.New("license cannot contain control characters"))
	}
	if err := validateDependencies(formula.Dependencies, tap); err != nil {
		errs = append(errs, fmt.Errorf("dependencies: %w", err))
	}
	seenVariations := map[string]struct{}{}
	for i, variation := range formula.Variations {
		if !validBottleTag(variation.Tag) {
			errs = append(errs, fmt.Errorf("variations[%d] has unsupported tag %q", i, variation.Tag))
		}
		if _, duplicate := seenVariations[variation.Tag]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate variation tag %q", variation.Tag))
		}
		seenVariations[variation.Tag] = struct{}{}
		if variation.Unavailable && (len(variation.Dependencies) != 0 || variation.OverridesDependencies || variation.KegOnly || variation.OverridesKegOnly) {
			errs = append(errs, fmt.Errorf("variation %q is unavailable but also overrides Formula fields", variation.Tag))
		}
		if len(variation.Dependencies) > 0 && !variation.OverridesDependencies {
			errs = append(errs, fmt.Errorf("variation %q has dependencies without overrides_dependencies", variation.Tag))
		}
		if variation.KegOnly && !variation.OverridesKegOnly {
			errs = append(errs, fmt.Errorf("variation %q sets keg_only without overrides_keg_only", variation.Tag))
		}
		if err := validateDependencies(variation.Dependencies, tap); err != nil {
			errs = append(errs, fmt.Errorf("variation %q dependencies: %w", variation.Tag, err))
		}
	}
	seenVersioned := make(map[FormulaID]struct{}, len(formula.VersionedFormulae))
	for i, id := range formula.VersionedFormulae {
		if err := id.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("versioned_formulae[%d]: %w", i, err))
		}
		if _, duplicate := seenVersioned[id]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate versioned Formula %q", id))
		}
		seenVersioned[id] = struct{}{}
	}
	if formula.Bottle != nil {
		if err := validateBottleDeclaration(*formula.Bottle); err != nil {
			errs = append(errs, fmt.Errorf("bottle: %w", err))
		}
	}
	if formula.PrebuiltArchive != nil {
		if err := ValidatePrebuiltArchiveDeclaration(*formula.PrebuiltArchive); err != nil {
			errs = append(errs, fmt.Errorf("prebuilt_archive: %w", err))
		}
	}
	if formula.Bottle != nil && formula.PrebuiltArchive != nil {
		bottleTags := make(map[string]struct{}, len(formula.Bottle.Files))
		for _, file := range formula.Bottle.Files {
			bottleTags[file.Tag] = struct{}{}
		}
		for _, file := range formula.PrebuiltArchive.Files {
			_, exact := bottleTags[file.Tag]
			_, all := bottleTags["all"]
			if exact || all {
				errs = append(errs, fmt.Errorf("tag %q is declared as both a native bottle and prebuilt archive", file.Tag))
			}
		}
	}
	return errors.Join(errs...)
}

func validateDependencies(dependencies []Dependency, owner TapID) error {
	var errs []error
	seen := make(map[FormulaID]struct{}, len(dependencies))
	for i, dependency := range dependencies {
		if err := dependency.ID.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("[%d].id: %w", i, err))
		}
		if dependency.Raw == "" || hasControl(dependency.Raw) || len(dependency.Raw) > MaxFormulaIDBytes {
			errs = append(errs, fmt.Errorf("[%d].raw is empty, overlong, or contains control characters", i))
		} else {
			switch strings.Count(dependency.Raw, "/") {
			case 0:
				if err := validateFormulaName(dependency.Raw); err != nil {
					errs = append(errs, fmt.Errorf("[%d].raw: %w", i, err))
				}
				if dependency.Raw != dependency.ID.Name() {
					errs = append(errs, fmt.Errorf("[%d] bare dependency %q does not match normalized Formula name %q", i, dependency.Raw, dependency.ID.Name()))
				}
				if dependency.ID.Tap() != TapID("homebrew/core") && dependency.ID.Tap() != owner {
					errs = append(errs, fmt.Errorf("[%d] bare dependency %q normalizes to unrelated tap %q", i, dependency.Raw, dependency.ID.Tap()))
				}
			case 2:
				parsed, err := ParseFormulaID(dependency.Raw)
				if err != nil {
					errs = append(errs, fmt.Errorf("[%d].raw: %w", i, err))
				} else if parsed != dependency.ID {
					errs = append(errs, fmt.Errorf("[%d].raw %q does not match normalized dependency %q", i, dependency.Raw, dependency.ID))
				}
			default:
				errs = append(errs, fmt.Errorf("[%d].raw %q is neither bare nor fully qualified", i, dependency.Raw))
			}
		}
		if _, duplicate := seen[dependency.ID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate normalized dependency %q", dependency.ID))
		}
		seen[dependency.ID] = struct{}{}
	}
	return errors.Join(errs...)
}

// ValidatePrebuiltArchiveDeclaration validates a policy-eligible stable-source
// declaration. Authorization remains a separate release-policy decision.
func ValidatePrebuiltArchiveDeclaration(declaration PrebuiltArchiveDeclaration) error {
	var errs []error
	if declaration.Files == nil || len(declaration.Files) == 0 {
		errs = append(errs, errors.New("files must be a non-empty array"))
	}
	if len(declaration.Files) > 2 {
		errs = append(errs, fmt.Errorf("files has %d entries, limit is 2", len(declaration.Files)))
	}
	seen := make(map[string]struct{}, len(declaration.Files))
	for i, file := range declaration.Files {
		if file.Tag != "x86_64_linux" && file.Tag != "arm64_linux" {
			errs = append(errs, fmt.Errorf("files[%d] has unsupported tag %q", i, file.Tag))
		}
		if _, duplicate := seen[file.Tag]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate prebuilt archive tag %q", file.Tag))
		}
		seen[file.Tag] = struct{}{}
		if err := validateHTTPSURL(file.URL, true); err != nil {
			errs = append(errs, fmt.Errorf("files[%d].url: %w", i, err))
		}
		if err := validateSHA256Digest(file.SHA256); err != nil {
			errs = append(errs, fmt.Errorf("files[%d].sha256: %w", i, err))
		}
		if err := validatePrebuiltArchiveFormat(file.Format, file.URL); err != nil {
			errs = append(errs, fmt.Errorf("files[%d].format: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func validateBottleDeclaration(bottle BottleDeclaration) error {
	var errs []error
	if err := validateHTTPSURL(bottle.RootURL, false); err != nil {
		errs = append(errs, fmt.Errorf("root_url: %w", err))
	}
	if bottle.Rebuild < 0 {
		errs = append(errs, errors.New("rebuild must be non-negative"))
	}
	if bottle.Files == nil || len(bottle.Files) == 0 {
		errs = append(errs, errors.New("files must be a non-empty array"))
	}
	seen := make(map[string]struct{}, len(bottle.Files))
	for i, file := range bottle.Files {
		if !validBottleTag(file.Tag) {
			errs = append(errs, fmt.Errorf("files[%d] has unsupported tag %q", i, file.Tag))
		}
		if _, duplicate := seen[file.Tag]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate bottle tag %q", file.Tag))
		}
		seen[file.Tag] = struct{}{}
		if err := validateHTTPSURL(file.URL, true); err != nil {
			errs = append(errs, fmt.Errorf("files[%d].url: %w", i, err))
		}
		if err := validateSHA256Digest(file.SHA256); err != nil {
			errs = append(errs, fmt.Errorf("files[%d].sha256: %w", i, err))
		}
		if err := validateCellarPolicy(file.Cellar); err != nil {
			errs = append(errs, fmt.Errorf("files[%d].cellar: %w", i, err))
		}
	}
	return errors.Join(errs...)
}

func validateScopedMapping(mapping ScopedMapping, tap TapID, formulae map[FormulaID]struct{}) error {
	var errs []error
	for _, field := range []struct {
		name string
		id   FormulaID
	}{{"from", mapping.From}, {"to", mapping.To}} {
		if err := field.id.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", field.name, err))
		}
		if field.id.Tap() != tap {
			errs = append(errs, fmt.Errorf("%s %q leaves tap %q", field.name, field.id, tap))
		}
	}
	if mapping.From == mapping.To {
		errs = append(errs, errors.New("mapping source and target are identical"))
	}
	if _, exists := formulae[mapping.From]; exists {
		errs = append(errs, fmt.Errorf("mapping source %q is also a canonical Formula", mapping.From))
	}
	if _, exists := formulae[mapping.To]; !exists {
		errs = append(errs, fmt.Errorf("mapping target %q is not a canonical Formula", mapping.To))
	}
	return errors.Join(errs...)
}

func validateMigration(migration Migration, tap TapID, formulae map[FormulaID]struct{}) error {
	var errs []error
	if err := migration.From.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("from: %w", err))
	}
	if migration.From.Tap() != tap {
		errs = append(errs, fmt.Errorf("migration source %q leaves tap %q", migration.From, tap))
	}
	if _, exists := formulae[migration.From]; exists {
		errs = append(errs, fmt.Errorf("migration source %q is also a canonical Formula", migration.From))
	}
	if strings.Count(migration.RawTarget, "/") != 2 {
		errs = append(errs, fmt.Errorf("migration target %q must be fully qualified", migration.RawTarget))
	} else if parsed, err := ParseFormulaID(migration.RawTarget); err != nil {
		errs = append(errs, fmt.Errorf("raw_target: %w", err))
	} else if parsed != migration.To {
		errs = append(errs, fmt.Errorf("raw_target %q does not match normalized target %q", migration.RawTarget, migration.To))
	}
	if err := migration.To.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("to: %w", err))
	}
	if migration.From == migration.To {
		errs = append(errs, errors.New("migration source and target are identical"))
	}
	if migration.To.Tap() == tap {
		if _, exists := formulae[migration.To]; !exists {
			errs = append(errs, fmt.Errorf("same-tap migration target %q is not a canonical Formula", migration.To))
		}
	}
	return errors.Join(errs...)
}

// ValidateCatalogSetPayload validates the authenticated payload structure and
// its graph/catalog limits. It does not verify the surrounding JWS.
func ValidateCatalogSetPayload(payload *CatalogSetPayload) error {
	if payload == nil {
		return errors.New("nil catalog-set payload")
	}
	var errs []error
	if payload.SchemaVersion != CatalogSetSchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q", payload.SchemaVersion))
	}
	if err := validateSHA256Digest(payload.RequestDigest); err != nil {
		errs = append(errs, fmt.Errorf("request_digest: %w", err))
	}
	if err := validateSHA256Digest(payload.CoreSnapshotDigest); err != nil {
		errs = append(errs, fmt.Errorf("core_snapshot_digest: %w", err))
	}
	if payload.GeneratedAt.IsZero() || payload.ExpiresAt.IsZero() {
		errs = append(errs, errors.New("generated_at and expires_at are required"))
	} else if !payload.ExpiresAt.After(payload.GeneratedAt) {
		errs = append(errs, errors.New("expires_at must be after generated_at"))
	} else if payload.ExpiresAt.Sub(payload.GeneratedAt) > MaxCatalogSetLifetime {
		errs = append(errs, fmt.Errorf("catalog-set lifetime exceeds %s", MaxCatalogSetLifetime))
	}
	if err := validateComponentIdentity(payload.CatalogService); err != nil {
		errs = append(errs, fmt.Errorf("catalog_service: %w", err))
	}
	if err := validateComponentIdentity(payload.Extractor); err != nil {
		errs = append(errs, fmt.Errorf("extractor: %w", err))
	}
	if payload.Catalogs == nil || len(payload.Catalogs) == 0 {
		errs = append(errs, errors.New("catalogs must be a non-empty array"))
	}
	if len(payload.Catalogs) > MaxTaps {
		errs = append(errs, fmt.Errorf("catalogs has %d entries, limit is %d", len(payload.Catalogs), MaxTaps))
	}
	catalogTaps := make(map[TapID]struct{}, len(payload.Catalogs))
	catalogDigests := make(map[string]TapID, len(payload.Catalogs))
	catalogURLs := make(map[string]TapID, len(payload.Catalogs))
	var aggregate int64
	for i, reference := range payload.Catalogs {
		if err := ValidateCatalogReference(reference); err != nil {
			errs = append(errs, fmt.Errorf("catalogs[%d]: %w", i, err))
		}
		if _, duplicate := catalogTaps[reference.Tap.ID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate catalog tap %q", reference.Tap.ID))
		}
		catalogTaps[reference.Tap.ID] = struct{}{}
		if prior, duplicate := catalogDigests[reference.SHA256]; duplicate {
			errs = append(errs, fmt.Errorf("catalog taps %q and %q reuse digest %q", prior, reference.Tap.ID, reference.SHA256))
		}
		catalogDigests[reference.SHA256] = reference.Tap.ID
		if prior, duplicate := catalogURLs[reference.URL]; duplicate {
			errs = append(errs, fmt.Errorf("catalog taps %q and %q reuse URL %q", prior, reference.Tap.ID, reference.URL))
		}
		catalogURLs[reference.URL] = reference.Tap.ID
		if !payload.GeneratedAt.IsZero() && reference.PublishedAt.After(payload.GeneratedAt) {
			errs = append(errs, fmt.Errorf("catalog %q published_at is after catalog-set generated_at", reference.Tap.ID))
		}
		if reference.Size > 0 {
			if aggregate > MaxAggregateCatalogBytes-reference.Size {
				aggregate = MaxAggregateCatalogBytes + 1
			} else {
				aggregate += reference.Size
			}
		}
	}
	if aggregate > MaxAggregateCatalogBytes {
		errs = append(errs, fmt.Errorf("aggregate catalog size exceeds %d bytes", MaxAggregateCatalogBytes))
	}
	if payload.Results == nil || len(payload.Results) == 0 {
		errs = append(errs, errors.New("results must be a non-empty array"))
	}
	if len(payload.Results) > 2 {
		errs = append(errs, fmt.Errorf("results has %d entries, limit is 2", len(payload.Results)))
	}
	seenPlatforms := make(map[string]struct{}, len(payload.Results))
	reachedTaps := make(map[TapID]struct{})
	unionNodes := make(map[FormulaID]struct{})
	rootVersions := make(map[FormulaID]rootVersionIdentity)
	for i, result := range payload.Results {
		if err := ValidatePlatformResult(result); err != nil {
			errs = append(errs, fmt.Errorf("results[%d]: %w", i, err))
		}
		key := result.Platform.key()
		if _, duplicate := seenPlatforms[key]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate result platform %s", key))
		}
		seenPlatforms[key] = struct{}{}
		nodesByID := make(map[FormulaID]Node, len(result.Closure.Nodes))
		for _, node := range result.Closure.Nodes {
			nodesByID[node.ID] = node
		}
		mappings := result.Closure.RequestedMappings
		if mappings == nil {
			mappings = make([]RequestedMapping, len(result.Closure.Requested))
			for i, id := range result.Closure.Requested {
				mappings[i] = RequestedMapping{Requested: id, Resolved: id}
			}
		}
		for _, mapping := range mappings {
			if node, present := nodesByID[mapping.Resolved]; present {
				identity := rootVersionFor(node)
				identity.Resolved = mapping.Resolved
				if prior, seen := rootVersions[mapping.Requested]; seen && prior != identity {
					errs = append(errs, fmt.Errorf("requested root %q has inconsistent versions or resolved identity across platforms", mapping.Requested))
				} else {
					rootVersions[mapping.Requested] = identity
				}
			}
		}
		for _, tap := range normalizedClosureTaps(result.Closure) {
			reachedTaps[tap] = struct{}{}
		}
		for _, node := range result.Closure.Nodes {
			unionNodes[node.ID] = struct{}{}
		}
	}
	if len(unionNodes) > MaxClosureNodes {
		errs = append(errs, fmt.Errorf("cross-platform closure contains %d nodes, limit is %d", len(unionNodes), MaxClosureNodes))
	}
	for _, tap := range sortedTapIDs(reachedTaps) {
		if _, present := catalogTaps[tap]; !present {
			errs = append(errs, fmt.Errorf("reached tap %q has no signed catalog reference", tap))
		}
	}
	for _, tap := range sortedTapIDs(catalogTaps) {
		if _, reached := reachedTaps[tap]; !reached {
			errs = append(errs, fmt.Errorf("catalog reference for tap %q is not reached by any result", tap))
		}
	}
	return errors.Join(errs...)
}

type rootVersionIdentity struct {
	Resolved         FormulaID
	Tap              TapID
	Name             string
	HomebrewFullName string
	FormulaVersion   string
	FormulaRevision  int
	PkgVersion       string
	VersionScheme    int
	BottleRebuild    int
	License          string
}

func rootVersionFor(node Node) rootVersionIdentity {
	return rootVersionIdentity{
		Tap: node.Tap, Name: node.Name, HomebrewFullName: node.HomebrewFullName,
		FormulaVersion: node.FormulaVersion, FormulaRevision: node.FormulaRevision,
		PkgVersion: node.PkgVersion, VersionScheme: node.VersionScheme,
		BottleRebuild: node.BottleRebuild, License: node.License,
	}
}

// ValidateCatalogSetAt applies expiry and not-before checks at a caller-supplied
// time after structural validation.
func ValidateCatalogSetAt(payload *CatalogSetPayload, now time.Time) error {
	if err := ValidateCatalogSetPayload(payload); err != nil {
		return err
	}
	if now.IsZero() {
		return errors.New("validation time is required")
	}
	now = now.UTC()
	if now.Before(payload.GeneratedAt) {
		return fmt.Errorf("catalog set is not valid before %s", payload.GeneratedAt.UTC())
	}
	if !now.Before(payload.ExpiresAt) {
		return fmt.Errorf("catalog set expired at %s", payload.ExpiresAt.UTC())
	}
	return nil
}

// ValidateCatalogReference validates one signed catalog locator and its per-doc
// size bound.
func ValidateCatalogReference(reference CatalogReference) error {
	var errs []error
	if err := validateTapSource(reference.Tap); err != nil {
		errs = append(errs, fmt.Errorf("tap: %w", err))
	}
	if reference.PublishedAt.IsZero() {
		errs = append(errs, errors.New("published_at is required"))
	}
	if reference.Sequence == 0 {
		errs = append(errs, errors.New("sequence must be positive"))
	}
	if reference.Size <= 0 || reference.Size > MaxCatalogDocumentBytes {
		errs = append(errs, fmt.Errorf("size %d is outside 1..%d", reference.Size, MaxCatalogDocumentBytes))
	}
	if err := validateSHA256Digest(reference.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("sha256: %w", err))
	}
	if err := validateCatalogURL(reference.URL, reference.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("url: %w", err))
	}
	return errors.Join(errs...)
}

func validateComponentIdentity(identity ComponentIdentity) error {
	var errs []error
	if err := validateBoundedText("name", identity.Name, 128, false); err != nil {
		errs = append(errs, err)
	}
	if err := validateBoundedText("version", identity.Version, 256, false); err != nil {
		errs = append(errs, err)
	}
	if err := validateSHA256Digest(identity.Digest); err != nil {
		errs = append(errs, fmt.Errorf("digest: %w", err))
	}
	return errors.Join(errs...)
}

func normalizedClosureTaps(closure ClosureResult) []TapID {
	if closure.NormalizationTaps != nil {
		return slices.Clone(closure.NormalizationTaps)
	}
	seen := map[TapID]struct{}{}
	for _, node := range closure.Nodes {
		if !node.ID.IsCore() {
			seen[node.Tap] = struct{}{}
		}
	}
	result := make([]TapID, 0, len(seen))
	for tap := range seen {
		result = append(result, tap)
	}
	slices.Sort(result)
	return result
}

// ValidatePlatformResult validates one platform closure and the exact one-to-one
// selected artifact set.
func ValidatePlatformResult(result PlatformResult) error {
	var errs []error
	if err := result.Platform.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("platform: %w", err))
	}
	if err := ValidateClosureResult(result.Closure); err != nil {
		errs = append(errs, fmt.Errorf("closure: %w", err))
	}
	if result.Artifacts == nil {
		errs = append(errs, errors.New("artifacts must be an array"))
	}
	nodes := make(map[FormulaID]struct{}, len(result.Closure.Nodes))
	for _, node := range result.Closure.Nodes {
		nodes[node.ID] = struct{}{}
	}
	artifacts := make(map[FormulaID]struct{}, len(result.Artifacts))
	for i, artifact := range result.Artifacts {
		if err := ValidateBottleArtifact(artifact); err != nil {
			errs = append(errs, fmt.Errorf("artifacts[%d]: %w", i, err))
		}
		if artifact.Platform != result.Platform {
			errs = append(errs, fmt.Errorf("artifact %q platform %s does not match result %s", artifact.ID, artifact.Platform.key(), result.Platform.key()))
		}
		if _, duplicate := artifacts[artifact.ID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate artifact %q", artifact.ID))
		}
		artifacts[artifact.ID] = struct{}{}
		if _, exists := nodes[artifact.ID]; !exists {
			errs = append(errs, fmt.Errorf("artifact %q has no closure node", artifact.ID))
		}
	}
	if len(result.Artifacts) != len(result.Closure.Nodes) {
		errs = append(errs, fmt.Errorf("artifacts has %d entries for %d closure nodes", len(result.Artifacts), len(result.Closure.Nodes)))
	}
	for _, node := range result.Closure.Nodes {
		if _, exists := artifacts[node.ID]; !exists {
			errs = append(errs, fmt.Errorf("closure node %q has no selected artifact", node.ID))
		}
	}
	return errors.Join(errs...)
}

// ValidateClosureResult validates graph identity, reachability, cycles,
// topological install order, and rack-name uniqueness.
func ValidateClosureResult(closure ClosureResult) error {
	var errs []error
	if closure.Requested == nil {
		errs = append(errs, errors.New("requested must be an array"))
	}
	if closure.Nodes == nil {
		errs = append(errs, errors.New("nodes must be an array"))
	}
	if closure.InstallOrder == nil {
		errs = append(errs, errors.New("install_order must be an array"))
	}
	if len(closure.Nodes) > MaxClosureNodes {
		errs = append(errs, fmt.Errorf("nodes has %d entries, limit is %d", len(closure.Nodes), MaxClosureNodes))
	}
	if len(closure.Requested) == 0 || len(closure.Nodes) == 0 {
		if len(closure.Requested) != 0 || len(closure.Nodes) != 0 || len(closure.InstallOrder) != 0 || len(closure.RequestedMappings) != 0 || len(closure.NormalizationTaps) != 0 {
			errs = append(errs, errors.New("empty requested roots require empty mappings, normalization taps, nodes, and install order"))
		}
		return errors.Join(errs...)
	}
	nodes := make(map[FormulaID]Node, len(closure.Nodes))
	racks := make(map[string]FormulaID, len(closure.Nodes))
	for i, node := range closure.Nodes {
		if err := validateNode(node); err != nil {
			errs = append(errs, fmt.Errorf("nodes[%d]: %w", i, err))
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate closure node %q", node.ID))
		}
		nodes[node.ID] = node
		if prior, duplicate := racks[node.Name]; duplicate && prior != node.ID {
			errs = append(errs, fmt.Errorf("closure nodes %q and %q share rack name %q", prior, node.ID, node.Name))
		}
		racks[node.Name] = node.ID
	}
	seenRoots := make(map[FormulaID]struct{}, len(closure.Requested))
	for i, root := range closure.Requested {
		if err := root.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("requested[%d]: %w", i, err))
		}
		if _, duplicate := seenRoots[root]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate requested root %q", root))
		}
		seenRoots[root] = struct{}{}
		if _, exists := nodes[root]; !exists {
			errs = append(errs, fmt.Errorf("requested root %q has no closure node", root))
		}
	}
	mappings := closure.RequestedMappings
	if mappings == nil {
		mappings = make([]RequestedMapping, len(closure.Requested))
		for i, id := range closure.Requested {
			mappings[i] = RequestedMapping{Requested: id, Resolved: id}
		}
	}
	if len(mappings) != len(closure.Requested) {
		errs = append(errs, errors.New("requested_mappings must have one entry per canonical requested root"))
	}
	seenMappingRequests := make(map[FormulaID]struct{}, len(mappings))
	seenMappingResolved := make(map[FormulaID]struct{}, len(mappings))
	for i, mapping := range mappings {
		if err := mapping.Requested.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("requested_mappings[%d].requested: %w", i, err))
		}
		if err := mapping.Resolved.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("requested_mappings[%d].resolved: %w", i, err))
		}
		if _, duplicate := seenMappingRequests[mapping.Requested]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate requested mapping source %q", mapping.Requested))
		}
		if _, duplicate := seenMappingResolved[mapping.Resolved]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate canonical requested root %q", mapping.Resolved))
		}
		seenMappingRequests[mapping.Requested] = struct{}{}
		seenMappingResolved[mapping.Resolved] = struct{}{}
	}
	for id := range seenRoots {
		if _, ok := seenMappingResolved[id]; !ok {
			errs = append(errs, fmt.Errorf("canonical requested root %q has no requested mapping", id))
		}
	}
	normalizationTaps := normalizedClosureTaps(closure)
	seenNormalizationTaps := make(map[TapID]struct{}, len(normalizationTaps))
	for i, tap := range normalizationTaps {
		if err := tap.Validate(); err != nil || tap.IsCore() {
			errs = append(errs, fmt.Errorf("normalization_taps[%d] is invalid", i))
		}
		if _, duplicate := seenNormalizationTaps[tap]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate normalization tap %q", tap))
		}
		seenNormalizationTaps[tap] = struct{}{}
	}
	for _, mapping := range mappings {
		if !mapping.Requested.IsCore() {
			if _, ok := seenNormalizationTaps[mapping.Requested.Tap()]; !ok {
				errs = append(errs, fmt.Errorf("requested mapping source tap %q is absent from normalization_taps", mapping.Requested.Tap()))
			}
		}
	}
	for _, node := range closure.Nodes {
		if !node.ID.IsCore() {
			if _, ok := seenNormalizationTaps[node.Tap]; !ok {
				errs = append(errs, fmt.Errorf("non-core node %q tap %q is absent from normalization_taps", node.ID, node.Tap))
			}
		}
		for _, dependency := range node.Dependencies {
			if strings.Contains(dependency.Raw, "/") {
				rawID, err := ParseFormulaID(dependency.Raw)
				if err == nil && !rawID.IsCore() {
					if _, ok := seenNormalizationTaps[rawID.Tap()]; !ok {
						errs = append(errs, fmt.Errorf("dependency source tap %q is absent from normalization_taps", rawID.Tap()))
					}
				}
			}
			if _, exists := nodes[dependency.ID]; !exists {
				errs = append(errs, fmt.Errorf("node %q dependency %q is missing", node.ID, dependency.ID))
			}
		}
	}
	position := make(map[FormulaID]int, len(closure.InstallOrder))
	if len(closure.InstallOrder) != len(nodes) {
		errs = append(errs, fmt.Errorf("install_order has %d entries for %d nodes", len(closure.InstallOrder), len(nodes)))
	}
	for i, id := range closure.InstallOrder {
		if err := id.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("install_order[%d]: %w", i, err))
		}
		if _, exists := nodes[id]; !exists {
			errs = append(errs, fmt.Errorf("install_order references unknown node %q", id))
		}
		if _, duplicate := position[id]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate install_order entry %q", id))
		}
		position[id] = i
	}
	for _, node := range closure.Nodes {
		nodePosition, present := position[node.ID]
		if !present {
			errs = append(errs, fmt.Errorf("install_order omits node %q", node.ID))
			continue
		}
		for _, dependency := range node.Dependencies {
			dependencyPosition, present := position[dependency.ID]
			if !present || dependencyPosition >= nodePosition {
				errs = append(errs, fmt.Errorf("install_order places %q before dependency %q", node.ID, dependency.ID))
			}
		}
	}

	state := make(map[FormulaID]uint8, len(nodes))
	reachable := make(map[FormulaID]struct{}, len(nodes))
	var visit func(FormulaID)
	visit = func(id FormulaID) {
		switch state[id] {
		case 1:
			errs = append(errs, fmt.Errorf("dependency cycle reaches %q", id))
			return
		case 2:
			reachable[id] = struct{}{}
			return
		}
		state[id] = 1
		reachable[id] = struct{}{}
		node, exists := nodes[id]
		if exists {
			for _, dependency := range node.Dependencies {
				visit(dependency.ID)
			}
		}
		state[id] = 2
	}
	for _, root := range closure.Requested {
		visit(root)
	}
	for _, node := range closure.Nodes {
		if _, exists := reachable[node.ID]; !exists {
			errs = append(errs, fmt.Errorf("closure node %q is unreachable from requested roots", node.ID))
		}
	}
	return errors.Join(errs...)
}

func validateNode(node Node) error {
	var errs []error
	if err := node.ID.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("id: %w", err))
	}
	if err := node.Tap.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("tap: %w", err))
	}
	if node.Tap != node.ID.Tap() {
		errs = append(errs, fmt.Errorf("tap %q does not match Formula ID %q", node.Tap, node.ID))
	}
	if err := validateFormulaName(node.Name); err != nil {
		errs = append(errs, fmt.Errorf("name: %w", err))
	}
	if node.Name != node.ID.Name() {
		errs = append(errs, fmt.Errorf("name %q does not match Formula ID %q", node.Name, node.ID))
	}
	if node.HomebrewFullName != string(node.ID) {
		errs = append(errs, fmt.Errorf("homebrew_full_name %q does not match Formula ID %q", node.HomebrewFullName, node.ID))
	}
	if strings.TrimSpace(node.FormulaVersion) == "" || strings.TrimSpace(node.PkgVersion) == "" || hasControl(node.FormulaVersion) || hasControl(node.PkgVersion) {
		errs = append(errs, errors.New("formula_version and pkg_version are required and cannot contain control characters"))
	}
	if node.FormulaRevision < 0 || node.VersionScheme < 0 || node.BottleRebuild < 0 {
		errs = append(errs, errors.New("formula_revision, version_scheme, and bottle_rebuild must be non-negative"))
	}
	if hasControl(node.License) {
		errs = append(errs, errors.New("license cannot contain control characters"))
	}
	seen := make(map[FormulaID]struct{}, len(node.Dependencies))
	for i, dependency := range node.Dependencies {
		if err := validateRequirement(dependency, node.Tap); err != nil {
			errs = append(errs, fmt.Errorf("dependencies[%d]: %w", i, err))
		}
		if dependency.ID == node.ID {
			errs = append(errs, fmt.Errorf("node %q depends on itself", node.ID))
		}
		if _, duplicate := seen[dependency.ID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate dependency %q", dependency.ID))
		}
		seen[dependency.ID] = struct{}{}
	}
	return errors.Join(errs...)
}

func validateRequirement(requirement Requirement, owner TapID) error {
	var errs []error
	if err := validateDependencies([]Dependency{{Raw: requirement.Raw, ID: requirement.ID}}, owner); err != nil {
		errs = append(errs, err)
	}
	if requirement.MinimumRevision < 0 || requirement.MinimumBottleRebuild < 0 {
		errs = append(errs, errors.New("minimum_revision and minimum_bottle_rebuild must be non-negative"))
	}
	if hasControl(requirement.MinimumPkgVersion) {
		errs = append(errs, errors.New("minimum_pkg_version cannot contain control characters"))
	}
	return errors.Join(errs...)
}

// ValidateBottleArtifact validates the strict transport/provenance unions and
// their binding to the exact selected bottle bytes.
func ValidateBottleArtifact(artifact BottleArtifact) error {
	var errs []error
	if err := artifact.ID.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("id: %w", err))
	}
	if err := artifact.Platform.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("platform: %w", err))
	}
	if artifact.Tag != "all" && artifact.Tag != artifact.Platform.bottleTag() {
		errs = append(errs, fmt.Errorf("tag %q does not match platform %s", artifact.Tag, artifact.Platform.key()))
	}
	if err := validateBottleTab(artifact.Tab, artifact.Platform, artifact.Tag); err != nil {
		errs = append(errs, fmt.Errorf("tab: %w", err))
	}
	if err := validateSafeFilename(artifact.Filename); err != nil {
		errs = append(errs, fmt.Errorf("filename: %w", err))
	}
	if err := validateSHA256Digest(artifact.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("sha256: %w", err))
	}
	if artifact.Size <= 0 || artifact.Size > MaxBottleBytes {
		errs = append(errs, fmt.Errorf("size %d is outside 1..%d", artifact.Size, MaxBottleBytes))
	}
	if err := validateCellarPolicy(artifact.Cellar); err != nil {
		errs = append(errs, fmt.Errorf("cellar: %w", err))
	}
	if err := validateSHA256Digest(artifact.CurrentFormulaSourceDigest); err != nil {
		errs = append(errs, fmt.Errorf("current_formula_source_digest: %w", err))
	}
	if err := validateSHA256Digest(artifact.BottleFormulaSourceDigest); err != nil {
		errs = append(errs, fmt.Errorf("bottle_formula_source_digest: %w", err))
	}
	switch {
	case artifact.PrebuiltDerivation != nil:
		if artifact.Transport.HTTPS == nil && artifact.Transport.Local == nil {
			errs = append(errs, errors.New("prebuilt-derived bottle requires HTTPS or build-local transport"))
		}
		if artifact.Transport.OCI != nil {
			errs = append(errs, errors.New("prebuilt-derived bottle cannot use OCI transport"))
		}
		if artifact.BottleSourceWaiver != "" {
			errs = append(errs, errors.New("prebuilt-derived bottle cannot use the native HTTPS bottle source waiver"))
		}
		if artifact.BottleSourceRepository != "" || artifact.BottleSourceCommit != "" || artifact.BottleFormulaPath != "" {
			errs = append(errs, errors.New("prebuilt-derived bottle cannot combine derivation evidence with native bottle source fields"))
		}
	case artifact.Transport.HTTPS != nil:
		if artifact.BottleSourceWaiver != HTTPSBottleSourceWaiver {
			errs = append(errs, fmt.Errorf("HTTPS bottle requires source waiver %q", HTTPSBottleSourceWaiver))
		}
		if artifact.BottleSourceRepository != "" || artifact.BottleSourceCommit != "" || artifact.BottleFormulaPath != "" {
			errs = append(errs, errors.New("HTTPS bottle source waiver cannot be combined with an asserted historical source"))
		}
	default:
		if artifact.BottleSourceWaiver != "" {
			errs = append(errs, errors.New("OCI bottle cannot use the HTTPS source waiver"))
		}
		expectedRepository := artifact.ID.Tap().DefaultGitHubRepository()
		if artifact.BottleSourceRepository != expectedRepository {
			errs = append(errs, fmt.Errorf("bottle_source_repository %q does not match %q", artifact.BottleSourceRepository, expectedRepository))
		}
		if err := validateCommit(artifact.BottleSourceCommit); err != nil {
			errs = append(errs, fmt.Errorf("bottle_source_commit: %w", err))
		}
		if err := validateFormulaSourcePath(artifact.BottleFormulaPath, artifact.ID.Name()); err != nil {
			errs = append(errs, fmt.Errorf("bottle_formula_path: %w", err))
		}
	}
	seenExecutablePaths := map[string]struct{}{}
	for i, executable := range artifact.ExecutablePaths {
		if executable == "" || executable == "." || executable == ".." || hasControl(executable) || path.IsAbs(executable) || path.Clean(executable) != executable || strings.HasPrefix(executable, "../") || strings.Contains(executable, "\\") {
			errs = append(errs, fmt.Errorf("executable_paths[%d] is unsafe", i))
		}
		if _, duplicate := seenExecutablePaths[executable]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate executable path %q", executable))
		}
		seenExecutablePaths[executable] = struct{}{}
	}
	if err := validateTransport(artifact.Transport, artifact); err != nil {
		errs = append(errs, fmt.Errorf("transport: %w", err))
	}
	if artifact.Tab.Receiptless && artifact.Transport.HTTPS == nil && artifact.Transport.Local == nil {
		errs = append(errs, errors.New("receiptless tab marker is supported only for HTTPS or build-local bottles"))
	}
	if err := validateBottleVerification(artifact.Verification); err != nil {
		errs = append(errs, fmt.Errorf("verification: %w", err))
	}
	if err := validateProvenance(artifact.Provenance, artifact.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("provenance: %w", err))
	}
	if artifact.PrebuiltDerivation != nil {
		if artifact.Provenance.Waiver == nil || artifact.Provenance.Waiver.Policy != PrebuiltProvenanceWaiver {
			errs = append(errs, errors.New("prebuilt derivation requires the dedicated provenance waiver"))
		}
		if err := ValidatePrebuiltDerivation(*artifact.PrebuiltDerivation); err != nil {
			errs = append(errs, fmt.Errorf("prebuilt_derivation: %w", err))
		}
		if err := validatePrebuiltDerivationBinding(*artifact.PrebuiltDerivation, artifact); err != nil {
			errs = append(errs, fmt.Errorf("prebuilt_derivation binding: %w", err))
		}
	} else if artifact.Provenance.Waiver != nil && artifact.Provenance.Waiver.Policy == PrebuiltProvenanceWaiver {
		errs = append(errs, errors.New("prebuilt provenance waiver requires derivation evidence"))
	}
	return errors.Join(errs...)
}

// ValidatePrebuiltDerivation validates the self-contained upstream-source and
// deterministic derived-bottle evidence. Use ValidateBottleArtifact to also
// bind it to a selected artifact and target platform.
func ValidatePrebuiltDerivation(derivation PrebuiltDerivation) error {
	var errs []error
	if err := validatePolicyVersion("policy_version", derivation.PolicyVersion); err != nil {
		errs = append(errs, err)
	}
	if err := validateSHA256Digest(derivation.PolicyDigest); err != nil {
		errs = append(errs, fmt.Errorf("policy_digest: %w", err))
	}
	if err := validatePrebuiltSourceArtifact(derivation.Source); err != nil {
		errs = append(errs, fmt.Errorf("source: %w", err))
	}
	if err := validatePrebuiltSourceInventory(derivation.SourceInventory); err != nil {
		errs = append(errs, fmt.Errorf("source_inventory: %w", err))
	}
	if err := validatePrebuiltPayloadEvidence(derivation.Payload); err != nil {
		errs = append(errs, fmt.Errorf("payload: %w", err))
	}
	if derivation.Payload.Size > derivation.SourceInventory.ExpandedSize && derivation.SourceInventory.ExpandedSize > 0 {
		errs = append(errs, errors.New("payload size exceeds verified source expanded size"))
	}
	if err := validatePrebuiltELFEvidence(derivation.ELF); err != nil {
		errs = append(errs, fmt.Errorf("elf: %w", err))
	}
	if err := validatePrebuiltFormulaSourceEvidence(derivation.FormulaSource); err != nil {
		errs = append(errs, fmt.Errorf("formula_source: %w", err))
	}
	if err := validateSHA256Digest(derivation.RecipeDigest); err != nil {
		errs = append(errs, fmt.Errorf("recipe_digest: %w", err))
	}
	if err := validatePrebuiltDerivedBottleRelation(derivation.DerivedBottle); err != nil {
		errs = append(errs, fmt.Errorf("derived_bottle: %w", err))
	}
	if derivation.DerivedBottle.FormulaSourceDigest != derivation.FormulaSource.SHA256 {
		errs = append(errs, errors.New("derived bottle Formula source digest does not match authenticated Formula source"))
	}
	if derivation.Source.SHA256 != "" && derivation.Source.SHA256 == derivation.DerivedBottle.SHA256 {
		errs = append(errs, errors.New("upstream source archive and derived bottle must have distinct digests"))
	}
	return errors.Join(errs...)
}

// ValidatePrebuiltDerivationSource binds a derivation to one platform file in
// an authenticated prebuilt archive declaration.
func ValidatePrebuiltDerivationSource(declaration PrebuiltArchiveDeclaration, tag string, derivation PrebuiltDerivation) error {
	var errs []error
	if err := ValidatePrebuiltArchiveDeclaration(declaration); err != nil {
		errs = append(errs, fmt.Errorf("declaration: %w", err))
	}
	if err := ValidatePrebuiltDerivation(derivation); err != nil {
		errs = append(errs, fmt.Errorf("derivation: %w", err))
	}
	var selected *PrebuiltArchiveFile
	for i := range declaration.Files {
		if declaration.Files[i].Tag == tag {
			selected = &declaration.Files[i]
			break
		}
	}
	if selected == nil {
		errs = append(errs, fmt.Errorf("declaration has no prebuilt archive for tag %q", tag))
		return errors.Join(errs...)
	}
	if derivation.Source.Transport.HTTPS == nil {
		errs = append(errs, errors.New("derivation source has no HTTPS transport"))
		return errors.Join(errs...)
	}
	if derivation.Source.Transport.HTTPS.URL != selected.URL {
		errs = append(errs, errors.New("derivation source URL does not match prebuilt declaration"))
	}
	if derivation.Source.SHA256 != selected.SHA256 {
		errs = append(errs, errors.New("derivation source digest does not match prebuilt declaration"))
	}
	if derivation.Source.Format != selected.Format {
		errs = append(errs, errors.New("derivation source format does not match prebuilt declaration"))
	}
	return errors.Join(errs...)
}

func validatePrebuiltSourceArtifact(source PrebuiltSourceArtifact) error {
	var errs []error
	if err := validateSafeFilename(source.Filename); err != nil {
		errs = append(errs, fmt.Errorf("filename: %w", err))
	}
	if source.Size <= 0 || source.Size > MaxBottleBytes {
		errs = append(errs, fmt.Errorf("size %d is outside 1..%d", source.Size, MaxBottleBytes))
	}
	if err := validateSHA256Digest(source.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("sha256: %w", err))
	}
	if (source.Transport.OCI == nil) == (source.Transport.HTTPS == nil) {
		errs = append(errs, errors.New("transport must set exactly one of oci or https"))
	}
	if source.Transport.OCI != nil {
		errs = append(errs, errors.New("transport must use HTTPS, not OCI"))
	}
	if source.Transport.HTTPS != nil {
		binding := BottleArtifact{Filename: source.Filename, Size: source.Size, SHA256: source.SHA256}
		if err := validateHTTPSTransport(*source.Transport.HTTPS, binding); err != nil {
			errs = append(errs, fmt.Errorf("transport.https: %w", err))
		}
		if parsed, err := url.Parse(source.Transport.HTTPS.URL); err == nil {
			name, unescapeErr := url.PathUnescape(path.Base(parsed.EscapedPath()))
			if unescapeErr != nil || name != source.Filename {
				errs = append(errs, fmt.Errorf("filename %q does not match source URL basename %q", source.Filename, name))
			}
		}
		if err := validatePrebuiltArchiveFormat(source.Format, source.Transport.HTTPS.URL); err != nil {
			errs = append(errs, fmt.Errorf("format: %w", err))
		}
	} else if source.Format != PrebuiltArchiveFormatTarGzip {
		errs = append(errs, fmt.Errorf("unsupported format %q", source.Format))
	}
	return errors.Join(errs...)
}

func validatePrebuiltSourceInventory(inventory PrebuiltSourceInventory) error {
	var errs []error
	if err := validateSHA256Digest(inventory.InventoryDigest); err != nil {
		errs = append(errs, fmt.Errorf("inventory_digest: %w", err))
	}
	if inventory.EntryCount <= 0 || inventory.EntryCount > MaxPrebuiltArchiveEntries {
		errs = append(errs, fmt.Errorf("entry_count %d is outside 1..%d", inventory.EntryCount, MaxPrebuiltArchiveEntries))
	}
	if inventory.ExpandedSize <= 0 || inventory.ExpandedSize > MaxPrebuiltArchiveExpandedBytes {
		errs = append(errs, fmt.Errorf("expanded_size %d is outside 1..%d", inventory.ExpandedSize, MaxPrebuiltArchiveExpandedBytes))
	}
	return errors.Join(errs...)
}

func validatePrebuiltPayloadEvidence(payload PrebuiltPayloadEvidence) error {
	var errs []error
	if err := validateContainedSlashPath(payload.SourcePath); err != nil {
		errs = append(errs, fmt.Errorf("source_path: %w", err))
	}
	if err := validateContainedSlashPath(payload.DestinationPath); err != nil {
		errs = append(errs, fmt.Errorf("destination_path: %w", err))
	}
	if err := validateSHA256Digest(payload.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("sha256: %w", err))
	}
	if payload.Size <= 0 || payload.Size > MaxPrebuiltArchiveExpandedBytes {
		errs = append(errs, fmt.Errorf("size %d is outside 1..%d", payload.Size, MaxPrebuiltArchiveExpandedBytes))
	}
	if payload.ArchiveMode == 0 || payload.ArchiveMode&^0o777 != 0 || payload.ArchiveMode&0o111 == 0 || payload.ArchiveMode&0o022 != 0 {
		errs = append(errs, fmt.Errorf("archive_mode %#o is not a non-group-writable executable mode", payload.ArchiveMode))
	}
	if payload.DerivedMode != 0o555 {
		errs = append(errs, fmt.Errorf("derived_mode %#o must be 0555", payload.DerivedMode))
	}
	return errors.Join(errs...)
}

func validatePrebuiltELFEvidence(evidence PrebuiltELFEvidence) error {
	var errs []error
	if evidence.Format != PrebuiltELFFormatELF64 {
		errs = append(errs, fmt.Errorf("unsupported format %q", evidence.Format))
	}
	if evidence.Machine != PrebuiltELFMachineX8664 && evidence.Machine != PrebuiltELFMachineAArch64 {
		errs = append(errs, fmt.Errorf("unsupported machine %q", evidence.Machine))
	}
	if !evidence.StaticallyLinked {
		errs = append(errs, errors.New("statically_linked must be true"))
	}
	if evidence.Interpreter != "" {
		errs = append(errs, errors.New("interpreter must be empty for a static executable"))
	}
	if evidence.NeededLibraries == nil || len(evidence.NeededLibraries) != 0 {
		errs = append(errs, errors.New("needed_libraries must be an explicit empty array"))
	}
	if evidence.RPaths == nil || len(evidence.RPaths) != 0 {
		errs = append(errs, errors.New("rpaths must be an explicit empty array"))
	}
	if evidence.WritableExecutableSegments {
		errs = append(errs, errors.New("writable_executable_segments must be false"))
	}
	return errors.Join(errs...)
}

func validatePrebuiltFormulaSourceEvidence(evidence PrebuiltFormulaSourceEvidence) error {
	var errs []error
	if err := validateTapSource(evidence.Transport.Tap); err != nil {
		errs = append(errs, fmt.Errorf("transport.tap: %w", err))
	}
	if err := validateFormulaSourceFilePath(evidence.Transport.Path); err != nil {
		errs = append(errs, fmt.Errorf("transport.path: %w", err))
	}
	if err := validateSHA256Digest(evidence.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("sha256: %w", err))
	}
	if evidence.Size <= 0 || evidence.Size > MaxPrebuiltFormulaSourceBytes {
		errs = append(errs, fmt.Errorf("size %d is outside 1..%d", evidence.Size, MaxPrebuiltFormulaSourceBytes))
	}
	return errors.Join(errs...)
}

func validatePrebuiltDerivedBottleRelation(relation PrebuiltDerivedBottleRelation) error {
	var errs []error
	if relation.Tag != "x86_64_linux" && relation.Tag != "arm64_linux" {
		errs = append(errs, fmt.Errorf("unsupported tag %q", relation.Tag))
	}
	if err := validateSafeFilename(relation.Filename); err != nil {
		errs = append(errs, fmt.Errorf("filename: %w", err))
	}
	if err := validateSHA256Digest(relation.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("sha256: %w", err))
	}
	if relation.Size <= 0 || relation.Size > MaxBottleBytes {
		errs = append(errs, fmt.Errorf("size %d is outside 1..%d", relation.Size, MaxBottleBytes))
	}
	if err := validateBottleVerification(relation.Verification); err != nil {
		errs = append(errs, fmt.Errorf("verification: %w", err))
	}
	if err := validateSHA256Digest(relation.FormulaSourceDigest); err != nil {
		errs = append(errs, fmt.Errorf("formula_source_digest: %w", err))
	}
	return errors.Join(errs...)
}

func validatePrebuiltDerivationBinding(derivation PrebuiltDerivation, artifact BottleArtifact) error {
	var errs []error
	if !artifact.Tab.Receiptless {
		errs = append(errs, errors.New("derived bottle must be marked receiptless"))
	}
	if derivation.DerivedBottle.Tag != artifact.Tag {
		errs = append(errs, errors.New("derived bottle tag does not match selected artifact"))
	}
	if derivation.DerivedBottle.Filename != artifact.Filename {
		errs = append(errs, errors.New("derived bottle filename does not match selected artifact"))
	}
	if derivation.DerivedBottle.SHA256 != artifact.SHA256 {
		errs = append(errs, errors.New("derived bottle digest does not match selected artifact"))
	}
	if derivation.DerivedBottle.Size != artifact.Size {
		errs = append(errs, errors.New("derived bottle size does not match selected artifact"))
	}
	if derivation.DerivedBottle.Verification != artifact.Verification {
		errs = append(errs, errors.New("derived bottle verification does not match selected artifact"))
	}
	if derivation.FormulaSource.Transport.Tap.ID != artifact.ID.Tap() {
		errs = append(errs, errors.New("Formula source tap does not match selected Formula ID"))
	}
	if err := validateFormulaSourcePath(derivation.FormulaSource.Transport.Path, artifact.ID.Name()); err != nil {
		errs = append(errs, fmt.Errorf("Formula source path: %w", err))
	}
	if derivation.FormulaSource.SHA256 != artifact.CurrentFormulaSourceDigest || derivation.FormulaSource.SHA256 != artifact.BottleFormulaSourceDigest || derivation.DerivedBottle.FormulaSourceDigest != artifact.BottleFormulaSourceDigest {
		errs = append(errs, errors.New("Formula source digest does not bind current, embedded, and derived-bottle evidence"))
	}
	expectedMachine := PrebuiltELFMachineX8664
	if artifact.Platform.Architecture == "arm64" {
		expectedMachine = PrebuiltELFMachineAArch64
	}
	if derivation.ELF.Machine != expectedMachine {
		errs = append(errs, fmt.Errorf("ELF machine %q does not match platform %s", derivation.ELF.Machine, artifact.Platform.key()))
	}
	if !slices.Contains(artifact.ExecutablePaths, derivation.Payload.DestinationPath) {
		errs = append(errs, errors.New("payload destination is absent from selected executable paths"))
	}
	return errors.Join(errs...)
}

func validateBottleTab(tab BottleTab, platform Platform, tag string) error {
	var errs []error
	expectedArch := "x86_64"
	if platform.Architecture == "arm64" {
		expectedArch = "arm64"
	}
	if tag == "all" {
		if tab.Arch != "" {
			errs = append(errs, fmt.Errorf("all bottle has architecture %q", tab.Arch))
		}
	} else if tab.Arch != expectedArch {
		errs = append(errs, fmt.Errorf("architecture %q does not match %q", tab.Arch, expectedArch))
	}
	if tab.Receiptless && (tab.HomebrewVersion != "" || tab.Compiler != "" || len(tab.ChangedFiles) != 0 || tab.BuiltOn != (BottleBuiltOn{}) || len(tab.Dependencies) != 0) {
		errs = append(errs, errors.New("receiptless tab cannot claim receipt-only metadata"))
	}
	seen := map[FormulaID]struct{}{}
	for i, dependency := range tab.Dependencies {
		if err := dependency.ID.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("runtime_dependencies[%d].id: %w", i, err))
		}
		if err := dependency.HomebrewFullName.Validate(); err != nil || dependency.HomebrewFullName != dependency.ID {
			errs = append(errs, fmt.Errorf("runtime_dependencies[%d] has mismatched full identity", i))
		}
		if dependency.Version == "" || dependency.PkgVersion == "" || dependency.Revision < 0 || dependency.BottleRebuild < 0 {
			errs = append(errs, fmt.Errorf("runtime_dependencies[%d] has invalid version metadata", i))
		}
		if _, duplicate := seen[dependency.ID]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate runtime dependency %q", dependency.ID))
		}
		seen[dependency.ID] = struct{}{}
	}
	for i, changed := range tab.ChangedFiles {
		if changed == "" || changed == "." || changed == ".." || hasControl(changed) || path.IsAbs(changed) || path.Clean(changed) != changed || strings.HasPrefix(changed, "../") || strings.Contains(changed, "\\") {
			errs = append(errs, fmt.Errorf("changed_files[%d] is unsafe", i))
		}
	}
	return errors.Join(errs...)
}

func validateTransport(transport Transport, artifact BottleArtifact) error {
	members := 0
	if transport.OCI != nil {
		members++
	}
	if transport.HTTPS != nil {
		members++
	}
	if transport.Local != nil {
		members++
	}
	if members != 1 {
		return errors.New("must set exactly one of oci, https, or local")
	}
	if transport.OCI != nil {
		return validateOCITransport(*transport.OCI, artifact)
	}
	if transport.HTTPS != nil {
		return validateHTTPSTransport(*transport.HTTPS, artifact)
	}
	return validateLocalTransport(*transport.Local, artifact)
}

func validateLocalTransport(transport LocalTransport, artifact BottleArtifact) error {
	var errs []error
	if artifact.PrebuiltDerivation == nil {
		errs = append(errs, errors.New("build-local transport is limited to prebuilt-derived bottles"))
	}
	if transport.PolicyVersion != BuildLocalArtifactPolicyVersion {
		errs = append(errs, fmt.Errorf("unsupported policy_version %q", transport.PolicyVersion))
	}
	if transport.SHA256 != artifact.SHA256 {
		errs = append(errs, errors.New("sha256 does not match artifact digest"))
	}
	if transport.Size != artifact.Size {
		errs = append(errs, errors.New("size does not match artifact size"))
	}
	if transport.Filename != artifact.Filename {
		errs = append(errs, errors.New("filename does not match artifact filename"))
	}
	if err := validateSHA256Digest(transport.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("sha256: %w", err))
	}
	if transport.Size <= 0 || transport.Size > MaxBottleBytes {
		errs = append(errs, fmt.Errorf("size %d is outside 1..%d", transport.Size, MaxBottleBytes))
	}
	if err := validateSafeFilename(transport.Filename); err != nil {
		errs = append(errs, fmt.Errorf("filename: %w", err))
	}
	return errors.Join(errs...)
}

func validateOCITransport(transport OCITransport, artifact BottleArtifact) error {
	var errs []error
	if err := validatePublicHostname(transport.Registry); err != nil {
		errs = append(errs, fmt.Errorf("registry: %w", err))
	}
	if err := validateOCIRepository(transport.Repository); err != nil {
		errs = append(errs, fmt.Errorf("repository: %w", err))
	}
	for _, item := range []struct {
		name       string
		descriptor Descriptor
	}{{"index", transport.Index}, {"manifest", transport.Manifest}, {"config", transport.Config}, {"layer", transport.Layer}} {
		if err := validateDescriptor(item.descriptor); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", item.name, err))
		}
	}
	if transport.Layer.Digest != artifact.SHA256 {
		errs = append(errs, fmt.Errorf("layer digest %q does not match artifact digest %q", transport.Layer.Digest, artifact.SHA256))
	}
	if transport.Layer.Size != artifact.Size {
		errs = append(errs, fmt.Errorf("layer size %d does not match artifact size %d", transport.Layer.Size, artifact.Size))
	}
	if artifact.Tag == "all" {
		if transport.Manifest.Platform != nil {
			errs = append(errs, errors.New("all bottle manifest must not carry a platform"))
		}
	} else if transport.Manifest.Platform == nil || *transport.Manifest.Platform != artifact.Platform {
		errs = append(errs, errors.New("manifest platform does not match artifact platform"))
	}
	return errors.Join(errs...)
}

func validateHTTPSTransport(transport HTTPSTransport, artifact BottleArtifact) error {
	var errs []error
	if err := validateHTTPSURL(transport.URL, true); err != nil {
		errs = append(errs, fmt.Errorf("url: %w", err))
	}
	if transport.ExpectedSize != artifact.Size {
		errs = append(errs, fmt.Errorf("expected_size %d does not match artifact size %d", transport.ExpectedSize, artifact.Size))
	}
	if transport.ExpectedSize <= 0 || transport.ExpectedSize > MaxBottleBytes {
		errs = append(errs, fmt.Errorf("expected_size %d is outside 1..%d", transport.ExpectedSize, MaxBottleBytes))
	}
	if transport.SHA256 != artifact.SHA256 {
		errs = append(errs, fmt.Errorf("sha256 %q does not match artifact digest %q", transport.SHA256, artifact.SHA256))
	}
	if err := validateSHA256Digest(transport.SHA256); err != nil {
		errs = append(errs, fmt.Errorf("sha256: %w", err))
	}
	if transport.Filename != artifact.Filename {
		errs = append(errs, fmt.Errorf("filename %q does not match artifact filename %q", transport.Filename, artifact.Filename))
	}
	if err := validateSafeFilename(transport.Filename); err != nil {
		errs = append(errs, fmt.Errorf("filename: %w", err))
	}
	if transport.FetchPolicyVersion != HTTPSFetchPolicyVersion {
		errs = append(errs, fmt.Errorf("unsupported fetch_policy_version %q", transport.FetchPolicyVersion))
	}
	if transport.AllowedRedirectHosts == nil || len(transport.AllowedRedirectHosts) == 0 {
		errs = append(errs, errors.New("allowed_redirect_hosts must be a non-empty array"))
	}
	if len(transport.AllowedRedirectHosts) > MaxRedirects+1 {
		errs = append(errs, fmt.Errorf("allowed_redirect_hosts has %d entries, limit is %d", len(transport.AllowedRedirectHosts), MaxRedirects+1))
	}
	seen := make(map[string]struct{}, len(transport.AllowedRedirectHosts))
	for i, host := range transport.AllowedRedirectHosts {
		if err := validatePublicHostname(host); err != nil {
			errs = append(errs, fmt.Errorf("allowed_redirect_hosts[%d]: %w", i, err))
		}
		if _, duplicate := seen[host]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate allowed redirect host %q", host))
		}
		seen[host] = struct{}{}
	}
	if parsed, err := url.Parse(transport.URL); err == nil {
		if _, present := seen[parsed.Hostname()]; !present {
			errs = append(errs, fmt.Errorf("initial URL host %q is absent from allowed_redirect_hosts", parsed.Hostname()))
		}
	}
	return errors.Join(errs...)
}

func validateDescriptor(descriptor Descriptor) error {
	var errs []error
	if err := validateSHA256Digest(descriptor.Digest); err != nil {
		errs = append(errs, fmt.Errorf("digest: %w", err))
	}
	if descriptor.Size <= 0 {
		errs = append(errs, fmt.Errorf("size must be positive, got %d", descriptor.Size))
	}
	if strings.TrimSpace(descriptor.MediaType) == "" || hasControl(descriptor.MediaType) {
		errs = append(errs, errors.New("media_type is required and cannot contain control characters"))
	}
	if descriptor.Platform != nil {
		if err := descriptor.Platform.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("platform: %w", err))
		}
	}
	seen := make(map[string]struct{}, len(descriptor.Annotations))
	for i, annotation := range descriptor.Annotations {
		if err := validateBoundedText("key", annotation.Key, 1024, false); err != nil {
			errs = append(errs, fmt.Errorf("annotations[%d]: %w", i, err))
		}
		if len(annotation.Value) > 16<<10 || hasControlExceptWhitespace(annotation.Value) {
			errs = append(errs, fmt.Errorf("annotations[%d].value is overlong or contains unsupported control characters", i))
		}
		if _, duplicate := seen[annotation.Key]; duplicate {
			errs = append(errs, fmt.Errorf("duplicate annotation key %q", annotation.Key))
		}
		seen[annotation.Key] = struct{}{}
	}
	return errors.Join(errs...)
}

func validateBottleVerification(verification BottleVerification) error {
	var errs []error
	if verification.PolicyVersion != BottleVerificationPolicy {
		errs = append(errs, fmt.Errorf("unsupported policy_version %q", verification.PolicyVersion))
	}
	if err := validateSHA256Digest(verification.InventoryDigest); err != nil {
		errs = append(errs, fmt.Errorf("inventory_digest: %w", err))
	}
	if verification.EntryCount <= 0 {
		errs = append(errs, errors.New("entry_count must be positive"))
	}
	if verification.ExpandedSize <= 0 {
		errs = append(errs, errors.New("expanded_size must be positive"))
	}
	return errors.Join(errs...)
}

func validateProvenance(provenance Provenance, subjectDigest string) error {
	if (provenance.Verified == nil) == (provenance.Waiver == nil) {
		return errors.New("must set exactly one of verified or waiver")
	}
	if provenance.Waiver != nil {
		if provenance.Waiver.Policy != ChecksumProvenanceWaiver && provenance.Waiver.Policy != PrebuiltProvenanceWaiver {
			return fmt.Errorf("unsupported waiver policy %q", provenance.Waiver.Policy)
		}
		return nil
	}
	verified := provenance.Verified
	var errs []error
	if verified.PolicyVersion != VerifiedProvenancePolicy {
		errs = append(errs, fmt.Errorf("unsupported verified policy_version %q", verified.PolicyVersion))
	}
	if err := validateSHA256Digest(verified.SubjectDigest); err != nil {
		errs = append(errs, fmt.Errorf("subject_digest: %w", err))
	}
	if verified.SubjectDigest != subjectDigest {
		errs = append(errs, fmt.Errorf("subject_digest %q does not match bottle digest %q", verified.SubjectDigest, subjectDigest))
	}
	if err := validateSHA256Digest(verified.StatementDigest); err != nil {
		errs = append(errs, fmt.Errorf("statement_digest: %w", err))
	}
	if err := validateSHA256Digest(verified.BundleDigest); err != nil {
		errs = append(errs, fmt.Errorf("bundle_digest: %w", err))
	}
	if err := validateBoundedText("signer_identity", verified.SignerIdentity, 2048, false); err != nil {
		errs = append(errs, err)
	}
	if err := validateBoundedText("issuer", verified.Issuer, 2048, false); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// ValidateCatalogSetResult validates the operation's opaque JWS carrier. It
// checks only serialization shape and digest bindings; cryptographic
// verification is intentionally outside this package.
func ValidateCatalogSetResult(result *CatalogSetResult) error {
	if result == nil {
		return errors.New("nil catalog-set result")
	}
	var errs []error
	if result.SchemaVersion != ResultSchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q", result.SchemaVersion))
	}
	if err := validateSHA256Digest(result.RequestDigest); err != nil {
		errs = append(errs, fmt.Errorf("request_digest: %w", err))
	}
	if err := validateSHA256Digest(result.PayloadDigest); err != nil {
		errs = append(errs, fmt.Errorf("payload_digest: %w", err))
	}
	if len(result.JWS) == 0 || int64(len(result.JWS)) > MaxJWSBytes {
		errs = append(errs, fmt.Errorf("jws size %d is outside 1..%d", len(result.JWS), MaxJWSBytes))
	} else if err := validateJWSShape(result.JWS); err != nil {
		errs = append(errs, fmt.Errorf("jws: %w", err))
	}
	return errors.Join(errs...)
}

// ValidateOperation validates the strict pending/completed/failed union.
func ValidateOperation(operation *Operation) error {
	if operation == nil {
		return errors.New("nil catalog operation")
	}
	var errs []error
	if operation.SchemaVersion != OperationSchemaVersion {
		errs = append(errs, fmt.Errorf("unsupported schema_version %q", operation.SchemaVersion))
	}
	if err := validateOperationID(operation.ID); err != nil {
		errs = append(errs, fmt.Errorf("id: %w", err))
	}
	switch operation.Status {
	case OperationPending:
		if operation.RetryAfterSeconds < 0 || operation.RetryAfterSeconds > 3600 {
			errs = append(errs, errors.New("pending operation retry_after_seconds must be omitted or in 1..3600"))
		}
		if operation.Result != nil || operation.Failure != nil {
			errs = append(errs, errors.New("pending operation cannot contain result or failure"))
		}
	case OperationCompleted:
		if operation.RetryAfterSeconds != 0 || operation.Result == nil || operation.Failure != nil {
			errs = append(errs, errors.New("completed operation must contain only result"))
		}
		if operation.Result != nil {
			if err := ValidateCatalogSetResult(operation.Result); err != nil {
				errs = append(errs, fmt.Errorf("result: %w", err))
			}
		}
	case OperationFailed:
		if operation.RetryAfterSeconds != 0 || operation.Result != nil || operation.Failure == nil {
			errs = append(errs, errors.New("failed operation must contain only failure"))
		}
		if operation.Failure != nil {
			if err := ValidateFailure(*operation.Failure); err != nil {
				errs = append(errs, fmt.Errorf("failure: %w", err))
			}
		}
	default:
		errs = append(errs, fmt.Errorf("unsupported operation status %q", operation.Status))
	}
	return errors.Join(errs...)
}

// Valid reports whether code is one of the stable protocol failure codes.
func (code FailureCode) Valid() bool {
	switch code {
	case FailureTimeout, FailureUnavailable, FailureInvalidTap, FailureMissingBottle, FailurePolicy, FailureSignature:
		return true
	default:
		return false
	}
}

// ValidateFailure validates one stable machine-readable failure.
func ValidateFailure(failure Failure) error {
	var errs []error
	if !failure.Code.Valid() {
		errs = append(errs, fmt.Errorf("unsupported failure code %q", failure.Code))
	}
	if len(failure.Message) > MaxFailureMessageBytes || hasControl(failure.Message) {
		errs = append(errs, errors.New("failure message is overlong or contains control characters"))
	}
	return errors.Join(errs...)
}

func validateJWSShape(data []byte) error {
	if err := validateUniqueJSON(data); err != nil {
		return err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return errors.New("expected a JWS JSON object")
	}
	var payload string
	if raw := object["payload"]; len(raw) == 0 || json.Unmarshal(raw, &payload) != nil {
		return errors.New("JWS payload must be a string")
	}
	hasGeneral := len(object["signatures"]) != 0
	hasFlattened := len(object["protected"]) != 0 || len(object["header"]) != 0 || len(object["signature"]) != 0
	if hasGeneral == hasFlattened {
		return errors.New("JWS must use exactly one of general or flattened JSON serialization")
	}
	validateSignature := func(raw json.RawMessage) error {
		var signature map[string]json.RawMessage
		if err := json.Unmarshal(raw, &signature); err != nil || signature == nil {
			return errors.New("JWS signature must be an object")
		}
		var protected, value string
		if json.Unmarshal(signature["protected"], &protected) != nil || protected == "" {
			return errors.New("JWS protected header must be a non-empty string")
		}
		if json.Unmarshal(signature["signature"], &value) != nil || value == "" {
			return errors.New("JWS signature value must be a non-empty string")
		}
		if header := signature["header"]; len(header) != 0 {
			var parsed map[string]json.RawMessage
			if json.Unmarshal(header, &parsed) != nil || parsed == nil {
				return errors.New("JWS header must be an object")
			}
		}
		return nil
	}
	if hasGeneral {
		var signatures []json.RawMessage
		if err := json.Unmarshal(object["signatures"], &signatures); err != nil || len(signatures) == 0 {
			return errors.New("JWS signatures must be a non-empty array")
		}
		for _, signature := range signatures {
			if err := validateSignature(signature); err != nil {
				return err
			}
		}
		return nil
	}
	flattenedObject := map[string]json.RawMessage{"protected": object["protected"], "signature": object["signature"]}
	if len(object["header"]) != 0 {
		flattenedObject["header"] = object["header"]
	}
	flattened, err := json.Marshal(flattenedObject)
	if err != nil {
		return err
	}
	return validateSignature(flattened)
}

func validateCatalogURL(value, digest string) error {
	if err := validateHTTPSURL(value, false); err != nil {
		return err
	}
	parsed, _ := url.Parse(value)
	if parsed.RawQuery != "" {
		return errors.New("catalog URL cannot contain a query")
	}
	want := CatalogDocumentPathPrefix + strings.TrimPrefix(digest, "sha256:")
	if parsed.EscapedPath() != want {
		return fmt.Errorf("catalog URL path %q does not match digest path %q", parsed.EscapedPath(), want)
	}
	return nil
}

func validateHTTPSURL(value string, allowQuery bool) error {
	if value == "" || len(value) > 16<<10 || hasControl(value) {
		return errors.New("URL is empty, overlong, or contains control characters")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.Opaque != "" || parsed.Host == "" {
		return errors.New("URL must be an absolute HTTPS URL")
	}
	if parsed.User != nil {
		return errors.New("URL userinfo is not allowed")
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(value, "#") {
		return errors.New("URL fragments are not allowed")
	}
	if !allowQuery && (parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "?")) {
		return errors.New("URL queries are not allowed")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return errors.New("URL has an explicit empty port")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return fmt.Errorf("URL port %q is not allowed", port)
	}
	if err := validatePublicHostname(parsed.Hostname()); err != nil {
		return err
	}
	return nil
}

func validatePublicHostname(host string) error {
	if host == "" || len(host) > 253 || host != strings.ToLower(host) || strings.HasSuffix(host, ".") {
		return fmt.Errorf("hostname %q is empty, overlong, non-canonical, or has a trailing dot", host)
	}
	if net.ParseIP(host) != nil || numericDottedHost(host) {
		return fmt.Errorf("IP-literal or numeric shorthand hostname %q is not allowed", host)
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return fmt.Errorf("localhost hostname %q is not allowed", host)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return fmt.Errorf("hostname %q must be a public DNS name", host)
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("hostname %q contains an invalid label", host)
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("hostname %q contains unsupported character %q", host, c)
			}
		}
	}
	return nil
}

func numericDottedHost(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return strings.Contains(host, ".")
}

func validateOCIRepository(repository string) error {
	if repository == "" || len(repository) > 255 || repository != strings.ToLower(repository) || strings.Contains(repository, "\\") {
		return fmt.Errorf("repository %q is empty, overlong, non-canonical, or contains a backslash", repository)
	}
	parts := strings.Split(repository, "/")
	if len(parts) < 2 {
		return fmt.Errorf("repository %q must contain at least two path components", repository)
	}
	for _, part := range parts {
		if len(part) == 0 || part == "." || part == ".." || !isASCIILowerAlphaNumeric(part[0]) || !isASCIILowerAlphaNumeric(part[len(part)-1]) {
			return fmt.Errorf("repository %q contains an invalid component", repository)
		}
		for i := range len(part) {
			c := part[i]
			if !isASCIILowerAlphaNumeric(c) && c != '.' && c != '_' && c != '-' {
				return fmt.Errorf("repository %q contains unsupported character %q", repository, c)
			}
		}
	}
	named, err := reference.ParseNormalizedNamed("example.invalid/" + repository)
	if err != nil || reference.Path(named) != repository {
		return fmt.Errorf("repository %q does not follow OCI repository grammar", repository)
	}
	return nil
}

func validatePolicyVersion(name, value string) error {
	if value == "" || len(value) > 256 || hasControl(value) || value != strings.ToLower(value) {
		return fmt.Errorf("%s is empty, overlong, contains control characters, or is not lowercase", name)
	}
	for i := range len(value) {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' || c == '/') {
			return fmt.Errorf("%s contains unsupported character %q", name, c)
		}
	}
	if !isASCIILowerAlphaNumeric(value[0]) || !isASCIILowerAlphaNumeric(value[len(value)-1]) {
		return fmt.Errorf("%s must start and end with a lowercase letter or digit", name)
	}
	return nil
}

func validatePrebuiltArchiveFormat(format, sourceURL string) error {
	if format != PrebuiltArchiveFormatTarGzip {
		return fmt.Errorf("unsupported archive format %q", format)
	}
	if sourceURL == "" {
		return nil
	}
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return err
	}
	archivePath := parsed.Path
	if !strings.HasSuffix(archivePath, ".tar.gz") && !strings.HasSuffix(archivePath, ".tgz") {
		return fmt.Errorf("URL path %q does not match %q", archivePath, format)
	}
	return nil
}

func validateContainedSlashPath(value string) error {
	if value == "" || len(value) > MaxSourcePathBytes || hasControl(value) || strings.Contains(value, "\\") || path.IsAbs(value) {
		return errors.New("must be a bounded relative slash-separated path")
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q is not clean and contained", value)
	}
	return nil
}

func validateFormulaSourceFilePath(value string) error {
	if err := validateContainedSlashPath(value); err != nil {
		return err
	}
	base := path.Base(value)
	if !strings.HasSuffix(base, ".rb") || base == ".rb" {
		return errors.New("path must name a Formula .rb file")
	}
	return validateFormulaName(strings.TrimSuffix(base, ".rb"))
}

func validateFormulaSourcePath(value, formulaName string) error {
	if value == "" || len(value) > MaxSourcePathBytes || hasControl(value) || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return errors.New("must be a bounded relative slash-separated path")
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path %q is not clean and contained", value)
	}
	if path.Base(value) != formulaName+".rb" {
		return fmt.Errorf("path basename %q does not match Formula %q", path.Base(value), formulaName)
	}
	return nil
}

func validateSafeFilename(value string) error {
	if value == "" || len(value) > 255 || hasControl(value) || strings.ContainsAny(value, "/\\") || path.Base(value) != value || value == "." || value == ".." {
		return fmt.Errorf("unsafe filename %q", value)
	}
	return nil
}

func validateCommit(value string) error {
	if len(value) != 40 || !lowerHex(value) {
		return fmt.Errorf("expected 40 lowercase hexadecimal characters")
	}
	return nil
}

func validateSHA256Digest(value string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return errors.New("expected sha256:<hex>")
	}
	hexValue := strings.TrimPrefix(value, prefix)
	if len(hexValue) != sha256.Size*2 || !lowerHex(hexValue) {
		return fmt.Errorf("expected %d lowercase hexadecimal characters", sha256.Size*2)
	}
	if _, err := hex.DecodeString(hexValue); err != nil {
		return err
	}
	return nil
}

func lowerHex(value string) bool {
	for i := range len(value) {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validateCellarPolicy(value string) error {
	switch value {
	case "any", ":any", "any_skip_relocation", ":any_skip_relocation", "/home/linuxbrew/.linuxbrew/Cellar":
		return nil
	default:
		return fmt.Errorf("unsupported Cellar policy %q", value)
	}
}

func validBottleTag(tag string) bool {
	return tag == "x86_64_linux" || tag == "arm64_linux" || tag == "all"
}

func validateOperationID(value string) error {
	if value == "" || value == "." || value == ".." || len(value) > MaxOperationIDBytes {
		return fmt.Errorf("length %d is outside 1..%d", len(value), MaxOperationIDBytes)
	}
	for i := range len(value) {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return fmt.Errorf("contains unsupported character %q", c)
		}
	}
	return nil
}

func validateBoundedText(name, value string, limit int, allowEmpty bool) error {
	if (!allowEmpty && strings.TrimSpace(value) == "") || len(value) > limit || hasControl(value) {
		return fmt.Errorf("%s is empty, overlong, or contains control characters", name)
	}
	return nil
}

func hasControl(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func hasControlExceptWhitespace(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	for _, r := range value {
		if (r < 0x20 && r != '\n' && r != '\r' && r != '\t') || r == 0x7f {
			return true
		}
	}
	return false
}

func sortedTapIDs(values map[TapID]struct{}) []TapID {
	result := make([]TapID, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

// ValidateServiceOrigin validates the one release-bound origin from which
// catalog operations and immutable documents may be fetched.
func ValidateServiceOrigin(value string) error {
	if err := validateHTTPSURL(value, false); err != nil {
		return err
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("catalog service origin must not contain a path, query, or fragment")
	}
	return nil
}

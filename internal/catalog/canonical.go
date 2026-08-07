package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	digest "github.com/opencontainers/go-digest"
)

// DecodeRequest strictly decodes, canonicalizes, and validates a request.
func DecodeRequest(data []byte) (*Request, error) {
	return decodeDocument(data, MaxRequestBytes, "catalog request", canonicalizeRequest, ValidateRequest)
}

// DecodeTapCatalog strictly decodes, canonicalizes, and validates a tap
// catalog, rejecting documents over 64 MiB.
func DecodeTapCatalog(data []byte) (*TapCatalog, error) {
	return decodeDocument(data, MaxCatalogDocumentBytes, "tap catalog", canonicalizeTapCatalog, ValidateTapCatalog)
}

// DecodeCatalogSetPayload strictly decodes, canonicalizes, and validates an
// authenticated catalog-set payload.
func DecodeCatalogSetPayload(data []byte) (*CatalogSetPayload, error) {
	return decodeDocument(data, MaxCatalogSetBytes, "catalog-set payload", canonicalizeCatalogSetPayload, ValidateCatalogSetPayload)
}

// DecodeCatalogSetResult strictly decodes and validates a completed result.
func DecodeCatalogSetResult(data []byte) (*CatalogSetResult, error) {
	return decodeDocument(data, MaxOperationBytes, "catalog-set result", func(*CatalogSetResult) {}, ValidateCatalogSetResult)
}

// DecodeOperation strictly decodes and validates an operation response.
func DecodeOperation(data []byte) (*Operation, error) {
	return decodeDocument(data, MaxOperationBytes, "catalog operation", func(*Operation) {}, ValidateOperation)
}

// CanonicalRequest returns stable JSON without mutating request.
func CanonicalRequest(request *Request) ([]byte, error) {
	if request == nil {
		return nil, errors.New("nil catalog request")
	}
	if err := ValidateRequest(request); err != nil {
		return nil, fmt.Errorf("validate catalog request: %w", err)
	}
	clone := *request
	clone.Targets = clonePlatformRequests(request.Targets)
	clone.ExternalRoots = slices.Clone(request.ExternalRoots)
	clone.Platforms = slices.Clone(request.Platforms)
	canonicalizeRequest(&clone)
	if err := ValidateRequest(&clone); err != nil {
		return nil, fmt.Errorf("validate canonical catalog request: %w", err)
	}
	data, err := encodeCanonical(clone)
	if err != nil {
		return nil, fmt.Errorf("encode canonical catalog request: %w", err)
	}
	if int64(len(data)) > MaxRequestBytes {
		return nil, fmt.Errorf("canonical catalog request exceeds %d bytes", MaxRequestBytes)
	}
	return data, nil
}

// RequestDigest returns the SHA-256 digest of CanonicalRequest.
func RequestDigest(request *Request) (digest.Digest, error) {
	data, err := CanonicalRequest(request)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

// CanonicalTapCatalog returns stable content-addressed JSON without mutating
// catalog.
func CanonicalTapCatalog(catalog *TapCatalog) ([]byte, error) {
	return canonicalDocument(catalog, "tap catalog", MaxCatalogDocumentBytes, canonicalizeTapCatalog, ValidateTapCatalog)
}

// TapCatalogDigest returns the SHA-256 digest of CanonicalTapCatalog.
func TapCatalogDigest(catalog *TapCatalog) (digest.Digest, error) {
	data, err := CanonicalTapCatalog(catalog)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

// CanonicalCatalogSetPayload returns stable JSON for signing without mutating
// payload.
func CanonicalCatalogSetPayload(payload *CatalogSetPayload) ([]byte, error) {
	return canonicalDocument(payload, "catalog-set payload", MaxCatalogSetBytes, canonicalizeCatalogSetPayload, ValidateCatalogSetPayload)
}

// CatalogSetPayloadDigest returns the SHA-256 digest of the canonical signed
// payload.
func CatalogSetPayloadDigest(payload *CatalogSetPayload) (digest.Digest, error) {
	data, err := CanonicalCatalogSetPayload(payload)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

// CanonicalPrebuiltArchiveDeclaration returns stable JSON for policy and
// catalog construction without mutating declaration.
func CanonicalPrebuiltArchiveDeclaration(declaration PrebuiltArchiveDeclaration) ([]byte, error) {
	if err := ValidatePrebuiltArchiveDeclaration(declaration); err != nil {
		return nil, fmt.Errorf("validate prebuilt archive declaration: %w", err)
	}
	clone, err := cloneValue(declaration)
	if err != nil {
		return nil, fmt.Errorf("clone prebuilt archive declaration: %w", err)
	}
	canonicalizePrebuiltArchiveDeclaration(&clone)
	if err := ValidatePrebuiltArchiveDeclaration(clone); err != nil {
		return nil, fmt.Errorf("validate canonical prebuilt archive declaration: %w", err)
	}
	return encodeCanonical(clone)
}

// CanonicalPrebuiltDerivation returns stable JSON for signed derivation
// evidence without mutating derivation.
func CanonicalPrebuiltDerivation(derivation PrebuiltDerivation) ([]byte, error) {
	if err := ValidatePrebuiltDerivation(derivation); err != nil {
		return nil, fmt.Errorf("validate prebuilt derivation: %w", err)
	}
	clone, err := cloneValue(derivation)
	if err != nil {
		return nil, fmt.Errorf("clone prebuilt derivation: %w", err)
	}
	canonicalizePrebuiltDerivation(&clone)
	if err := ValidatePrebuiltDerivation(clone); err != nil {
		return nil, fmt.Errorf("validate canonical prebuilt derivation: %w", err)
	}
	return encodeCanonical(clone)
}

// CanonicalClosureResult returns stable JSON for independent closure
// comparison. InstallOrder remains ordered because it is materialization input.
func CanonicalClosureResult(closure ClosureResult) ([]byte, error) {
	if err := ValidateClosureResult(closure); err != nil {
		return nil, err
	}
	clone, err := cloneValue(closure)
	if err != nil {
		return nil, err
	}
	canonicalizeClosure(&clone)
	if err := ValidateClosureResult(clone); err != nil {
		return nil, err
	}
	return encodeCanonical(clone)
}

// ClosureResultDigest returns the SHA-256 digest of CanonicalClosureResult.
func ClosureResultDigest(closure ClosureResult) (digest.Digest, error) {
	data, err := CanonicalClosureResult(closure)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

// CanonicalPlatformResult returns stable JSON for comparing a signed service
// result with an independently recomputed frontend result.
func CanonicalPlatformResult(result PlatformResult) ([]byte, error) {
	if err := ValidatePlatformResult(result); err != nil {
		return nil, err
	}
	clone, err := cloneValue(result)
	if err != nil {
		return nil, err
	}
	canonicalizePlatformResult(&clone)
	if err := ValidatePlatformResult(clone); err != nil {
		return nil, err
	}
	return encodeCanonical(clone)
}

// PlatformResultDigest returns the SHA-256 digest of CanonicalPlatformResult.
func PlatformResultDigest(result PlatformResult) (digest.Digest, error) {
	data, err := CanonicalPlatformResult(result)
	if err != nil {
		return "", err
	}
	return digest.FromBytes(data), nil
}

func decodeDocument[T any](data []byte, limit int64, name string, canonicalize func(*T), validate func(*T) error) (*T, error) {
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s is not valid UTF-8", name)
	}
	if err := validateUniqueJSON(data); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode %s: %w", name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("decode %s: multiple JSON values", name)
		}
		return nil, fmt.Errorf("decode %s trailing data: %w", name, err)
	}
	canonicalize(&value)
	if err := validate(&value); err != nil {
		return nil, fmt.Errorf("validate %s: %w", name, err)
	}
	return &value, nil
}

func canonicalDocument[T any](value *T, name string, limit int64, canonicalize func(*T), validate func(*T) error) ([]byte, error) {
	if value == nil {
		return nil, fmt.Errorf("nil %s", name)
	}
	if err := validate(value); err != nil {
		return nil, fmt.Errorf("validate %s: %w", name, err)
	}
	clone, err := cloneValue(*value)
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", name, err)
	}
	canonicalize(&clone)
	if err := validate(&clone); err != nil {
		return nil, fmt.Errorf("validate canonical %s: %w", name, err)
	}
	data, err := encodeCanonical(clone)
	if err != nil {
		return nil, fmt.Errorf("encode canonical %s: %w", name, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("canonical %s exceeds %d bytes", name, limit)
	}
	return data, nil
}

func cloneValue[T any](value T) (T, error) {
	var clone T
	data, err := json.Marshal(value)
	if err != nil {
		return clone, err
	}
	if err := json.Unmarshal(data, &clone); err != nil {
		return clone, err
	}
	return clone, nil
}

func encodeCanonical(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

func canonicalizeRequest(request *Request) {
	targets, err := normalizedRequestTargets(request)
	if err == nil {
		request.Targets = targets
	}
	request.ExternalRoots = nil
	request.Platforms = nil
	for i := range request.Targets {
		slices.Sort(request.Targets[i].ExternalRoots)
		slices.Sort(request.Targets[i].CoreRoots)
	}
	slices.SortFunc(request.Targets, func(left, right PlatformRequest) int {
		return comparePlatform(left.Platform, right.Platform)
	})
}

func canonicalizeTapCatalog(catalog *TapCatalog) {
	catalog.PublishedAt = catalog.PublishedAt.UTC().Round(0)
	slices.SortFunc(catalog.Formulae, func(left, right Formula) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	for i := range catalog.Formulae {
		canonicalizeFormula(&catalog.Formulae[i])
	}
	sortMappings(catalog.Aliases)
	sortMappings(catalog.Renames)
	slices.SortFunc(catalog.Migrations, func(left, right Migration) int {
		if compared := strings.Compare(string(left.From), string(right.From)); compared != 0 {
			return compared
		}
		if compared := strings.Compare(left.RawTarget, right.RawTarget); compared != 0 {
			return compared
		}
		return strings.Compare(string(left.To), string(right.To))
	})
	nilEmptySlicesTapCatalog(catalog)
}

func canonicalizeFormula(formula *Formula) {
	sortDependencies(formula.Dependencies)
	slices.SortFunc(formula.Variations, func(left, right FormulaVariation) int {
		return strings.Compare(left.Tag, right.Tag)
	})
	for i := range formula.Variations {
		sortDependencies(formula.Variations[i].Dependencies)
		if len(formula.Variations[i].Dependencies) == 0 {
			formula.Variations[i].Dependencies = nil
		}
	}
	slices.Sort(formula.VersionedFormulae)
	if formula.Bottle != nil {
		slices.SortFunc(formula.Bottle.Files, func(left, right BottleFile) int {
			return strings.Compare(left.Tag, right.Tag)
		})
	}
	if formula.PrebuiltArchive != nil {
		canonicalizePrebuiltArchiveDeclaration(formula.PrebuiltArchive)
	}
	if len(formula.Dependencies) == 0 {
		formula.Dependencies = nil
	}
	if len(formula.Variations) == 0 {
		formula.Variations = nil
	}
	if len(formula.VersionedFormulae) == 0 {
		formula.VersionedFormulae = nil
	}
}

func canonicalizeCatalogSetPayload(payload *CatalogSetPayload) {
	payload.GeneratedAt = payload.GeneratedAt.UTC().Round(0)
	payload.ExpiresAt = payload.ExpiresAt.UTC().Round(0)
	slices.SortFunc(payload.Catalogs, func(left, right CatalogReference) int {
		return strings.Compare(string(left.Tap.ID), string(right.Tap.ID))
	})
	for i := range payload.Catalogs {
		payload.Catalogs[i].PublishedAt = payload.Catalogs[i].PublishedAt.UTC().Round(0)
	}
	slices.SortFunc(payload.Results, func(left, right PlatformResult) int {
		return comparePlatform(left.Platform, right.Platform)
	})
	for i := range payload.Results {
		canonicalizePlatformResult(&payload.Results[i])
	}
}

func canonicalizePlatformResult(result *PlatformResult) {
	canonicalizeClosure(&result.Closure)
	slices.SortFunc(result.Artifacts, func(left, right BottleArtifact) int {
		if compared := strings.Compare(string(left.ID), string(right.ID)); compared != 0 {
			return compared
		}
		return comparePlatform(left.Platform, right.Platform)
	})
	for i := range result.Artifacts {
		canonicalizeArtifact(&result.Artifacts[i])
	}
}

func canonicalizeClosure(closure *ClosureResult) {
	if closure.RequestedMappings == nil {
		closure.RequestedMappings = make([]RequestedMapping, len(closure.Requested))
		for i, id := range closure.Requested {
			closure.RequestedMappings[i] = RequestedMapping{Requested: id, Resolved: id}
		}
	}
	if closure.NormalizationTaps == nil {
		seen := map[TapID]struct{}{}
		for _, node := range closure.Nodes {
			if !node.ID.IsCore() {
				seen[node.Tap] = struct{}{}
			}
		}
		for tap := range seen {
			closure.NormalizationTaps = append(closure.NormalizationTaps, tap)
		}
	}
	slices.Sort(closure.NormalizationTaps)
	slices.SortFunc(closure.RequestedMappings, func(a, b RequestedMapping) int {
		if compared := strings.Compare(string(a.Requested), string(b.Requested)); compared != 0 {
			return compared
		}
		return strings.Compare(string(a.Resolved), string(b.Resolved))
	})
	slices.Sort(closure.Requested)
	slices.SortFunc(closure.Nodes, func(left, right Node) int {
		return strings.Compare(string(left.ID), string(right.ID))
	})
	for i := range closure.Nodes {
		slices.SortFunc(closure.Nodes[i].Dependencies, func(left, right Requirement) int {
			if compared := strings.Compare(string(left.ID), string(right.ID)); compared != 0 {
				return compared
			}
			return strings.Compare(left.Raw, right.Raw)
		})
		if len(closure.Nodes[i].Dependencies) == 0 {
			closure.Nodes[i].Dependencies = nil
		}
	}
}

func canonicalizeArtifact(artifact *BottleArtifact) {
	slices.Sort(artifact.Tab.ChangedFiles)
	slices.Sort(artifact.ExecutablePaths)
	slices.SortFunc(artifact.Tab.Dependencies, func(a, b BottleRuntimeDependency) int { return strings.Compare(string(a.ID), string(b.ID)) })
	if artifact.Transport.HTTPS != nil {
		slices.Sort(artifact.Transport.HTTPS.AllowedRedirectHosts)
	}
	if artifact.Transport.OCI != nil {
		for _, descriptor := range []*Descriptor{
			&artifact.Transport.OCI.Index,
			&artifact.Transport.OCI.Manifest,
			&artifact.Transport.OCI.Config,
			&artifact.Transport.OCI.Layer,
		} {
			slices.SortFunc(descriptor.Annotations, func(left, right Annotation) int {
				if compared := strings.Compare(left.Key, right.Key); compared != 0 {
					return compared
				}
				return strings.Compare(left.Value, right.Value)
			})
			if len(descriptor.Annotations) == 0 {
				descriptor.Annotations = nil
			}
		}
	}
	if artifact.PrebuiltDerivation != nil {
		canonicalizePrebuiltDerivation(artifact.PrebuiltDerivation)
	}
}

func canonicalizePrebuiltArchiveDeclaration(declaration *PrebuiltArchiveDeclaration) {
	slices.SortFunc(declaration.Files, func(left, right PrebuiltArchiveFile) int {
		return strings.Compare(left.Tag, right.Tag)
	})
}

func canonicalizePrebuiltDerivation(derivation *PrebuiltDerivation) {
	if derivation.Source.Transport.HTTPS != nil {
		slices.Sort(derivation.Source.Transport.HTTPS.AllowedRedirectHosts)
	}
	slices.Sort(derivation.ELF.NeededLibraries)
	slices.Sort(derivation.ELF.RPaths)
}

func comparePlatform(left, right Platform) int {
	if compared := strings.Compare(left.OS, right.OS); compared != 0 {
		return compared
	}
	if compared := strings.Compare(left.Architecture, right.Architecture); compared != 0 {
		return compared
	}
	return strings.Compare(left.Variant, right.Variant)
}

func sortDependencies(dependencies []Dependency) {
	slices.SortFunc(dependencies, func(left, right Dependency) int {
		if compared := strings.Compare(string(left.ID), string(right.ID)); compared != 0 {
			return compared
		}
		return strings.Compare(left.Raw, right.Raw)
	})
}

func sortMappings(mappings []ScopedMapping) {
	slices.SortFunc(mappings, func(left, right ScopedMapping) int {
		if compared := strings.Compare(string(left.From), string(right.From)); compared != 0 {
			return compared
		}
		return strings.Compare(string(left.To), string(right.To))
	})
}

func nilEmptySlicesTapCatalog(catalog *TapCatalog) {
	if len(catalog.Aliases) == 0 {
		catalog.Aliases = nil
	}
	if len(catalog.Renames) == 0 {
		catalog.Renames = nil
	}
	if len(catalog.Migrations) == 0 {
		catalog.Migrations = nil
	}
}

// validateUniqueJSON rejects duplicate object members at every nesting level.
// encoding/json otherwise accepts the last value, which is unsafe for signed
// catalog policy and digest fields.
func validateUniqueJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := walkUniqueJSONValue(decoder, "$", nil, 0); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected trailing JSON token %v", token)
	}
	return nil
}

func walkUniqueJSONValue(decoder *json.Decoder, path string, first json.Token, depth int) error {
	if depth > MaxJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels at %s", MaxJSONDepth, path)
	}
	token := first
	var err error
	if token == nil {
		token, err = decoder.Token()
		if err != nil {
			return err
		}
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object at %s has a non-string key", path)
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON member %q at %s", key, path)
			}
			seen[key] = struct{}{}
			if err := walkUniqueJSONValue(decoder, path+"."+key, nil, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("JSON object at %s has invalid closing token", path)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := walkUniqueJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index), nil, depth+1); err != nil {
				return err
			}
			index++
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("JSON array at %s has invalid closing token", path)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q at %s", delimiter, path)
	}
	return nil
}

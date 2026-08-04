package oci

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

// DiscoverAuthenticatedFormulaIdentity reads the digest-verified OCI index and
// derives the exact historical source commit and Formula path recorded by the
// bottle publisher. The current tap catalog authenticates the Formula ID,
// default repository, version, and checksum; the signed catalog-set later binds
// this independently verified bottle-build identity.
func (client *Client) DiscoverAuthenticatedFormulaIdentity(ctx context.Context, id formulaid.FormulaID, homebrewFullName, stableVersion string, revision, bottleRebuild int) (AuthenticatedFormulaIdentity, error) {
	if client == nil {
		return AuthenticatedFormulaIdentity{}, errors.New("nil OCI client")
	}
	if err := validateFormulaID(id); err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	if homebrewFullName != id.String() {
		return AuthenticatedFormulaIdentity{}, fmt.Errorf("Homebrew full name %q does not match Formula ID %q", homebrewFullName, id.String())
	}
	repository, err := RepositoryPathForFormulaID(id)
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	pkgVersion, err := PkgVersion(stableVersion, revision)
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	indexTag, err := IndexTag(pkgVersion, bottleRebuild)
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	content, err := client.FetchIndex(ctx, repository, indexTag)
	if err != nil {
		return AuthenticatedFormulaIdentity{}, fmt.Errorf("fetch Formula %q OCI index for source discovery: %w", id.String(), err)
	}
	var index ocispec.Index
	if err := decodeJSON(content.Bytes, &index); err != nil {
		return AuthenticatedFormulaIdentity{}, fmt.Errorf("decode Formula %q OCI index for source discovery: %w", id.String(), err)
	}
	for key, expected := range map[string]string{
		annotationPackageType:     homebrewBottlePackageType,
		ocispec.AnnotationVendor:  id.Tap().Owner(),
		ocispec.AnnotationTitle:   homebrewFullName,
		ocispec.AnnotationVersion: pkgVersion,
		ocispec.AnnotationRefName: indexTag,
	} {
		if actual := index.Annotations[key]; actual != expected {
			return AuthenticatedFormulaIdentity{}, fmt.Errorf("index annotation %q is %q, expected %q", key, actual, expected)
		}
	}
	commit := index.Annotations[ocispec.AnnotationRevision]
	if err := validateSourceCommit(commit); err != nil {
		return AuthenticatedFormulaIdentity{}, fmt.Errorf("index source commit: %w", err)
	}
	repositoryURL, err := DefaultGitHubRepository(id.Tap())
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	formulaPath, err := parseAuthenticatedSourceURL(index.Annotations[ocispec.AnnotationSource], repositoryURL, commit, id.Name())
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	return NewAuthenticatedFormulaIdentity(id, homebrewFullName, repositoryURL, commit, formulaPath)
}

func parseAuthenticatedSourceURL(raw, repository, commit, formulaName string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("OCI source annotation is not a canonical public GitHub URL")
	}
	prefix := repository + "/blob/" + commit + "/"
	if !strings.HasPrefix(raw, prefix) {
		return "", fmt.Errorf("OCI source annotation %q does not match repository and revision", raw)
	}
	formulaPath := strings.TrimPrefix(raw, prefix)
	if err := validateFormulaSourcePath(formulaPath, formulaName); err != nil {
		return "", fmt.Errorf("OCI source Formula path: %w", err)
	}
	if raw != prefix+formulaPath {
		return "", errors.New("OCI source annotation is not canonical")
	}
	return formulaPath, nil
}

// DiscoverCoreFormulaIdentity derives the exact historical homebrew/core
// source revision from the verified OCI index while preserving the V1 core
// descriptor-validation path.
func (client *Client) DiscoverCoreFormulaIdentity(ctx context.Context, formula Formula) (AuthenticatedFormulaIdentity, error) {
	if client == nil {
		return AuthenticatedFormulaIdentity{}, errors.New("nil OCI client")
	}
	reference, err := ResolveFormulaReference(formula)
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	content, err := client.FetchIndex(ctx, reference.Repository, reference.IndexTag)
	if err != nil {
		return AuthenticatedFormulaIdentity{}, fmt.Errorf("fetch core Formula %q OCI index for source discovery: %w", formula.Name, err)
	}
	var index ocispec.Index
	if err := decodeJSON(content.Bytes, &index); err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	if err := validateCommonAnnotations(index.Annotations, formula.Name, reference.PkgVersion, reference.IndexTag, formula.Name); err != nil {
		return AuthenticatedFormulaIdentity{}, fmt.Errorf("core Formula source annotations: %w", err)
	}
	commit := index.Annotations[ocispec.AnnotationRevision]
	if err := validateSourceCommit(commit); err != nil {
		return AuthenticatedFormulaIdentity{}, fmt.Errorf("core index source commit: %w", err)
	}
	id, err := formulaid.Parse("homebrew/core/" + formula.Name)
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	repositoryURL, err := DefaultGitHubRepository(id.Tap())
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	formulaPath, err := parseAuthenticatedSourceURL(index.Annotations[ocispec.AnnotationSource], repositoryURL, commit, formula.Name)
	if err != nil {
		return AuthenticatedFormulaIdentity{}, err
	}
	return NewAuthenticatedFormulaIdentity(id, id.String(), repositoryURL, commit, formulaPath)
}

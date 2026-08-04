package oci

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/formulaid"
)

type authenticatedRegistryFixtureOptions struct {
	formulaID                 string
	bottleTag                 string
	target                    ocispec.Platform
	descriptorPlatform        *ocispec.Platform
	configPlatform            ocispec.Platform
	tab                       string
	authenticatedChecksum     string
	manifestLayerDigest       digest.Digest
	mutateManifest            bool
	mutateConfig              bool
	mutateIndexAnnotations    func(map[string]string)
	mutateChildAnnotations    func(map[string]string)
	mutateManifestAnnotations func(map[string]string)
}

type authenticatedRegistryFixture struct {
	server         *httptest.Server
	client         *Client
	formula        AuthenticatedFormula
	target         ocispec.Platform
	reference      FormulaReference
	tokenRequests  atomic.Int64
	challengeCount atomic.Int64
}

func newAuthenticatedRegistryFixture(t *testing.T, options authenticatedRegistryFixtureOptions) *authenticatedRegistryFixture {
	t.Helper()

	if options.formulaID == "" {
		options.formulaID = "acme/tools/fixture"
	}
	id, err := formulaid.Parse(options.formulaID)
	if err != nil {
		t.Fatal(err)
	}
	if options.bottleTag == "" {
		options.bottleTag = BottleTagX8664Linux
	}
	if options.target.OS == "" {
		options.target = ocispec.Platform{OS: "linux", Architecture: "amd64"}
	}
	if options.configPlatform.OS == "" {
		options.configPlatform = options.target
	}
	if options.descriptorPlatform == nil && options.bottleTag != BottleTagAll {
		platform := options.target
		options.descriptorPlatform = &platform
	}

	sourceRepository, err := DefaultGitHubRepository(id.Tap())
	if err != nil {
		t.Fatal(err)
	}
	rootURL, err := HomebrewGHCRRootURL(id.Tap())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NewAuthenticatedFormulaIdentity(
		id,
		id.String(),
		sourceRepository,
		strings.Repeat("a", 40),
		"Formula/"+id.Name()+".rb",
	)
	if err != nil {
		t.Fatal(err)
	}

	if options.tab == "" {
		if options.bottleTag == BottleTagAll {
			options.tab = fmt.Sprintf(`{"homebrew_version":"5.0.0","compiler":"gcc-13","runtime_dependencies":[{"full_name":%q,"version":"1.2","revision":1,"bottle_rebuild":2,"pkg_version":"1.2_1","declared_directly":true}]}`, id.Tap().String()+"/dep")
		} else {
			arch := "x86_64"
			if options.target.Architecture == "arm64" {
				arch = "arm64"
			}
			options.tab = fmt.Sprintf(`{"homebrew_version":"5.0.0","compiler":"gcc-13","runtime_dependencies":[{"full_name":%q,"version":"1.2","revision":1,"bottle_rebuild":2,"pkg_version":"1.2_1","declared_directly":true}],"arch":%q,"built_on":{"os":"Linux","os_version":"Ubuntu 24.04","cpu_family":"test","oldest_cpu_family":"test","glibc_version":"2.39"}}`, id.Tap().String()+"/dep", arch)
		}
	}

	layerBytes := []byte("authenticated fixture bottle layer")
	layerDigest := digest.FromBytes(layerBytes)
	if options.authenticatedChecksum == "" {
		options.authenticatedChecksum = layerDigest.Encoded()
	}
	if options.manifestLayerDigest == "" {
		options.manifestLayerDigest = layerDigest
	}
	formula := AuthenticatedFormula{
		Identity:      identity,
		BottleRootURL: rootURL,
		StableVersion: "1.2.3",
		Revision:      1,
		VersionScheme: 2,
		BottleRebuild: 3,
		License:       "MIT",
		KegOnly:       true,
		BottleFiles: map[string]BottleFile{
			options.bottleTag: {
				Cellar: "/home/linuxbrew/.linuxbrew/Cellar",
				SHA256: options.authenticatedChecksum,
			},
		},
	}
	reference, err := ResolveAuthenticatedFormulaReference(formula)
	if err != nil {
		t.Fatal(err)
	}
	childTag, err := ChildTag(reference.PkgVersion, options.bottleTag, formula.BottleRebuild)
	if err != nil {
		t.Fatal(err)
	}
	filename, err := BottleFilename(identity.Name(), reference.PkgVersion, options.bottleTag, formula.BottleRebuild)
	if err != nil {
		t.Fatal(err)
	}

	config := ocispec.Image{
		Platform: options.configPlatform,
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{digest.FromBytes([]byte("uncompressed authenticated fixture bottle"))},
		},
	}
	configBytes := mustJSON(t, config)
	configDescriptor := descriptor(ocispec.MediaTypeImageConfig, configBytes)

	selectedAnnotations := map[string]string{
		ocispec.AnnotationRefName: childTag,
		annotationBottleDigest:    layerDigest.Encoded(),
		annotationBottleSize:      fmt.Sprintf("%d", len(layerBytes)),
		annotationBottleLicense:   formula.License,
		annotationBottleTab:       options.tab,
		annotationPathExecFiles:   "bin/fixture,sbin/fixture-helper",
	}
	if options.mutateChildAnnotations != nil {
		options.mutateChildAnnotations(selectedAnnotations)
	}
	manifestAnnotations := authenticatedFixtureCommonAnnotations(identity, reference, childTag, identity.HomebrewFullName()+" "+childTag)
	for key, value := range selectedAnnotations {
		manifestAnnotations[key] = value
	}
	if options.mutateManifestAnnotations != nil {
		options.mutateManifestAnnotations(manifestAnnotations)
	}
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Config:    configDescriptor,
		Layers: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageLayerGzip,
			Digest:    options.manifestLayerDigest,
			Size:      int64(len(layerBytes)),
			Annotations: map[string]string{
				ocispec.AnnotationTitle: filename,
			},
		}},
		Annotations: manifestAnnotations,
	}
	manifestBytes := mustJSON(t, manifest)
	manifestDescriptor := descriptor(ocispec.MediaTypeImageManifest, manifestBytes)
	manifestDescriptor.Platform = options.descriptorPlatform
	manifestDescriptor.Annotations = selectedAnnotations

	servedManifestBytes := append([]byte(nil), manifestBytes...)
	if options.mutateManifest {
		position := strings.Index(string(servedManifestBytes), identity.HomebrewFullName()+" "+childTag)
		if position < 0 {
			t.Fatal("could not locate authenticated manifest mutation target")
		}
		servedManifestBytes[position] ^= 1
	}
	servedConfigBytes := append([]byte(nil), configBytes...)
	if options.mutateConfig {
		position := strings.Index(string(servedConfigBytes), options.configPlatform.Architecture)
		if position < 0 {
			t.Fatal("could not locate config mutation target")
		}
		servedConfigBytes[position] ^= 1
	}

	indexAnnotations := authenticatedFixtureCommonAnnotations(identity, reference, reference.IndexTag, identity.HomebrewFullName())
	if options.mutateIndexAnnotations != nil {
		options.mutateIndexAnnotations(indexAnnotations)
	}
	index := ocispec.Index{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		Manifests:   []ocispec.Descriptor{manifestDescriptor},
		Annotations: indexAnnotations,
	}
	indexBytes := mustJSON(t, index)

	fixture := &authenticatedRegistryFixture{formula: formula, target: options.target, reference: reference}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/token" {
			fixture.tokenRequests.Add(1)
			if request.URL.Query().Get("service") != "fixture-registry" {
				http.Error(writer, "wrong service", http.StatusBadRequest)
				return
			}
			if request.URL.Query().Get("scope") != "repository:"+reference.Repository+":pull" {
				http.Error(writer, "wrong scope", http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"token":"placeholder","expires_in":300}`))
			return
		}
		if request.Header.Get("Authorization") != "Bearer placeholder" {
			fixture.challengeCount.Add(1)
			writer.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="fixture-registry",scope=%q`, server.URL+"/token", "repository:"+reference.Repository+":pull"))
			http.Error(writer, "authentication required", http.StatusUnauthorized)
			return
		}

		indexPath := "/v2/" + reference.Repository + "/manifests/" + reference.IndexTag
		manifestPath := "/v2/" + reference.Repository + "/manifests/" + manifestDescriptor.Digest.String()
		configPath := "/v2/" + reference.Repository + "/blobs/" + configDescriptor.Digest.String()
		switch request.URL.Path {
		case indexPath:
			writeOCI(writer, ocispec.MediaTypeImageIndex, indexBytes)
		case manifestPath:
			writeOCI(writer, ocispec.MediaTypeImageManifest, servedManifestBytes)
		case configPath:
			writer.Header().Set("Content-Type", "application/octet-stream")
			_, _ = writer.Write(servedConfigBytes)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	fixture.server = server
	fixture.client, err = NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func authenticatedFixtureCommonAnnotations(identity AuthenticatedFormulaIdentity, reference FormulaReference, refName, title string) map[string]string {
	return map[string]string{
		annotationPackageType:      homebrewBottlePackageType,
		ocispec.AnnotationVendor:   identity.Publisher(),
		ocispec.AnnotationTitle:    title,
		ocispec.AnnotationVersion:  reference.PkgVersion,
		ocispec.AnnotationRefName:  refName,
		ocispec.AnnotationRevision: identity.SourceCommit(),
		ocispec.AnnotationSource:   identity.SourceURL(),
	}
}

package oci

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type registryFixtureOptions struct {
	formulaName        string
	bottleTag          string
	target             ocispec.Platform
	descriptorPlatform *ocispec.Platform
	configPlatform     ocispec.Platform
	tab                string
	mutateManifest     bool
}

type registryFixture struct {
	server         *httptest.Server
	client         *Client
	formula        Formula
	target         ocispec.Platform
	tokenRequests  atomic.Int64
	challengeCount atomic.Int64
}

func newRegistryFixture(t *testing.T, options registryFixtureOptions) *registryFixture {
	t.Helper()

	if options.formulaName == "" {
		options.formulaName = "fixture"
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
	if options.tab == "" {
		if options.bottleTag == BottleTagAll {
			options.tab = `{"homebrew_version":"5.0.0","compiler":"gcc-13","runtime_dependencies":[{"full_name":"homebrew/core/dep","version":"1.2","revision":1,"bottle_rebuild":2,"pkg_version":"1.2_1","declared_directly":true}]}`
		} else {
			arch := "x86_64"
			if options.target.Architecture == "arm64" {
				arch = "arm64"
			}
			options.tab = fmt.Sprintf(`{"homebrew_version":"5.0.0","compiler":"gcc-13","runtime_dependencies":[{"full_name":"dep","version":"1.2","revision":1,"bottle_rebuild":2,"pkg_version":"1.2_1","declared_directly":true}],"arch":%q,"built_on":{"os":"Linux","os_version":"Ubuntu 24.04","cpu_family":"test","oldest_cpu_family":"test","glibc_version":"2.39"}}`, arch)
		}
	}

	layerBytes := []byte("fixture bottle layer")
	layerDigest := digest.FromBytes(layerBytes)
	formula := Formula{
		Name:          options.formulaName,
		FullName:      "homebrew/core/" + options.formulaName,
		StableVersion: "1.2.3",
		Revision:      1,
		VersionScheme: 2,
		BottleRebuild: 3,
		License:       "MIT",
		KegOnly:       true,
		BottleFiles: map[string]BottleFile{
			options.bottleTag: {
				Cellar: "/home/linuxbrew/.linuxbrew/Cellar",
				SHA256: layerDigest.Encoded(),
			},
		},
	}
	reference, err := ResolveFormulaReference(formula)
	if err != nil {
		t.Fatal(err)
	}
	childTag, err := ChildTag(reference.PkgVersion, options.bottleTag, formula.BottleRebuild)
	if err != nil {
		t.Fatal(err)
	}
	filename, err := BottleFilename(formula.Name, reference.PkgVersion, options.bottleTag, formula.BottleRebuild)
	if err != nil {
		t.Fatal(err)
	}

	config := ocispec.Image{
		Platform: options.configPlatform,
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []digest.Digest{digest.FromBytes([]byte("uncompressed fixture bottle"))},
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
	manifestAnnotations := commonAnnotations(formula, reference, childTag, formula.Name+" "+childTag)
	for key, value := range selectedAnnotations {
		manifestAnnotations[key] = value
	}
	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		Config:    configDescriptor,
		Layers: []ocispec.Descriptor{{
			MediaType: ocispec.MediaTypeImageLayerGzip,
			Digest:    layerDigest,
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
		position := strings.Index(string(servedManifestBytes), "fixture 1.2.3_1")
		if position < 0 {
			t.Fatal("could not locate manifest mutation target")
		}
		servedManifestBytes[position] = 'F'
	}

	index := ocispec.Index{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		Manifests:   []ocispec.Descriptor{manifestDescriptor},
		Annotations: commonAnnotations(formula, reference, reference.IndexTag, formula.Name),
	}
	indexBytes := mustJSON(t, index)

	fixture := &registryFixture{formula: formula, target: options.target}
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
			_, _ = writer.Write(configBytes)
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

func commonAnnotations(formula Formula, reference FormulaReference, refName, title string) map[string]string {
	return map[string]string{
		annotationPackageType:     homebrewBottlePackageType,
		ocispec.AnnotationVendor:  "homebrew",
		ocispec.AnnotationTitle:   title,
		ocispec.AnnotationVersion: reference.PkgVersion,
		ocispec.AnnotationRefName: refName,
		ocispec.AnnotationSource:  "https://github.com/homebrew/homebrew-core/blob/0123456789abcdef/Formula/f/fixture.rb",
	}
}

func descriptor(mediaType string, data []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: digest.FromBytes(data), Size: int64(len(data))}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeOCI(writer http.ResponseWriter, mediaType string, data []byte) {
	writer.Header().Set("Content-Type", mediaType)
	writer.Header().Set("Docker-Content-Digest", digest.FromBytes(data).String())
	_, _ = writer.Write(data)
}

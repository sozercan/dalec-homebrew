package resolution

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func validRecordV2() *RecordV2 {
	coreGenerated := time.Unix(1_800_000_000, 123_000_000).UTC()
	externalGenerated := coreGenerated.Add(time.Hour)
	resolvedAt := externalGenerated.Add(2 * time.Minute)
	shaA := strings.Repeat("a", 64)
	shaB := strings.Repeat("b", 64)
	shaC := strings.Repeat("c", 64)
	shaD := strings.Repeat("d", 64)
	digestA := "sha256:" + shaA
	digestD := "sha256:" + shaD
	descriptor := func(value string) Descriptor {
		return Descriptor{Digest: value, Size: 1, MediaType: "application/test"}
	}
	manifest := descriptor(digestA)
	manifest.Platform = &Platform{OS: "linux", Architecture: "amd64"}

	return &RecordV2{
		SchemaVersion: SchemaVersionV2,
		PolicyVersion: PolicyVersionV2,
		Input: Input{
			DalecSpecDigest: digestA,
			Platform:        Platform{OS: "linux", Architecture: "amd64"},
		},
		MetadataSources: []MetadataSource{
			{
				Tap:                  "acme/tools",
				Commit:               strings.Repeat("2", 40),
				CatalogPolicyVersion: "tap-catalog-v1",
				Signer:               Signature{KeyID: "catalog-service-1", Algorithm: "PS512", Verified: true},
				Documents:            []MetadataDocument{{Name: "set", Digest: digestD, EnvelopeDigest: digestA}, {Name: "catalog", Digest: digestA}},
				GeneratedAt:          externalGenerated,
				FetchedAt:            externalGenerated.Add(time.Minute),
				Sequence:             8,
				Rollback:             RollbackEvidence{Policy: MetadataRollbackPolicyV1, SequenceFloor: 7, StateDigest: digestD},
			},
			{
				Tap:         "homebrew/core",
				Commit:      strings.Repeat("1", 40),
				Signer:      Signature{KeyID: "homebrew-1", Algorithm: "PS512", Verified: true},
				Documents:   []MetadataDocument{{Name: "migrations", Digest: digestD, EnvelopeDigest: digestA}, {Name: "formula", Digest: digestA, EnvelopeDigest: digestD}},
				GeneratedAt: coreGenerated,
				FetchedAt:   coreGenerated.Add(time.Minute),
				Sequence:    uint64(coreGenerated.Unix()),
				Rollback:    RollbackEvidence{Policy: CoreMetadataRollbackPolicyV1, SequenceFloor: uint64(coreGenerated.Add(-time.Hour).Unix()), StateDigest: digestA},
			},
		},
		ResolvedAt:      resolvedAt,
		SourceDateEpoch: coreGenerated.Unix(),
		Requested:       []RequestedRootV2{{Requested: "acme/tools/widget", ID: "acme/tools/widget"}},
		Nodes: []NodeV2{
			{
				ID:               "acme/tools/widget",
				Tap:              "acme/tools",
				Name:             "widget",
				HomebrewFullName: "acme/tools/widget",
				FormulaVersion:   "2.0",
				PkgVersion:       "2.0",
				License:          "Apache-2.0",
				Dependencies:     []RequirementV2{{ID: "homebrew/core/hello", Minimum: "1.0", Direct: true}},
				Bottle: BottleV2{
					Tag:                        "x86_64_linux",
					Filename:                   "widget--2.0.x86_64_linux.bottle.tar.gz",
					Size:                       2,
					SHA256:                     "sha256:" + shaC,
					Cellar:                     "/home/linuxbrew/.linuxbrew/Cellar",
					CurrentFormulaSourceDigest: digestA,
					BottleFormulaSourceDigest:  digestD,
					BottleSourceWaiver:         HTTPSBottleSourceWaiverPolicyV1,
					Verification:               BottleVerificationV2{PolicyVersion: BottleVerificationPolicyV1, InventoryDigest: digestA, EntryCount: 1, ExpandedSize: 2},
					Tab:                        BottleTabV2{Arch: "x86_64", Dependencies: []RuntimeDependencyV2{{ID: "homebrew/core/hello", HomebrewFullName: "homebrew/core/hello", Version: "1.0", PkgVersion: "1.0", DeclaredDirectly: true}}},
					SelectedAnnotations:        []KV{{Key: "org.example.source", Value: "acme/tools/widget"}, {Key: "org.example.commit", Value: strings.Repeat("2", 40)}},
					Transport: BottleTransport{HTTPS: &HTTPSTransport{
						URL:                  "https://bottles.example.test/widget-2.0.tar.gz",
						ExpectedSize:         2,
						SHA256:               "sha256:" + shaC,
						Filename:             "widget--2.0.x86_64_linux.bottle.tar.gz",
						AllowedRedirectHosts: []string{"cdn.example.test", "bottles.example.test"},
						FetchPolicyVersion:   HTTPSFetchPolicyVersionV1,
					}},
				},
				Provenance: Provenance{Verified: &VerifiedProvenance{
					PolicyVersion:   VerifiedProvenancePolicyV1,
					SubjectDigest:   "sha256:" + shaC,
					StatementDigest: digestD,
					BundleDigest:    digestA,
					SignerIdentity:  "https://github.com/acme/tools/.github/workflows/release.yml@refs/heads/main",
					Issuer:          "https://token.actions.githubusercontent.com",
				}},
				ExecutablePaths: []string{"bin/widget", "bin/widget-helper"},
			},
			{
				ID:               "homebrew/core/hello",
				Tap:              "homebrew/core",
				Name:             "hello",
				HomebrewFullName: "homebrew/core/hello",
				FormulaVersion:   "1.0",
				PkgVersion:       "1.0",
				License:          "GPL-3.0-or-later",
				Bottle: BottleV2{
					Tag:                        "x86_64_linux",
					Filename:                   "hello--1.0.x86_64_linux.bottle.tar.gz",
					Size:                       1,
					SHA256:                     "sha256:" + shaB,
					Cellar:                     "/home/linuxbrew/.linuxbrew/Cellar",
					CurrentFormulaSourceDigest: digestD,
					BottleFormulaSourceDigest:  digestA,
					BottleSourceRepository:     "https://github.com/homebrew/homebrew-core",
					BottleSourceCommit:         strings.Repeat("1", 40),
					BottleFormulaPath:          "Formula/h/hello.rb",
					Verification:               BottleVerificationV2{PolicyVersion: BottleVerificationPolicyV1, InventoryDigest: digestD, EntryCount: 1, ExpandedSize: 1},
					Tab:                        BottleTabV2{Arch: "x86_64"},
					Transport: BottleTransport{OCI: &OCITransport{
						Registry:   "ghcr.io",
						Repository: "homebrew/core/hello",
						Index:      descriptor(digestA),
						Manifest:   manifest,
						Config:     descriptor(digestD),
						Layer:      descriptor("sha256:" + shaB),
					}},
				},
				Provenance: Provenance{Waiver: &ProvenanceWaiver{Policy: ProvenanceWaiverPolicyV1}},
			},
		},
		InstallOrder: []FormulaID{"homebrew/core/hello", "acme/tools/widget"},
		Components: ComponentsV2{
			FrontendIndexRef:                  "ghcr.io/example/frontend@" + digestD,
			FrontendRef:                       "ghcr.io/example/frontend@" + digestA,
			RuntimeBaseRef:                    "ghcr.io/example/runtime-base@" + digestD,
			MaterializerRef:                   "ghcr.io/example/materializer@" + digestA,
			BottleFetcherRef:                  "ghcr.io/example/bottle-fetcher@" + digestD,
			CatalogServiceOrigin:              "https://catalog.example.test",
			IngestionJWSKeyPolicyDigest:       digestA,
			TapPolicyDigest:                   digestD,
			ExecutableRuntimePolicyDigest:     digestA,
			HomebrewCommit:                    strings.Repeat("1", 40),
			RubyRuntime:                       "4.0.6",
			VerificationKeys:                  digestD,
			DalecModule:                       "github.com/project-dalec/dalec@v0.21.5",
			BuildKitModule:                    "github.com/moby/buildkit@v0.31.2",
			SupportedCatalogPolicyVersions:    []string{"tap-catalog-v2", "tap-catalog-v1"},
			SupportedFetchPolicyVersions:      []string{HTTPSFetchPolicyVersionV1},
			SupportedProvenancePolicyVersions: []string{HTTPSBottleSourceWaiverPolicyV1, ProvenanceWaiverPolicyV1, VerifiedProvenancePolicyV1},
		},
		Runtime: RuntimePolicy{
			User:          "linuxbrew",
			UID:           1000,
			GID:           1000,
			WritablePaths: []string{"/home/linuxbrew/.linuxbrew/var/widget", "/home/linuxbrew/.linuxbrew/var/hello"},
			CPUBaseline:   "core2",
		},
		PruningPolicyDigest: digestA,
	}
}

func TestCanonicalV2StableAcrossSetOrdering(t *testing.T) {
	a := validRecordV2()
	b := cloneRecordV2(*a)
	slices.Reverse(b.MetadataSources)
	for i := range b.MetadataSources {
		slices.Reverse(b.MetadataSources[i].Documents)
	}
	slices.Reverse(b.Components.SupportedCatalogPolicyVersions)
	slices.Reverse(b.Components.SupportedFetchPolicyVersions)
	slices.Reverse(b.Components.SupportedProvenancePolicyVersions)
	slices.Reverse(b.Nodes)
	slices.Reverse(b.Runtime.WritablePaths)
	for i := range b.Nodes {
		slices.Reverse(b.Nodes[i].Dependencies)
		slices.Reverse(b.Nodes[i].ExecutablePaths)
		slices.Reverse(b.Nodes[i].Bottle.SelectedAnnotations)
		slices.Reverse(b.Nodes[i].Bottle.Tab.Dependencies)
		if b.Nodes[i].Bottle.Transport.HTTPS != nil {
			slices.Reverse(b.Nodes[i].Bottle.Transport.HTTPS.AllowedRedirectHosts)
		}
	}

	canonicalA, err := CanonicalV2(a)
	if err != nil {
		t.Fatal(err)
	}
	canonicalB, err := CanonicalV2(&b)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonicalA) != string(canonicalB) {
		t.Fatalf("canonical V2 bytes depend on set ordering:\nA=%s\nB=%s", canonicalA, canonicalB)
	}
	digestA, err := DigestV2(a)
	if err != nil {
		t.Fatal(err)
	}
	digestB, err := DigestV2(&b)
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatalf("canonical V2 digests differ: %s != %s", digestA, digestB)
	}
	const wantDigest = "sha256:65706c7853a63dc3d8deb2d3cf518b1c7a33096876835649fbf04cf07e2435db"
	if digestA.String() != wantDigest {
		t.Fatalf("canonical V2 digest = %s, want stable %s", digestA, wantDigest)
	}
	version, err := SchemaVersionOf(canonicalA)
	if err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersionV2 {
		t.Fatalf("schema version = %q, want %q", version, SchemaVersionV2)
	}
	if _, err := Decode(canonicalA); err == nil {
		t.Fatal("V1 decoder accepted a V2 record")
	}

	decoded, err := DecodeV2(canonicalA)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.MetadataSources[0].Tap != "acme/tools" || decoded.Nodes[0].ID != "acme/tools/widget" {
		t.Fatalf("decoded V2 record was not canonicalized: sources=%v nodes=%v", decoded.MetadataSources, decoded.Nodes)
	}
}

func TestV1CanonicalAndDecodeCompatibility(t *testing.T) {
	record := validRecord()
	canonical, err := Canonical(record)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Digest(record)
	if err != nil {
		t.Fatal(err)
	}
	const wantDigest = "sha256:679f338ef9499ca773d72fd37b88843aa89707bcb534ad3f27093697635c202b"
	if d.String() != wantDigest {
		t.Fatalf("canonical V1 digest = %s, want pre-V2 %s", d, wantDigest)
	}
	version, err := SchemaVersionOf(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersionV1 {
		t.Fatalf("schema version = %q, want %q", version, SchemaVersionV1)
	}
	decoded, err := Decode(canonical)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := Canonical(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(roundTrip) != string(canonical) {
		t.Fatalf("V1 canonical bytes changed after V2 addition:\n%s\n%s", canonical, roundTrip)
	}
}

func TestValidateV2RequiresCompleteReleaseBindings(t *testing.T) {
	tests := map[string]struct {
		mutate func(*RecordV2)
		want   string
	}{
		"frontend": {
			mutate: func(record *RecordV2) { record.Components.FrontendRef = "" },
			want:   "V2 frontend component",
		},
		"bottle fetcher": {
			mutate: func(record *RecordV2) { record.Components.BottleFetcherRef = "" },
			want:   "V2 bottle fetcher component",
		},
		"catalog service": {
			mutate: func(record *RecordV2) { record.Components.CatalogServiceOrigin += "/v1" },
			want:   "V2 catalog service origin",
		},
		"ingestion key policy": {
			mutate: func(record *RecordV2) { record.Components.IngestionJWSKeyPolicyDigest = "" },
			want:   "V2 ingestion JWS key policy",
		},
		"tap policy": {
			mutate: func(record *RecordV2) { record.Components.TapPolicyDigest = "" },
			want:   "V2 tap policy",
		},
		"runtime policy": {
			mutate: func(record *RecordV2) { record.Components.ExecutableRuntimePolicyDigest = "" },
			want:   "V2 executable runtime policy",
		},
		"catalog policy versions": {
			mutate: func(record *RecordV2) { record.Components.SupportedCatalogPolicyVersions = nil },
			want:   "supported catalog policy versions are required",
		},
		"fetch policy versions": {
			mutate: func(record *RecordV2) { record.Components.SupportedFetchPolicyVersions = nil },
			want:   "supported fetch policy versions are required",
		},
		"provenance policy versions": {
			mutate: func(record *RecordV2) { record.Components.SupportedProvenancePolicyVersions = nil },
			want:   "supported provenance policy versions are required",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			record := validRecordV2()
			test.mutate(record)
			if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateV2() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateV2RequiresExactAuthenticatedMetadataDocuments(t *testing.T) {
	tests := map[string]struct {
		mutate func(*RecordV2)
		want   string
	}{
		"core formula": {
			mutate: func(record *RecordV2) {
				removeMetadataDocumentV2(metadataSourceV2(record, "homebrew/core"), "formula")
			},
			want: `missing required authenticated document "formula"`,
		},
		"core migrations": {
			mutate: func(record *RecordV2) {
				removeMetadataDocumentV2(metadataSourceV2(record, "homebrew/core"), "migrations")
			},
			want: `missing required authenticated document "migrations"`,
		},
		"core extra document": {
			mutate: func(record *RecordV2) {
				source := metadataSourceV2(record, "homebrew/core")
				source.Documents = append(source.Documents, MetadataDocument{Name: "extra", Digest: source.Documents[0].Digest})
			},
			want: `unexpected authenticated document "extra"`,
		},
		"core formula envelope": {
			mutate: func(record *RecordV2) {
				metadataDocumentV2(metadataSourceV2(record, "homebrew/core"), "formula").EnvelopeDigest = ""
			},
			want: `document "formula" requires an authenticated envelope digest`,
		},
		"core migrations envelope": {
			mutate: func(record *RecordV2) {
				metadataDocumentV2(metadataSourceV2(record, "homebrew/core"), "migrations").EnvelopeDigest = ""
			},
			want: `document "migrations" requires an authenticated envelope digest`,
		},
		"tap catalog": {
			mutate: func(record *RecordV2) {
				removeMetadataDocumentV2(metadataSourceV2(record, "acme/tools"), "catalog")
			},
			want: `missing required authenticated document "catalog"`,
		},
		"tap catalog set": {
			mutate: func(record *RecordV2) {
				removeMetadataDocumentV2(metadataSourceV2(record, "acme/tools"), "set")
			},
			want: `missing required authenticated document "set"`,
		},
		"tap extra document": {
			mutate: func(record *RecordV2) {
				source := metadataSourceV2(record, "acme/tools")
				source.Documents = append(source.Documents, MetadataDocument{Name: "extra", Digest: source.Documents[0].Digest})
			},
			want: `unexpected authenticated document "extra"`,
		},
		"tap catalog envelope": {
			mutate: func(record *RecordV2) {
				document := metadataDocumentV2(metadataSourceV2(record, "acme/tools"), "catalog")
				document.EnvelopeDigest = document.Digest
			},
			want: `document "catalog" must not carry a separate envelope digest`,
		},
		"tap catalog set envelope": {
			mutate: func(record *RecordV2) {
				metadataDocumentV2(metadataSourceV2(record, "acme/tools"), "set").EnvelopeDigest = ""
			},
			want: `document "set" requires an authenticated envelope digest`,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			record := validRecordV2()
			test.mutate(record)
			if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateV2() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestValidateV2BindsSelectedPoliciesAndHomebrewCommit(t *testing.T) {
	tests := map[string]struct {
		mutate func(*RecordV2)
		want   string
	}{
		"catalog": {
			mutate: func(record *RecordV2) { metadataSourceV2(record, "acme/tools").CatalogPolicyVersion = "tap-catalog-v3" },
			want:   "catalog policy",
		},
		"fetch": {
			mutate: func(record *RecordV2) { record.Components.SupportedFetchPolicyVersions = []string{"other-fetch-v1"} },
			want:   "fetch policy",
		},
		"provenance": {
			mutate: func(record *RecordV2) {
				record.Components.SupportedProvenancePolicyVersions = []string{ProvenanceWaiverPolicyV1}
			},
			want: "verified provenance policy",
		},
		"Homebrew commit": {
			mutate: func(record *RecordV2) { record.Components.HomebrewCommit = strings.Repeat("3", 40) },
			want:   "does not match component Homebrew commit",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			record := validRecordV2()
			test.mutate(record)
			if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateV2() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateV2RejectsInvalidTransportUnion(t *testing.T) {
	tests := map[string]func(*RecordV2){
		"neither": func(record *RecordV2) {
			nodeV2(record, "acme/tools/widget").Bottle.Transport = BottleTransport{}
		},
		"both": func(record *RecordV2) {
			node := nodeV2(record, "acme/tools/widget")
			node.Bottle.Transport.OCI = cloneRecordV2(*validRecordV2()).Nodes[1].Bottle.Transport.OCI
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record := validRecordV2()
			mutate(record)
			err := ValidateV2(record)
			if err == nil || !strings.Contains(err.Error(), "transport must set exactly one") {
				t.Fatalf("ValidateV2() error = %v, want strict transport union error", err)
			}
		})
	}
}

func TestValidateV2RejectsInvalidProvenanceUnion(t *testing.T) {
	tests := map[string]func(*RecordV2){
		"neither": func(record *RecordV2) {
			nodeV2(record, "homebrew/core/hello").Provenance = Provenance{}
		},
		"both": func(record *RecordV2) {
			node := nodeV2(record, "homebrew/core/hello")
			node.Provenance.Verified = &VerifiedProvenance{
				PolicyVersion:   VerifiedProvenancePolicyV1,
				SubjectDigest:   node.Bottle.Transport.OCI.Layer.Digest,
				StatementDigest: "sha256:" + strings.Repeat("d", 64),
				BundleDigest:    "sha256:" + strings.Repeat("a", 64),
				SignerIdentity:  "builder@example.test",
				Issuer:          "https://issuer.example.test",
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record := validRecordV2()
			mutate(record)
			err := ValidateV2(record)
			if err == nil || !strings.Contains(err.Error(), "provenance must set exactly one") {
				t.Fatalf("ValidateV2() error = %v, want strict provenance union error", err)
			}
		})
	}
}

func TestValidateV2BindsVerifiedProvenanceToBottleDigest(t *testing.T) {
	record := validRecordV2()
	nodeV2(record, "acme/tools/widget").Provenance.Verified.SubjectDigest = "sha256:" + strings.Repeat("a", 64)
	if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), "does not match bottle digest") {
		t.Fatalf("ValidateV2() error = %v, want provenance subject binding error", err)
	}
}

func TestDecodeV2RejectsDuplicateJSONMembers(t *testing.T) {
	canonical, err := CanonicalV2(validRecordV2())
	if err != nil {
		t.Fatal(err)
	}
	duplicate := strings.Replace(string(canonical), `"schema_version":"`+SchemaVersionV2+`"`, `"schema_version":"`+SchemaVersionV2+`","schema_version":"`+SchemaVersionV2+`"`, 1)
	if _, err := DecodeV2([]byte(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate JSON member") {
		t.Fatalf("DecodeV2() error = %v, want duplicate member rejection", err)
	}
}

func TestValidateV2SourceDateEpochUsesEarliestAuthenticatedSource(t *testing.T) {
	record := validRecordV2()
	if err := ValidateV2(record); err != nil {
		t.Fatal(err)
	}
	record.SourceDateEpoch++
	if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), "earliest authenticated generation time") {
		t.Fatalf("ValidateV2() error = %v, want source epoch mismatch", err)
	}

	record = validRecordV2()
	external := metadataSourceV2(record, "acme/tools")
	external.GeneratedAt = metadataSourceV2(record, "homebrew/core").GeneratedAt.Add(-time.Hour)
	external.FetchedAt = external.GeneratedAt.Add(time.Minute)
	record.SourceDateEpoch = external.GeneratedAt.Unix()
	if err := ValidateV2(record); err != nil {
		t.Fatalf("earliest external source was rejected: %v", err)
	}
}

func TestValidateV2RejectsDuplicateRackNames(t *testing.T) {
	record := validRecordV2()
	node := nodeV2(record, "acme/tools/widget")
	node.ID = "acme/tools/hello"
	node.Name = "hello"
	node.HomebrewFullName = "acme/tools/hello"
	record.Requested[0].ID = node.ID
	record.Requested[0].Requested = node.ID.String()
	record.InstallOrder[1] = node.ID
	if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), "rack name collision") {
		t.Fatalf("ValidateV2() error = %v, want rack collision", err)
	}
}

func TestValidateV2RequiresCanonicalFormulaIDsEverywhere(t *testing.T) {
	for name, mutate := range map[string]func(*RecordV2){
		"root":          func(record *RecordV2) { record.Requested[0].ID = "widget" },
		"install order": func(record *RecordV2) { record.InstallOrder[1] = "widget" },
		"requirement":   func(record *RecordV2) { nodeV2(record, "acme/tools/widget").Dependencies[0].ID = "hello" },
	} {
		t.Run(name, func(t *testing.T) {
			record := validRecordV2()
			mutate(record)
			if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), "is not canonical") {
				t.Fatalf("ValidateV2() error = %v, want non-canonical Formula ID rejection", err)
			}
		})
	}
}

func nodeV2(record *RecordV2, id FormulaID) *NodeV2 {
	for i := range record.Nodes {
		if record.Nodes[i].ID == id {
			return &record.Nodes[i]
		}
	}
	panic("missing V2 node " + id.String())
}

func metadataSourceV2(record *RecordV2, tap TapID) *MetadataSource {
	for i := range record.MetadataSources {
		if record.MetadataSources[i].Tap == tap {
			return &record.MetadataSources[i]
		}
	}
	panic("missing V2 metadata source " + tap.String())
}

func metadataDocumentV2(source *MetadataSource, name string) *MetadataDocument {
	for i := range source.Documents {
		if source.Documents[i].Name == name {
			return &source.Documents[i]
		}
	}
	panic("missing V2 metadata document " + name)
}

func removeMetadataDocumentV2(source *MetadataSource, name string) {
	for i := range source.Documents {
		if source.Documents[i].Name == name {
			source.Documents = append(source.Documents[:i], source.Documents[i+1:]...)
			return
		}
	}
	panic("missing V2 metadata document " + name)
}

func TestValidateV2RestrictsOCITransportToCanonicalGHCRRepository(t *testing.T) {
	for name, mutate := range map[string]func(*OCITransport){
		"registry":   func(transport *OCITransport) { transport.Registry = "registry.example.com" },
		"repository": func(transport *OCITransport) { transport.Repository = "other/tap/hello" },
	} {
		t.Run(name, func(t *testing.T) {
			record := validRecordV2()
			mutate(nodeV2(record, "homebrew/core/hello").Bottle.Transport.OCI)
			if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), "canonical ghcr.io") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestValidateV2AllowsDeferredStaticEvidenceOnlyForCore(t *testing.T) {
	record := validRecordV2()
	core := nodeV2(record, "homebrew/core/hello")
	core.Bottle.BottleFormulaSourceDigest = ""
	core.Bottle.BottleSourceRepository = ""
	core.Bottle.BottleSourceCommit = ""
	core.Bottle.BottleFormulaPath = ""
	core.Bottle.Verification = BottleVerificationV2{PolicyVersion: CoreBottleVerificationDeferredV1}
	if err := ValidateV2(record); err != nil {
		t.Fatal(err)
	}
	nonCore := nodeV2(record, "acme/tools/widget")
	nonCore.Bottle.BottleFormulaSourceDigest = ""
	nonCore.Bottle.BottleSourceRepository = ""
	nonCore.Bottle.BottleSourceCommit = ""
	nonCore.Bottle.BottleFormulaPath = ""
	nonCore.Bottle.Verification = BottleVerificationV2{PolicyVersion: CoreBottleVerificationDeferredV1}
	if err := ValidateV2(record); err == nil {
		t.Fatal("non-core deferred verification accepted")
	}
}

func TestDecodeV2RejectsCaseFoldedFieldAliases(t *testing.T) {
	canonical, err := CanonicalV2(validRecordV2())
	if err != nil {
		t.Fatal(err)
	}
	aliased := strings.Replace(string(canonical), `"runtime_base_ref":`, `"RUNTIME_BASE_REF":`, 1)
	if _, err := DecodeV2([]byte(aliased)); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("err=%v", err)
	}
}

func addValidPrebuiltDerivationV2(record *RecordV2) *NodeV2 {
	node := nodeV2(record, "acme/tools/widget")
	digestA := record.Components.TapPolicyDigest
	digestB := "sha256:" + strings.Repeat("b", 64)
	digestE := "sha256:" + strings.Repeat("e", 64)
	digestF := "sha256:" + strings.Repeat("f", 64)
	digest9 := "sha256:" + strings.Repeat("9", 64)
	node.Bottle.Tab = BottleTabV2{Receiptless: true, Arch: "x86_64"}
	node.Bottle.BottleFormulaSourceDigest = node.Bottle.CurrentFormulaSourceDigest
	node.Bottle.BottleSourceRepository = ""
	node.Bottle.BottleSourceCommit = ""
	node.Bottle.BottleFormulaPath = ""
	node.Bottle.BottleSourceWaiver = ""
	node.Bottle.Transport.HTTPS = &HTTPSTransport{
		URL:                  record.Components.CatalogServiceOrigin + "/v1/artifacts/sha256/" + strings.TrimPrefix(node.Bottle.SHA256, "sha256:"),
		ExpectedSize:         node.Bottle.Size,
		SHA256:               node.Bottle.SHA256,
		Filename:             node.Bottle.Filename,
		AllowedRedirectHosts: []string{"catalog.example.test"},
		FetchPolicyVersion:   HTTPSFetchPolicyVersionV1,
	}
	node.Provenance = Provenance{Waiver: &ProvenanceWaiver{Policy: PrebuiltProvenanceWaiverPolicyV1}}
	record.Components.SupportedProvenancePolicyVersions = append(record.Components.SupportedProvenancePolicyVersions, PrebuiltProvenanceWaiverPolicyV1)
	node.Bottle.PrebuiltDerivation = &PrebuiltDerivationV2{
		PolicyVersion: PrebuiltDerivedBottlePolicyV1,
		PolicyDigest:  digestA,
		Source: PrebuiltSourceArtifactV2{
			Filename: "widget_2.0_linux_amd64.tar.gz",
			Size:     3,
			SHA256:   digestB,
			Format:   "tar+gzip",
			Transport: BottleTransport{HTTPS: &HTTPSTransport{
				URL:                  "https://github.com/acme/widget/releases/download/v2.0/widget_2.0_linux_amd64.tar.gz",
				ExpectedSize:         3,
				SHA256:               digestB,
				Filename:             "widget_2.0_linux_amd64.tar.gz",
				AllowedRedirectHosts: []string{"release-assets.githubusercontent.com", "github.com"},
				FetchPolicyVersion:   HTTPSFetchPolicyVersionV1,
			}},
		},
		SourceInventory: PrebuiltSourceInventoryV2{InventoryDigest: digestE, EntryCount: 3, ExpandedSize: 10},
		Payload:         PrebuiltPayloadEvidenceV2{SourcePath: "widget", DestinationPath: "bin/widget", SHA256: digestF, Size: 5, ArchiveMode: 0o755, DerivedMode: 0o555},
		ELF:             PrebuiltELFEvidenceV2{Format: "elf64", Machine: "x86_64", StaticallyLinked: true, NeededLibraries: []string{}, RPaths: []string{}},
		FormulaSource: PrebuiltFormulaSourceEvidenceV2{
			Transport: TapFormulaSourceTransportV2{Tap: TapSourceV2{ID: "acme/tools", Repository: "https://github.com/acme/homebrew-tools", Commit: strings.Repeat("2", 40), TreeDigest: digest9, ArchiveDigest: digestE}, Path: "Formula/widget.rb"},
			SHA256:    node.Bottle.CurrentFormulaSourceDigest,
			Size:      128,
		},
		RecipeDigest: digest9,
		DerivedBottle: PrebuiltDerivedBottleRelationV2{
			Tag: node.Bottle.Tag, Filename: node.Bottle.Filename, SHA256: node.Bottle.SHA256, Size: node.Bottle.Size,
			Verification: node.Bottle.Verification, FormulaSourceDigest: node.Bottle.CurrentFormulaSourceDigest,
		},
	}
	return node
}

func TestValidateV2PrebuiltDerivation(t *testing.T) {
	record := validRecordV2()
	addValidPrebuiltDerivationV2(record)
	canonical, err := CanonicalV2(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeV2(canonical); err != nil {
		t.Fatal(err)
	}
}

func TestValidateV2RejectsPrebuiltDerivationMixAndMatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RecordV2, *NodeV2)
		want   string
	}{
		{name: "derived bottle", mutate: func(_ *RecordV2, node *NodeV2) {
			node.Bottle.PrebuiltDerivation.DerivedBottle.SHA256 = "sha256:" + strings.Repeat("1", 64)
		}, want: "derived-bottle relation"},
		{name: "wrong platform", mutate: func(_ *RecordV2, node *NodeV2) { node.Bottle.PrebuiltDerivation.ELF.Machine = "aarch64" }, want: "static x86_64"},
		{name: "wrong source tap", mutate: func(_ *RecordV2, node *NodeV2) {
			node.Bottle.PrebuiltDerivation.FormulaSource.Transport.Tap.ID = "other/tools"
		}, want: "does not match node tap"},
		{name: "wrong source digest", mutate: func(_ *RecordV2, node *NodeV2) {
			node.Bottle.PrebuiltDerivation.FormulaSource.SHA256 = "sha256:" + strings.Repeat("1", 64)
		}, want: "does not bind current"},
		{name: "wrong policy", mutate: func(_ *RecordV2, node *NodeV2) {
			node.Bottle.PrebuiltDerivation.PolicyDigest = "sha256:" + strings.Repeat("1", 64)
		}, want: "does not match release tap policy"},
		{name: "wrong service", mutate: func(_ *RecordV2, node *NodeV2) {
			node.Bottle.Transport.HTTPS.URL = "https://other.example.com/v1/artifacts/sha256/" + strings.TrimPrefix(node.Bottle.SHA256, "sha256:")
		}, want: "does not match catalog service"},
		{name: "native waiver", mutate: func(_ *RecordV2, node *NodeV2) { node.Provenance.Waiver.Policy = ProvenanceWaiverPolicyV1 }, want: "prebuilt provenance waiver"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := validRecordV2()
			node := addValidPrebuiltDerivationV2(record)
			test.mutate(record, node)
			if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateV2() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCanonicalV2SortsPrebuiltDerivationSets(t *testing.T) {
	a := validRecordV2()
	addValidPrebuiltDerivationV2(a)
	b := cloneRecordV2(*a)
	slices.Reverse(b.Nodes[0].Bottle.PrebuiltDerivation.Source.Transport.HTTPS.AllowedRedirectHosts)
	left, err := CanonicalV2(a)
	if err != nil {
		t.Fatal(err)
	}
	right, err := CanonicalV2(&b)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) {
		t.Fatal("prebuilt derivation canonical bytes depend on redirect-host order")
	}
}

func TestValidateV2BuildLocalCatalogEvidence(t *testing.T) {
	record := validRecordV2()
	record.Components.CatalogExtractorRef = "ghcr.io/example/catalog-extractor@sha256:" + strings.Repeat("e", 64)
	record.Components.CatalogServiceOrigin = ""
	record.Components.IngestionJWSKeyPolicyDigest = ""
	source := metadataSourceV2(record, "acme/tools")
	source.Signer = Signature{}
	source.Documents = []MetadataDocument{{Name: "catalog", Digest: "sha256:" + strings.Repeat("a", 64)}}
	source.Extraction = &TapExtractionV2{
		PolicyVersion: BuildLocalExtractionPolicyV1,
		ExtractorRef:  record.Components.CatalogExtractorRef,
		Repository:    "https://github.com/acme/homebrew-tools",
		TreeDigest:    "sha256:" + strings.Repeat("b", 64),
		ArchiveDigest: "sha256:" + strings.Repeat("c", 64),
		CatalogDigest: source.Documents[0].Digest,
	}
	source.Sequence = 1
	source.Rollback = RollbackEvidence{Policy: BuildLocalRollbackPolicyV1, StateDigest: source.Documents[0].Digest}
	if err := ValidateV2(record); err != nil {
		t.Fatal(err)
	}
	source.Extraction.ExtractorRef = "ghcr.io/example/other@sha256:" + strings.Repeat("f", 64)
	if err := ValidateV2(record); err == nil || !strings.Contains(err.Error(), "extractor_ref does not match") {
		t.Fatalf("tampered extraction error = %v", err)
	}
}

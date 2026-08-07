package catalog

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestValidFoundationDocuments(t *testing.T) {
	t.Parallel()
	if err := ValidateRequest(validRequest()); err != nil {
		t.Fatalf("request: %v", err)
	}
	if err := ValidateTapCatalog(validTapCatalog()); err != nil {
		t.Fatalf("tap catalog: %v", err)
	}
	payload, request, _ := validPayload(t)
	if err := ValidateCatalogSetPayload(payload); err != nil {
		t.Fatalf("catalog set: %v", err)
	}
	if err := ValidateCatalogSetBinding(payload, request); err != nil {
		t.Fatalf("binding: %v", err)
	}
	if err := ValidateCatalogSetAt(payload, payload.GeneratedAt.Add(time.Hour)); err != nil {
		t.Fatalf("freshness: %v", err)
	}
	if err := ValidatePlatformResult(validPlatformResult()); err != nil {
		t.Fatalf("platform result: %v", err)
	}
}

func TestRequestEnforcesTapLimit(t *testing.T) {
	t.Parallel()
	request := validRequest()
	request.Targets[0].ExternalRoots = nil
	for i := 0; i < MaxTaps+1; i++ {
		request.Targets[0].ExternalRoots = append(request.Targets[0].ExternalRoots, FormulaID(fmt.Sprintf("owner%d/tap/formula", i)))
	}
	if err := ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "limit is 16") {
		t.Fatalf("err = %v", err)
	}
}

func TestClosureEnforcesNodeLimit(t *testing.T) {
	t.Parallel()
	closure := ClosureResult{}
	for i := 0; i < MaxClosureNodes+1; i++ {
		id := FormulaID(fmt.Sprintf("acme/tools/formula%d", i))
		closure.Requested = append(closure.Requested, id)
		closure.Nodes = append(closure.Nodes, Node{
			ID:               id,
			Tap:              "acme/tools",
			Name:             id.Name(),
			HomebrewFullName: string(id),
			FormulaVersion:   "1",
			PkgVersion:       "1",
		})
		closure.InstallOrder = append(closure.InstallOrder, id)
	}
	if err := ValidateClosureResult(closure); err == nil || !strings.Contains(err.Error(), "limit is 256") {
		t.Fatalf("err = %v", err)
	}
}

func TestCatalogReferenceAndAggregateLimits(t *testing.T) {
	t.Parallel()
	payload, _, _ := validPayload(t)
	reference := payload.Catalogs[0]
	reference.Size = MaxCatalogDocumentBytes + 1
	if err := ValidateCatalogReference(reference); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("per-document err = %v", err)
	}

	payload.Catalogs = nil
	for i := 0; i < 5; i++ {
		tap := TapID(fmt.Sprintf("owner%d/tap%d", i, i))
		d := testDigest(byte('a' + i))
		payload.Catalogs = append(payload.Catalogs, CatalogReference{
			Tap: TapSource{
				ID:            tap,
				Repository:    tap.DefaultGitHubRepository(),
				Commit:        strings.Repeat(string(byte('a'+i)), 40),
				TreeDigest:    d,
				ArchiveDigest: d,
			},
			PublishedAt: testTime(),
			Sequence:    uint64(i + 1),
			URL:         "https://catalog.example.com" + CatalogDocumentPathPrefix + strings.TrimPrefix(d, "sha256:"),
			Size:        MaxCatalogDocumentBytes,
			SHA256:      d,
		})
	}
	if err := ValidateCatalogSetPayload(payload); err == nil || !strings.Contains(err.Error(), "aggregate catalog size") {
		t.Fatalf("aggregate err = %v", err)
	}
}

func TestTapCatalogRejectsInvalidRelationships(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*TapCatalog)
		want   string
	}{
		{
			name: "bare dependency escapes owning and core taps",
			mutate: func(catalog *TapCatalog) {
				for i := range catalog.Formulae {
					if catalog.Formulae[i].ID == "acme/tools/widget" {
						catalog.Formulae[i].Dependencies = []Dependency{{Raw: "foreign", ID: "other/utils/foreign"}}
					}
				}
			},
			want: "unrelated tap",
		},
		{
			name: "bare dependency spelling must match normalized name",
			mutate: func(catalog *TapCatalog) {
				for i := range catalog.Formulae {
					if catalog.Formulae[i].ID == "acme/tools/widget" {
						catalog.Formulae[i].Dependencies = []Dependency{{Raw: "helper", ID: "acme/tools/widget"}}
					}
				}
			},
			want: "does not match normalized Formula name",
		},
		{
			name: "qualified dependency spelling must match normalized ID",
			mutate: func(catalog *TapCatalog) {
				for i := range catalog.Formulae {
					if catalog.Formulae[i].ID == "acme/tools/widget" {
						catalog.Formulae[i].Dependencies = []Dependency{{Raw: "acme/tools/helper", ID: "acme/tools/widget"}}
					}
				}
			},
			want: "does not match normalized dependency",
		},
		{
			name: "migration target must be qualified",
			mutate: func(catalog *TapCatalog) {
				catalog.Migrations[0].RawTarget = "new-widget"
			},
			want: "fully qualified",
		},
		{
			name: "alias cannot cross tap",
			mutate: func(catalog *TapCatalog) {
				catalog.Aliases[0].To = "other/utils/widget"
			},
			want: "leaves tap",
		},
		{
			name: "versioned target must exist",
			mutate: func(catalog *TapCatalog) {
				for i := range catalog.Formulae {
					if catalog.Formulae[i].ID == "acme/tools/widget" {
						catalog.Formulae[i].VersionedFormulae = []FormulaID{"acme/tools/missing@1"}
					}
				}
			},
			want: "is missing",
		},
		{
			name: "duplicate normalized dependency",
			mutate: func(catalog *TapCatalog) {
				for i := range catalog.Formulae {
					if catalog.Formulae[i].ID == "acme/tools/widget" {
						catalog.Formulae[i].Dependencies = append(catalog.Formulae[i].Dependencies, Dependency{Raw: "homebrew/core/zlib", ID: "homebrew/core/zlib"})
					}
				}
			},
			want: "duplicate normalized dependency",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			catalog := validTapCatalog()
			test.mutate(catalog)
			if err := ValidateTapCatalog(catalog); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestClosureRejectsRackCollisionAndCycle(t *testing.T) {
	t.Parallel()
	collision := validClosure()
	other := Node{
		ID:               "other/utils/zlib",
		Tap:              "other/utils",
		Name:             "zlib",
		HomebrewFullName: "other/utils/zlib",
		FormulaVersion:   "9",
		PkgVersion:       "9",
	}
	collision.Requested = append(collision.Requested, other.ID)
	collision.Nodes = append(collision.Nodes, other)
	collision.InstallOrder = append(collision.InstallOrder, other.ID)
	if err := ValidateClosureResult(collision); err == nil || !strings.Contains(err.Error(), "share rack name") {
		t.Fatalf("rack collision err = %v", err)
	}

	cycle := validClosure()
	for i := range cycle.Nodes {
		if cycle.Nodes[i].ID == "homebrew/core/zlib" {
			cycle.Nodes[i].Dependencies = []Requirement{{Raw: "acme/tools/widget", ID: "acme/tools/widget"}}
		}
	}
	if err := ValidateClosureResult(cycle); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle err = %v", err)
	}
}

func TestCompareClosureAndPlatformResults(t *testing.T) {
	t.Parallel()
	signedClosure := validClosure()
	recomputedClosure := cloneForTest(t, signedClosure)
	slices.Reverse(recomputedClosure.Nodes)
	if err := CompareClosure(signedClosure, recomputedClosure); err != nil {
		t.Fatal(err)
	}
	recomputedClosure.Nodes[0].FormulaVersion = "different"
	if err := CompareClosure(signedClosure, recomputedClosure); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("closure mismatch err = %v", err)
	}

	signedResult := validPlatformResult()
	recomputedResult := cloneForTest(t, signedResult)
	slices.Reverse(recomputedResult.Closure.Nodes)
	slices.Reverse(recomputedResult.Artifacts)
	for i := range recomputedResult.Artifacts {
		if recomputedResult.Artifacts[i].Transport.HTTPS != nil {
			slices.Reverse(recomputedResult.Artifacts[i].Transport.HTTPS.AllowedRedirectHosts)
		}
	}
	if err := ComparePlatformResult(signedResult, recomputedResult); err != nil {
		t.Fatal(err)
	}
	recomputedResult.Artifacts[0].Size++
	if err := ComparePlatformResult(signedResult, recomputedResult); err == nil {
		t.Fatal("artifact mismatch accepted")
	}
}

func TestCatalogSetBindingAndFreshness(t *testing.T) {
	t.Parallel()
	payload, request, _ := validPayload(t)
	if err := ValidateCatalogSetBinding(payload, request); err != nil {
		t.Fatal(err)
	}
	mismatchedRequest := cloneForTest(t, *request)
	mismatchedRequest.HomebrewCommit = strings.Repeat("b", 40)
	if err := ValidateCatalogSetBinding(payload, &mismatchedRequest); err == nil || !strings.Contains(err.Error(), "request_digest") {
		t.Fatalf("request binding err = %v", err)
	}
	mismatchedCore := cloneForTest(t, *payload)
	mismatchedCore.CoreSnapshotDigest = testDigest('b')
	if err := ValidateCatalogSetBinding(&mismatchedCore, request); err == nil || !strings.Contains(err.Error(), "core_snapshot_digest") {
		t.Fatalf("core binding err = %v", err)
	}
	if err := ValidateCatalogSetAt(payload, payload.GeneratedAt.Add(-time.Nanosecond)); err == nil || !strings.Contains(err.Error(), "not valid before") {
		t.Fatalf("not-before err = %v", err)
	}
	if err := ValidateCatalogSetAt(payload, payload.ExpiresAt); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expiry err = %v", err)
	}
}

func TestTransportUnionAndBindings(t *testing.T) {
	t.Parallel()
	base := validHTTPSArtifact()
	without := cloneForTest(t, base)
	without.Transport = Transport{}
	if err := ValidateBottleArtifact(without); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("empty union err = %v", err)
	}
	both := cloneForTest(t, base)
	both.Transport.OCI = validOCIArtifact().Transport.OCI
	if err := ValidateBottleArtifact(both); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("double union err = %v", err)
	}
	missingHost := cloneForTest(t, base)
	missingHost.Transport.HTTPS.AllowedRedirectHosts = []string{"objects.example.com"}
	if err := ValidateBottleArtifact(missingHost); err == nil || !strings.Contains(err.Error(), "initial URL host") {
		t.Fatalf("host binding err = %v", err)
	}
	privateIP := cloneForTest(t, base)
	privateIP.Transport.HTTPS.URL = "https://127.0.0.1/bottle.tar.gz"
	if err := ValidateBottleArtifact(privateIP); err == nil || !strings.Contains(err.Error(), "IP-literal") {
		t.Fatalf("IP literal err = %v", err)
	}
	ociMismatch := validOCIArtifact()
	ociMismatch.Transport.OCI.Layer.Digest = testDigest('e')
	if err := ValidateBottleArtifact(ociMismatch); err == nil || !strings.Contains(err.Error(), "does not match artifact digest") {
		t.Fatalf("OCI digest err = %v", err)
	}
}

func TestProvenanceUnionAndSubjectBinding(t *testing.T) {
	t.Parallel()
	base := validHTTPSArtifact()
	none := cloneForTest(t, base)
	none.Provenance = Provenance{}
	if err := ValidateBottleArtifact(none); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("empty provenance err = %v", err)
	}
	both := cloneForTest(t, base)
	both.Provenance.Verified = &VerifiedProvenance{
		PolicyVersion:   VerifiedProvenancePolicy,
		SubjectDigest:   both.SHA256,
		StatementDigest: testDigest('a'),
		BundleDigest:    testDigest('b'),
		SignerIdentity:  "https://github.com/acme/release/.github/workflows/release.yml@refs/heads/main",
		Issuer:          "https://token.actions.githubusercontent.com",
	}
	if err := ValidateBottleArtifact(both); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("double provenance err = %v", err)
	}
	verified := cloneForTest(t, base)
	verified.Provenance = Provenance{Verified: both.Provenance.Verified}
	if err := ValidateBottleArtifact(verified); err != nil {
		t.Fatal(err)
	}
	verified.Provenance.Verified.SubjectDigest = testDigest('c')
	if err := ValidateBottleArtifact(verified); err == nil || !strings.Contains(err.Error(), "does not match bottle digest") {
		t.Fatalf("subject binding err = %v", err)
	}
}

func TestOperationStrictUnionAndFailureCodes(t *testing.T) {
	t.Parallel()
	pending := &Operation{SchemaVersion: OperationSchemaVersion, ID: "op_123", Status: OperationPending, RetryAfterSeconds: 10}
	if err := ValidateOperation(pending); err != nil {
		t.Fatal(err)
	}
	headerOnlyPending := &Operation{SchemaVersion: OperationSchemaVersion, ID: "op_124", Status: OperationPending}
	if err := ValidateOperation(headerOnlyPending); err != nil {
		t.Fatalf("header-only Retry-After pending operation: %v", err)
	}
	negativeRetry := cloneForTest(t, *pending)
	negativeRetry.RetryAfterSeconds = -1
	if err := ValidateOperation(&negativeRetry); err == nil || !strings.Contains(err.Error(), "retry_after_seconds") {
		t.Fatalf("negative retry err = %v", err)
	}
	completed := &Operation{SchemaVersion: OperationSchemaVersion, ID: "op_123", Status: OperationCompleted, Result: validResult()}
	if err := ValidateOperation(completed); err != nil {
		t.Fatal(err)
	}
	failed := &Operation{SchemaVersion: OperationSchemaVersion, ID: "op_123", Status: OperationFailed, Failure: &Failure{Code: FailureMissingBottle, Message: "no Linux bottle"}}
	if err := ValidateOperation(failed); err != nil {
		t.Fatal(err)
	}
	invalid := cloneForTest(t, *pending)
	invalid.Result = validResult()
	if err := ValidateOperation(&invalid); err == nil || !strings.Contains(err.Error(), "cannot contain") {
		t.Fatalf("pending union err = %v", err)
	}

	validCodes := []FailureCode{FailureTimeout, FailureUnavailable, FailureInvalidTap, FailureMissingBottle, FailurePolicy, FailureSignature}
	for _, code := range validCodes {
		if !code.Valid() {
			t.Errorf("stable code %q is invalid", code)
		}
	}
	if FailureCode("internal").Valid() {
		t.Fatal("unstable internal code accepted")
	}
}

func TestCatalogSetResultRejectsDuplicateJWSMembers(t *testing.T) {
	t.Parallel()
	result := validResult()
	result.JWS = []byte(`{"payload":"{}","payload":"tampered","protected":"e30","signature":"sig"}`)
	if err := ValidateCatalogSetResult(result); err == nil || !strings.Contains(err.Error(), "duplicate JSON member") {
		t.Fatalf("err = %v", err)
	}
}

func TestCatalogSetRequiresExactlyReachedTapCatalogs(t *testing.T) {
	t.Parallel()
	payload, _, _ := validPayload(t)
	payload.Catalogs = nil
	if err := ValidateCatalogSetPayload(payload); err == nil || !strings.Contains(err.Error(), "no signed catalog reference") {
		t.Fatalf("missing catalog err = %v", err)
	}
}

func TestRequestAndCatalogSetBindExternalRootsPerPlatform(t *testing.T) {
	t.Parallel()
	payload, request, _ := validPayload(t)
	armTarget := PlatformRequest{Platform: Platform{OS: "linux", Architecture: "arm64"}, ExternalRoots: []FormulaID{}}
	request.Targets = append(request.Targets, armTarget)
	requestDigest, err := RequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	payload.RequestDigest = requestDigest.String()
	payload.Results = append(payload.Results, PlatformResult{
		Platform:  armTarget.Platform,
		Closure:   ClosureResult{Requested: []FormulaID{}, Nodes: []Node{}, InstallOrder: []FormulaID{}},
		Artifacts: []BottleArtifact{},
	})
	if err := ValidateCatalogSetPayload(payload); err != nil {
		t.Fatalf("payload with an empty platform-specific external closure: %v", err)
	}
	if err := ValidateCatalogSetBinding(payload, request); err != nil {
		t.Fatalf("per-platform binding: %v", err)
	}

	mismatchedRequest := cloneForTest(t, *request)
	mismatchedRequest.Targets[1].ExternalRoots = []FormulaID{"acme/tools/helper"}
	mismatchedDigest, err := RequestDigest(&mismatchedRequest)
	if err != nil {
		t.Fatal(err)
	}
	mismatchedPayload := cloneForTest(t, *payload)
	mismatchedPayload.RequestDigest = mismatchedDigest.String()
	if err := ValidateCatalogSetBinding(&mismatchedPayload, &mismatchedRequest); err == nil || !strings.Contains(err.Error(), "requested roots do not match request target") {
		t.Fatalf("per-platform root mismatch err = %v", err)
	}
}

func TestLegacyInMemoryRequestCanonicalizesToPerPlatformTargets(t *testing.T) {
	t.Parallel()
	legacy := &Request{
		SchemaVersion:      RequestSchemaVersion,
		ExternalRoots:      []FormulaID{"acme/tools/widget"},
		Platforms:          []Platform{{OS: "linux", Architecture: "arm64"}, {OS: "linux", Architecture: "amd64"}},
		HomebrewCommit:     strings.Repeat("a", 40),
		CoreSnapshotDigest: testDigest('a'),
	}
	canonical, err := CanonicalRequest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), `"platforms"`) || !strings.Contains(string(canonical), `"targets"`) {
		t.Fatalf("legacy request did not canonicalize to scoped targets: %s", canonical)
	}
	decoded, err := DecodeRequest(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Targets) != 2 || len(decoded.Targets[0].ExternalRoots) != 1 || len(decoded.Targets[1].ExternalRoots) != 1 {
		t.Fatalf("decoded targets = %+v", decoded.Targets)
	}
}

func TestRequestRejectsDuplicatePlatformsAndAllEmptyRoots(t *testing.T) {
	t.Parallel()
	request := validRequest()
	request.Targets = append(request.Targets, cloneForTest(t, request.Targets[0]))
	if err := ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "duplicate target platform") {
		t.Fatalf("duplicate platform err = %v", err)
	}
	request = validRequest()
	request.Targets[0].ExternalRoots = []FormulaID{}
	if err := ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "no external roots") {
		t.Fatalf("empty roots err = %v", err)
	}
}

func TestCatalogSetRejectsCrossPlatformRootVersionMismatch(t *testing.T) {
	t.Parallel()
	payload, _, _ := validPayload(t)
	arm := cloneForTest(t, payload.Results[0])
	arm.Platform = Platform{OS: "linux", Architecture: "arm64"}
	for i := range arm.Artifacts {
		arm.Artifacts[i].Platform = arm.Platform
		arm.Artifacts[i].Tag = "arm64_linux"
		arm.Artifacts[i].Tab.Arch = "arm64"
		if arm.Artifacts[i].Transport.OCI != nil {
			arm.Artifacts[i].Transport.OCI.Manifest.Platform = &arm.Platform
		}
	}
	for i := range arm.Artifacts {
		artifact := &arm.Artifacts[i]
		artifact.Platform = arm.Platform
		artifact.Tag = "arm64_linux"
		artifact.Filename = strings.ReplaceAll(artifact.Filename, "x86_64_linux", "arm64_linux")
		if artifact.Transport.HTTPS != nil {
			artifact.Transport.HTTPS.Filename = artifact.Filename
		}
		if artifact.Transport.OCI != nil {
			platform := arm.Platform
			artifact.Transport.OCI.Manifest.Platform = &platform
		}
	}
	payload.Results = append(payload.Results, arm)
	if err := ValidateCatalogSetPayload(payload); err != nil {
		t.Fatal(err)
	}
	for i := range payload.Results[1].Closure.Nodes {
		if payload.Results[1].Closure.Nodes[i].ID == "acme/tools/widget" {
			payload.Results[1].Closure.Nodes[i].FormulaVersion = "2.1"
		}
	}
	if err := ValidateCatalogSetPayload(payload); err == nil || !strings.Contains(err.Error(), "inconsistent versions") {
		t.Fatalf("err = %v", err)
	}
}

func TestCellarPolicyIsFailClosed(t *testing.T) {
	t.Parallel()
	artifact := validHTTPSArtifact()
	artifact.Cellar = "/tmp/Cellar"
	if err := ValidateBottleArtifact(artifact); err == nil || !strings.Contains(err.Error(), "unsupported Cellar policy") {
		t.Fatalf("artifact cellar err = %v", err)
	}
	catalog := validTapCatalog()
	catalog.Formulae[0].Bottle.Files[0].Cellar = "/usr/local/Cellar"
	if err := ValidateTapCatalog(catalog); err == nil || !strings.Contains(err.Error(), "unsupported Cellar policy") {
		t.Fatalf("catalog cellar err = %v", err)
	}
}

func TestCatalogSetRejectsReusedCatalogDocumentIdentity(t *testing.T) {
	t.Parallel()
	payload, _, _ := validPayload(t)
	duplicate := cloneForTest(t, payload.Catalogs[0])
	duplicate.Tap.ID = "other/utils"
	duplicate.Tap.Repository = duplicate.Tap.ID.DefaultGitHubRepository()
	payload.Catalogs = append(payload.Catalogs, duplicate)
	if err := ValidateCatalogSetPayload(payload); err == nil || !strings.Contains(err.Error(), "reuse digest") {
		t.Fatalf("err = %v", err)
	}
}

func TestRequestBindsCoreAndExternalRootsPerPlatform(t *testing.T) {
	request := validRequest()
	core, err := ParseFormulaID("hello")
	if err != nil {
		t.Fatal(err)
	}
	request.Targets[0].CoreRoots = []FormulaID{core}
	if _, err := CanonicalRequest(request); err != nil {
		t.Fatal(err)
	}
	nonCore, _ := ParseFormulaID("acme/tools/widget")
	request.Targets[0].CoreRoots = []FormulaID{nonCore}
	if err := ValidateRequest(request); err == nil {
		t.Fatal("non-core identity accepted in core_roots")
	}
}

func TestValidateServiceOriginMatchesCatalogReferencePolicy(t *testing.T) {
	for _, bad := range []string{"https://catalog.example.com:8443", "https://Catalog.example.com", "https://127.0.0.1", "https://localhost", "https://catalog.example.com/path"} {
		if err := ValidateServiceOrigin(bad); err == nil {
			t.Fatalf("accepted service origin %q", bad)
		}
	}
	if err := ValidateServiceOrigin("https://catalog.example.com"); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPSArtifactRequiresExplicitReceiptlessAndSourceEvidenceMarkers(t *testing.T) {
	artifact := validHTTPSArtifact()
	artifact.BottleSourceWaiver = ""
	if err := ValidateBottleArtifact(artifact); err == nil || !strings.Contains(err.Error(), "source waiver") {
		t.Fatalf("missing source waiver err=%v", err)
	}
	artifact = validHTTPSArtifact()
	artifact.Tab.Receiptless = true
	artifact.Tab.HomebrewVersion = "forged"
	if err := ValidateBottleArtifact(artifact); err == nil || !strings.Contains(err.Error(), "receiptless") {
		t.Fatalf("receiptless metadata err=%v", err)
	}
}

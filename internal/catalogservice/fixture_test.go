package catalogservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

func validStaticFixture(t *testing.T) StaticFixture {
	t.Helper()
	requestBytes, err := catalog.CanonicalRequest(testRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	request, err := catalog.DecodeRequest(requestBytes)
	if err != nil {
		t.Fatal(err)
	}
	generated := testGeneratedSet(t)
	return StaticFixture{
		SchemaVersion: StaticFixtureSchemaVersion,
		Request:       *request,
		Catalogs:      generated.Catalogs,
		Results:       generated.Results,
	}
}

func TestDecodeStaticFixtureAndGenerate(t *testing.T) {
	fixture := validStaticFixture(t)
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeStaticFixture(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	generator, err := NewStaticGenerator(&decoded.Request, &GeneratedSet{Catalogs: decoded.Catalogs, Results: decoded.Results})
	if err != nil {
		t.Fatal(err)
	}
	generated, err := generator.Generate(t.Context(), &decoded.Request)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated.Catalogs) != 1 || len(generated.Results) != 1 {
		t.Fatalf("generated=%+v", generated)
	}
	generated.Catalogs[0].Formulae[0].Name = "mutated"
	again, err := generator.Generate(t.Context(), &decoded.Request)
	if err != nil {
		t.Fatal(err)
	}
	if again.Catalogs[0].Formulae[0].Name != "widget" {
		t.Fatal("static generator result was not cloned")
	}
}

func TestDecodeStaticFixtureRejectsDuplicateAndUnknownInput(t *testing.T) {
	fixture := validStaticFixture(t)
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := bytes.Replace(data, []byte(`"schema_version":`), []byte(`"schema_version":"`+StaticFixtureSchemaVersion+`","schema_version":`), 1)
	if _, err := DecodeStaticFixture(bytes.NewReader(duplicate)); err == nil || !strings.Contains(err.Error(), "duplicate JSON member") {
		t.Fatalf("duplicate error=%v", err)
	}
	unknown := bytes.Replace(data, []byte(`{"schema_version":`), []byte(`{"unknown":true,"schema_version":`), 1)
	if _, err := DecodeStaticFixture(bytes.NewReader(unknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown error=%v", err)
	}
}

func TestStaticGeneratorRejectsUnboundRequestWithStableFailure(t *testing.T) {
	fixture := validStaticFixture(t)
	generator, err := NewStaticGenerator(&fixture.Request, &GeneratedSet{Catalogs: fixture.Catalogs, Results: fixture.Results})
	if err != nil {
		t.Fatal(err)
	}
	other := fixture.Request
	other.HomebrewCommit = strings.Repeat("e", 40)
	_, err = generator.Generate(context.Background(), &other)
	var failure *FailureError
	if !errors.As(err, &failure) || failure.Failure.Code != catalog.FailurePolicy {
		t.Fatalf("error=%T %v", err, err)
	}
}

func TestDecodeStaticFixtureRejectsCaseFoldedFields(t *testing.T) {
	fixture := validStaticFixture(t)
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	aliased := strings.Replace(string(data), `"schema_version":`, `"SCHEMA_VERSION":`, 1)
	if _, err := DecodeStaticFixture(strings.NewReader(aliased)); err == nil {
		t.Fatal("case-folded fixture field accepted")
	}
}

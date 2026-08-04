package frontend

import (
	"context"
	"errors"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogclient"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
)

type fakeCatalogClient struct {
	calls   int
	err     error
	request *catalog.Request
}

func (f *fakeCatalogClient) Resolve(_ context.Context, request *catalog.Request) (*catalogclient.Result, error) {
	f.calls++
	f.request = request
	return nil, f.err
}

type emptyCore struct{}

func (emptyCore) Lookup(name string) (metadata.Match, error) {
	return metadata.Match{}, &metadata.LookupError{Name: name, Err: metadata.ErrFormulaNotFound}
}

func TestResolveNonCoreCatalogsCoreOnlyBypassesService(t *testing.T) {
	client := &fakeCatalogClient{err: errors.New("must not be called")}
	result, err := ResolveNonCoreCatalogs(t.Context(), client, emptyCore{}, []NonCoreTarget{{Platform: catalog.Platform{OS: "linux", Architecture: "amd64"}}}, "0123456789abcdef0123456789abcdef01234567", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if result != nil || client.calls != 0 {
		t.Fatalf("result=%+v calls=%d", result, client.calls)
	}
}

func TestResolveNonCoreCatalogsRequiresClientOnlyForExternalRoots(t *testing.T) {
	root, err := catalog.ParseFormulaID("acme/tools/widget")
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveNonCoreCatalogs(t.Context(), nil, emptyCore{}, []NonCoreTarget{{Platform: catalog.Platform{OS: "linux", Architecture: "amd64"}, ExternalRoots: []catalog.FormulaID{root}}}, "0123456789abcdef0123456789abcdef01234567", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err == nil {
		t.Fatal("external roots accepted without catalog client")
	}
}

func TestResolveNonCoreCatalogsBindsCoreRootsWithExternalRequest(t *testing.T) {
	external, _ := catalog.ParseFormulaID("acme/tools/widget")
	core, _ := catalog.ParseFormulaID("hello")
	client := &fakeCatalogClient{err: errors.New("stop after capture")}
	_, _ = ResolveNonCoreCatalogs(t.Context(), client, emptyCore{}, []NonCoreTarget{{Platform: catalog.Platform{OS: "linux", Architecture: "amd64"}, ExternalRoots: []catalog.FormulaID{external}, CoreRoots: []catalog.FormulaID{core}}}, "0123456789abcdef0123456789abcdef01234567", "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if client.request == nil || len(client.request.Targets) != 1 || len(client.request.Targets[0].ExternalRoots) != 1 || len(client.request.Targets[0].CoreRoots) != 1 {
		t.Fatalf("captured request=%+v", client.request)
	}
}

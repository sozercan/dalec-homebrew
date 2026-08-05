package frontend

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
	"github.com/sozercan/dalec-homebrew/internal/catalogclient"
	"github.com/sozercan/dalec-homebrew/internal/catalogresolver"
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

func TestResolveNonCoreCatalogsRecomputesCanonicalRootOrder(t *testing.T) {
	platform := catalog.Platform{OS: "linux", Architecture: "amd64"}
	digest := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tap := catalog.TapID("acme/tools")
	formula := func(name string) catalog.Formula {
		id := catalog.FormulaID("acme/tools/" + name)
		return catalog.Formula{
			ID: id, Name: name, HomebrewFullName: string(id), SourcePath: "Formula/" + name + ".rb", SourceDigest: digest, StableVersion: "1.0",
			Bottle: &catalog.BottleDeclaration{RootURL: "https://github.com/acme/homebrew-tools/releases/download/v1.0", Files: []catalog.BottleFile{{Tag: "x86_64_linux", URL: "https://github.com/acme/homebrew-tools/releases/download/v1.0/" + name + "--1.0.x86_64_linux.bottle.tar.gz", SHA256: digest, Cellar: "/home/linuxbrew/.linuxbrew/Cellar"}}},
		}
	}
	document := &catalog.TapCatalog{
		SchemaVersion: catalog.TapCatalogSchemaVersion,
		Tap:           catalog.TapSource{ID: tap, Repository: tap.DefaultGitHubRepository(), Commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TreeDigest: digest, ArchiveDigest: digest},
		PublishedAt:   time.Unix(1, 0).UTC(), Sequence: 1, Formulae: []catalog.Formula{formula("a"), formula("b")},
	}
	catalogs := map[catalog.TapID]*catalog.TapCatalog{tap: document}
	resolver, err := catalogresolver.New(emptyCore{}, catalogs)
	if err != nil {
		t.Fatal(err)
	}
	a := catalog.FormulaID("acme/tools/a")
	b := catalog.FormulaID("acme/tools/b")
	signed, err := resolver.Resolve([]catalog.FormulaID{a, b}, platform)
	if err != nil {
		t.Fatal(err)
	}
	client := &staticCatalogClient{result: &catalogclient.Result{Payload: &catalog.CatalogSetPayload{Results: []catalog.PlatformResult{{Platform: platform, Closure: signed}}}, Catalogs: catalogs}}
	resolved, err := ResolveNonCoreCatalogs(t.Context(), client, emptyCore{}, []NonCoreTarget{{Platform: platform, ExternalRoots: []catalog.FormulaID{b, a}}}, "0123456789abcdef0123456789abcdef01234567", digest)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.ByPlatform["linux/amd64"].Closure.InstallOrder; !slices.Equal(got, []catalog.FormulaID{a, b}) {
		t.Fatalf("install order=%v, want canonical root order", got)
	}
}

type staticCatalogClient struct {
	result *catalogclient.Result
}

func (client *staticCatalogClient) Resolve(context.Context, *catalog.Request) (*catalogclient.Result, error) {
	return client.result, nil
}

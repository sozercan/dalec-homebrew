package catalogservice

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sozercan/dalec-homebrew/internal/catalog"
)

const (
	StaticFixtureSchemaVersion       = "dalec-homebrew-catalog-service-fixture/v1"
	MaxStaticFixtureBytes      int64 = catalog.MaxAggregateCatalogBytes + catalog.MaxOperationBytes
)

// StaticFixture is the strict on-disk format accepted by the test/local static
// generator. Production extraction implementations should implement Generator
// directly instead of using this format.
type StaticFixture struct {
	SchemaVersion string                   `json:"schema_version"`
	Request       catalog.Request          `json:"request"`
	Catalogs      []catalog.TapCatalog     `json:"catalogs"`
	Results       []catalog.PlatformResult `json:"results"`
}

// DecodeStaticFixture reads one bounded, duplicate-free fixture document.
func DecodeStaticFixture(reader io.Reader) (*StaticFixture, error) {
	if reader == nil {
		return nil, errors.New("static fixture reader is nil")
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxStaticFixtureBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read static fixture: %w", err)
	}
	if int64(len(data)) > MaxStaticFixtureBytes {
		return nil, fmt.Errorf("static fixture exceeds %d bytes", MaxStaticFixtureBytes)
	}
	var fixture StaticFixture
	if err := decodeStrictJSON(data, MaxStaticFixtureBytes, "static catalog fixture", &fixture); err != nil {
		return nil, err
	}
	if fixture.SchemaVersion != StaticFixtureSchemaVersion {
		return nil, fmt.Errorf("unsupported static fixture schema_version %q", fixture.SchemaVersion)
	}
	if _, err := catalog.CanonicalRequest(&fixture.Request); err != nil {
		return nil, fmt.Errorf("validate static fixture request: %w", err)
	}
	generated := &GeneratedSet{Catalogs: fixture.Catalogs, Results: fixture.Results}
	if err := validateGeneratedSet(&fixture.Request, generated); err != nil {
		return nil, fmt.Errorf("validate static fixture result: %w", err)
	}
	return &fixture, nil
}

// LoadStaticGenerator loads a strict fixture from a regular file and returns a
// generator bound to its canonical request digest.
func LoadStaticGenerator(filename string) (*StaticGenerator, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("static fixture must be a regular non-symlink file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	fixture, err := DecodeStaticFixture(file)
	if err != nil {
		return nil, err
	}
	return NewStaticGenerator(&fixture.Request, &GeneratedSet{Catalogs: fixture.Catalogs, Results: fixture.Results})
}

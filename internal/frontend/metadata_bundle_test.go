package frontend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	fstypes "github.com/tonistiigi/fsutil/types"
)

type metadataBundleTestRef struct {
	stats        map[string]*fstypes.Stat
	data         map[string][]byte
	readErrs     map[string]error
	read         []string
	readDirCalls int
}

func newMetadataBundleTestRef() *metadataBundleTestRef {
	data := map[string][]byte{
		"/" + metadata.BundleManifestFilename:   []byte("manifest\n"),
		"/" + metadata.BundleFormulaFilename:    []byte("formula\n"),
		"/" + metadata.BundleMigrationsFilename: []byte("migrations\n"),
	}
	ref := &metadataBundleTestRef{
		stats:    make(map[string]*fstypes.Stat, len(data)),
		data:     data,
		readErrs: map[string]error{},
	}
	for _, name := range []string{
		metadata.BundleManifestFilename,
		metadata.BundleFormulaFilename,
		metadata.BundleMigrationsFilename,
	} {
		path := "/" + name
		ref.stats[path] = &fstypes.Stat{Path: name, Mode: uint32(0o444), Size: int64(len(data[path]))}
	}
	return ref
}

func (r *metadataBundleTestRef) ToState() (llb.State, error)    { return llb.Scratch(), nil }
func (r *metadataBundleTestRef) Evaluate(context.Context) error { return nil }

func (r *metadataBundleTestRef) ReadDir(context.Context, gwclient.ReadDirRequest) ([]*fstypes.Stat, error) {
	r.readDirCalls++
	return nil, errors.New("metadata bundle reader must not enumerate caller-controlled directories")
}

func (r *metadataBundleTestRef) StatFile(_ context.Context, req gwclient.StatRequest) (*fstypes.Stat, error) {
	stat, ok := r.stats[req.Path]
	if !ok {
		return nil, os.ErrNotExist
	}
	if stat == nil {
		return nil, nil
	}
	return stat.Clone(), nil
}

func (r *metadataBundleTestRef) ReadFile(_ context.Context, req gwclient.ReadRequest) ([]byte, error) {
	r.read = append(r.read, req.Filename)
	if err := r.readErrs[req.Filename]; err != nil {
		return nil, err
	}
	data, ok := r.data[req.Filename]
	if !ok {
		return nil, os.ErrNotExist
	}
	if req.Range == nil || req.Range.Offset != 0 || req.Range.Length != len(data) {
		return slices.Clone(data), nil
	}
	return slices.Clone(data), nil
}

type metadataBundleTestClient struct {
	gwclient.Client
	inputs      map[string]llb.State
	inputsErr   error
	ref         gwclient.Reference
	solveErr    error
	inputsCalls int
	solveCalls  int
}

func (c *metadataBundleTestClient) Inputs(context.Context) (map[string]llb.State, error) {
	c.inputsCalls++
	return c.inputs, c.inputsErr
}

func (c *metadataBundleTestClient) Solve(context.Context, gwclient.SolveRequest) (*gwclient.Result, error) {
	c.solveCalls++
	if c.solveErr != nil {
		return nil, c.solveErr
	}
	result := gwclient.NewResult()
	result.SetRef(c.ref)
	return result, nil
}

func TestReadMetadataBundleReference(t *testing.T) {
	ref := newMetadataBundleTestRef()
	bundle, err := readMetadataBundleReference(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(bundle.manifest) != "manifest\n" || string(bundle.formula) != "formula\n" || string(bundle.migrations) != "migrations\n" {
		t.Fatalf("bundle=%q %q %q", bundle.manifest, bundle.formula, bundle.migrations)
	}
	if len(ref.read) != 3 {
		t.Fatalf("read calls=%v, want all three files", ref.read)
	}
	if ref.readDirCalls != 0 {
		t.Fatalf("read directory calls=%d, want 0", ref.readDirCalls)
	}
}

func TestReadMetadataBundleReferenceIgnoresUnreferencedFilesWithoutEnumeration(t *testing.T) {
	ref := newMetadataBundleTestRef()
	ref.stats["/unused"] = &fstypes.Stat{Path: "unused", Mode: uint32(0o444), Size: 1}
	ref.data["/unused"] = []byte("x")
	if _, err := readMetadataBundleReference(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	if ref.readDirCalls != 0 {
		t.Fatalf("read directory calls=%d, want 0", ref.readDirCalls)
	}
	if slices.Contains(ref.read, "/unused") {
		t.Fatalf("unused file was read: %v", ref.read)
	}
}

func TestReadMetadataBundleReferenceRejectsUnsafeOrUnboundedFiles(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		mutate func(*metadataBundleTestRef, string)
		want   string
	}{
		{
			name: "missing file",
			file: metadata.BundleManifestFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				delete(ref.stats, path)
			},
			want: "stat metadata bundle file",
		},
		{
			name: "symlink",
			file: metadata.BundleManifestFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.stats[path].Mode = uint32(os.ModeSymlink | 0o777)
				ref.stats[path].Linkname = metadata.BundleFormulaFilename
			},
			want: "must be a regular file",
		},
		{
			name: "regular file with link target",
			file: metadata.BundleManifestFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.stats[path].Linkname = metadata.BundleFormulaFilename
			},
			want: "must be a regular file",
		},
		{
			name: "directory",
			file: metadata.BundleFormulaFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.stats[path].Mode = uint32(os.ModeDir | 0o555)
			},
			want: "must be a regular file",
		},
		{
			name: "device",
			file: metadata.BundleMigrationsFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.stats[path].Mode = uint32(os.ModeDevice | os.ModeCharDevice | 0o444)
			},
			want: "must be a regular file",
		},
		{
			name: "oversize manifest",
			file: metadata.BundleManifestFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.stats[path].Size = metadata.DefaultMaxBundleManifestBytes + 1
			},
			want: "is outside",
		},
		{
			name: "oversize formula",
			file: metadata.BundleFormulaFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.stats[path].Size = metadata.DefaultMaxFormulaBytes + 1
			},
			want: "is outside",
		},
		{
			name: "oversize migrations",
			file: metadata.BundleMigrationsFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.stats[path].Size = metadata.DefaultMaxMigrationsBytes + 1
			},
			want: "is outside",
		},
		{
			name: "truncated read",
			file: metadata.BundleManifestFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.stats[path].Size++
			},
			want: "after stat reported",
		},
		{
			name: "read failure",
			file: metadata.BundleManifestFilename,
			mutate: func(ref *metadataBundleTestRef, path string) {
				ref.readErrs[path] = errors.New("read failed")
			},
			want: "read metadata bundle file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := newMetadataBundleTestRef()
			path := "/" + tt.file
			tt.mutate(ref, path)
			_, err := readMetadataBundleReference(t.Context(), ref)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestReadMetadataBundleInputRequiresReservedNamedInput(t *testing.T) {
	client := &metadataBundleTestClient{inputs: map[string]llb.State{"other": llb.Scratch()}}
	_, err := readMetadataBundleInput(t.Context(), client)
	if err == nil || !strings.Contains(err.Error(), metadataBundleInputName) {
		t.Fatalf("error=%v", err)
	}
	if client.solveCalls != 0 {
		t.Fatalf("solve calls=%d, want 0", client.solveCalls)
	}
}

func TestReadMetadataBundleInputSolvesReservedInput(t *testing.T) {
	ref := newMetadataBundleTestRef()
	client := &metadataBundleTestClient{
		inputs: map[string]llb.State{metadataBundleInputName: llb.Scratch()},
		ref:    ref,
	}
	if _, err := readMetadataBundleInput(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	if client.inputsCalls != 1 || client.solveCalls != 1 {
		t.Fatalf("input calls=%d solve calls=%d, want 1 each", client.inputsCalls, client.solveCalls)
	}
}

func TestBundledMetadataDigestMismatchFailsClosed(t *testing.T) {
	ref := newMetadataBundleTestRef()
	client := &metadataBundleTestClient{
		inputs: map[string]llb.State{metadataBundleInputName: llb.Scratch()},
		ref:    ref,
	}
	_, err := loadMetadataSnapshot(t.Context(), client, config.Config{
		MetadataBundleDigest: "sha256:" + strings.Repeat("f", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "does not match release-bound digest") {
		t.Fatalf("error=%v", err)
	}
}

func TestMetadataWithoutBundleDigestUsesLiveFetch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := &metadataBundleTestClient{}
	_, err := loadMetadataSnapshot(t.Context(), client, config.Config{
		FormulaURL:    server.URL + "/" + metadata.FormulaEndpoint,
		MigrationsURL: server.URL + "/" + metadata.MigrationsEndpoint,
	})
	if err == nil || !strings.Contains(err.Error(), "unexpected HTTP status") {
		t.Fatalf("error=%v", err)
	}
	if client.inputsCalls != 0 || client.solveCalls != 0 {
		t.Fatalf("ordinary metadata fetch inspected named inputs: inputs=%d solve=%d", client.inputsCalls, client.solveCalls)
	}
}

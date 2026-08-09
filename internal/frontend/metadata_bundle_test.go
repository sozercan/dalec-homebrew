package frontend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/solver/pb"
	"github.com/sozercan/dalec-homebrew/internal/config"
	"github.com/sozercan/dalec-homebrew/internal/homebrew/metadata"
	fstypes "github.com/tonistiigi/fsutil/types"
)

type metadataBundleTestRef struct {
	stats        map[string]*fstypes.Stat
	data         map[string][]byte
	readErrs     map[string]error
	read         []string
	readRequests []gwclient.ReadRequest
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
	r.readRequests = append(r.readRequests, req)
	if err := r.readErrs[req.Filename]; err != nil {
		return nil, err
	}
	data, ok := r.data[req.Filename]
	if !ok {
		return nil, os.ErrNotExist
	}
	if req.Range == nil {
		return slices.Clone(data), nil
	}
	if req.Range.Offset < 0 || req.Range.Length < 0 || req.Range.Offset > len(data) {
		return nil, errors.New("invalid test range")
	}
	end := req.Range.Offset + req.Range.Length
	if end > len(data) {
		end = len(data)
	}
	return slices.Clone(data[req.Range.Offset:end]), nil
}

type metadataBundleTestClient struct {
	gwclient.Client
	buildOpts    gwclient.BuildOpts
	inputs       map[string]llb.State
	inputsErr    error
	ref          gwclient.Reference
	solveErr     error
	inputsCalls  int
	solveCalls   int
	solveRequest []gwclient.SolveRequest
}

func (c *metadataBundleTestClient) BuildOpts() gwclient.BuildOpts { return c.buildOpts }

func (c *metadataBundleTestClient) Inputs(context.Context) (map[string]llb.State, error) {
	c.inputsCalls++
	return c.inputs, c.inputsErr
}

func (c *metadataBundleTestClient) Solve(_ context.Context, req gwclient.SolveRequest) (*gwclient.Result, error) {
	c.solveCalls++
	c.solveRequest = append(c.solveRequest, req)
	if c.solveErr != nil {
		return nil, c.solveErr
	}
	result := gwclient.NewResult()
	result.SetRef(c.ref)
	return result, nil
}

func metadataBundleTransferredAttrs(t *testing.T, requests []gwclient.SolveRequest) map[string]string {
	t.Helper()
	for _, request := range requests {
		if request.Definition == nil {
			continue
		}
		for _, raw := range request.Definition.Def {
			var op pb.Op
			if err := op.Unmarshal(raw); err != nil {
				t.Fatal(err)
			}
			source := op.GetSource()
			if source == nil || source.Attrs[pb.AttrIncludePatterns] == "" {
				continue
			}
			return source.Attrs
		}
	}
	return nil
}

func metadataBundleTransferredPatterns(t *testing.T, attrs map[string]string, attr string) []string {
	t.Helper()
	rawPatterns := attrs[attr]
	if rawPatterns == "" {
		return nil
	}
	var patterns []string
	if err := json.Unmarshal([]byte(rawPatterns), &patterns); err != nil {
		t.Fatalf("decode patterns %q: %v", rawPatterns, err)
	}
	return patterns
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

func TestReadMetadataBundleReferenceChunksLargeMembers(t *testing.T) {
	ref := newMetadataBundleTestRef()
	path := "/" + metadata.BundleFormulaFilename
	ref.data[path] = make([]byte, 2*metadataBundleReadChunkBytes+123)
	for i := range ref.data[path] {
		ref.data[path][i] = byte(i)
	}
	ref.stats[path].Size = int64(len(ref.data[path]))

	bundle, err := readMetadataBundleReference(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(bundle.formula, ref.data[path]) {
		t.Fatal("chunked formula contents differ from the input")
	}

	var formulaRanges []gwclient.FileRange
	for _, request := range ref.readRequests {
		if request.Filename != path {
			continue
		}
		if request.Range == nil {
			t.Fatal("formula read did not use a bounded range")
		}
		formulaRanges = append(formulaRanges, *request.Range)
	}
	want := []gwclient.FileRange{
		{Offset: 0, Length: metadataBundleReadChunkBytes},
		{Offset: metadataBundleReadChunkBytes, Length: metadataBundleReadChunkBytes},
		{Offset: 2 * metadataBundleReadChunkBytes, Length: 123},
	}
	if !slices.Equal(formulaRanges, want) {
		t.Fatalf("formula read ranges=%v, want %v", formulaRanges, want)
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

func TestReadMetadataBundleInputRequiresReservedLocalContext(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   string
	}{
		{name: "missing", want: "is missing"},
		{name: "frontend input", source: "input:" + metadataBundleInputName, want: "must use local source"},
		{name: "image", source: "docker-image://example.com/metadata@sha256:" + strings.Repeat("a", 64), want: "must use local source"},
		{name: "other local", source: "local:other", want: "must use local source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &metadataBundleTestClient{buildOpts: gwclient.BuildOpts{Opts: map[string]string{}}}
			if tt.source != "" {
				client.buildOpts.Opts[metadataBundleContextOption] = tt.source
			}
			_, err := readMetadataBundleInput(t.Context(), client)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v, want containing %q", err, tt.want)
			}
			if client.inputsCalls != 0 || client.solveCalls != 0 {
				t.Fatalf("rejected context accessed BuildKit: inputs=%d solves=%d", client.inputsCalls, client.solveCalls)
			}
		})
	}
	t.Run("additional context", func(t *testing.T) {
		client := &metadataBundleTestClient{buildOpts: gwclient.BuildOpts{
			SessionID: "active-session",
			Opts: map[string]string{
				metadataBundleContextOption: metadataBundleLocalSource,
				"context:other":             "local:other",
			},
		}}
		_, err := readMetadataBundleInput(t.Context(), client)
		if err == nil || !strings.Contains(err.Error(), "unsupported named contexts: other") {
			t.Fatalf("error=%v", err)
		}
		if client.inputsCalls != 0 || client.solveCalls != 0 {
			t.Fatalf("rejected context accessed BuildKit: inputs=%d solves=%d", client.inputsCalls, client.solveCalls)
		}
	})
	t.Run("cross-session override", func(t *testing.T) {
		client := &metadataBundleTestClient{buildOpts: gwclient.BuildOpts{
			SessionID: "active-session",
			Opts: map[string]string{
				metadataBundleContextOption: metadataBundleLocalSource,
				metadataBundleSessionOption: "other-session",
			},
		}}
		_, err := readMetadataBundleInput(t.Context(), client)
		if err == nil || !strings.Contains(err.Error(), "does not match the active BuildKit session") {
			t.Fatalf("error=%v", err)
		}
		if client.inputsCalls != 0 || client.solveCalls != 0 {
			t.Fatalf("rejected context accessed BuildKit: inputs=%d solves=%d", client.inputsCalls, client.solveCalls)
		}
	})
}

func TestReadMetadataBundleInputLoadsBoundedReservedLocalContext(t *testing.T) {
	ref := newMetadataBundleTestRef()
	client := &metadataBundleTestClient{
		buildOpts: gwclient.BuildOpts{
			SessionID: "forwarded-session",
			Opts: map[string]string{
				metadataBundleContextOption:                     metadataBundleLocalSource,
				"sharedkey:localdir:" + metadataBundleInputName: "shared",
			},
		},
		ref: ref,
	}
	if _, err := readMetadataBundleInput(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	if client.inputsCalls != 0 || client.solveCalls != 1 {
		t.Fatalf("input calls=%d solve calls=%d, want 0 and 1", client.inputsCalls, client.solveCalls)
	}
	wantPatterns := []string{
		metadata.BundleManifestFilename,
		metadata.BundleFormulaFilename,
		metadata.BundleMigrationsFilename,
	}
	attrs := metadataBundleTransferredAttrs(t, client.solveRequest)
	if got := metadataBundleTransferredPatterns(t, attrs, pb.AttrIncludePatterns); !slices.Equal(got, wantPatterns) {
		t.Fatalf("transferred patterns=%v, want %v", got, wantPatterns)
	}
	wantExcludes := make([]string, len(wantPatterns))
	for i, pattern := range wantPatterns {
		wantExcludes[i] = pattern + "/**"
	}
	if got := metadataBundleTransferredPatterns(t, attrs, pb.AttrExcludePatterns); !slices.Equal(got, wantExcludes) {
		t.Fatalf("excluded patterns=%v, want %v", got, wantExcludes)
	}
	if got := attrs[pb.AttrLocalSessionID]; got != "forwarded-session" {
		t.Fatalf("local session=%q, want forwarded-session", got)
	}
}

func TestBundledMetadataDigestMismatchFailsClosed(t *testing.T) {
	ref := newMetadataBundleTestRef()
	client := &metadataBundleTestClient{
		buildOpts: gwclient.BuildOpts{
			SessionID: "test-session",
			Opts: map[string]string{
				metadataBundleContextOption: metadataBundleLocalSource,
			},
		},
		ref: ref,
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

func TestMetadataWithoutBundleDigestRejectsNamedContextBeforeFetch(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := &metadataBundleTestClient{buildOpts: gwclient.BuildOpts{Opts: map[string]string{
		metadataBundleContextOption: metadataBundleLocalSource,
	}}}
	_, err := loadMetadataSnapshot(t.Context(), client, config.Config{
		FormulaURL:    server.URL + "/" + metadata.FormulaEndpoint,
		MigrationsURL: server.URL + "/" + metadata.MigrationsEndpoint,
	})
	if err == nil || !strings.Contains(err.Error(), "named contexts require a release-bound metadata bundle") {
		t.Fatalf("error=%v", err)
	}
	if requests != 0 {
		t.Fatalf("metadata requests=%d, want 0", requests)
	}
}

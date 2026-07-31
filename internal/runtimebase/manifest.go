package runtimebase

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Package describes one Ubuntu package recorded by a Chisel manifest.
type Package struct {
	Name         string
	Version      string
	Architecture string
	SHA256       string
	RegularBytes int64
}

type manifestRecord struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Arch        string   `json:"arch"`
	SHA256      string   `json:"sha256"`
	FinalSHA256 string   `json:"final_sha256"`
	Slice       string   `json:"slice"`
	Slices      []string `json:"slices"`
	Path        string   `json:"path"`
	Size        int64    `json:"size"`
	Inode       *uint64  `json:"inode"`
}

// ReadChiselManifest validates a Chisel jsonwall manifest and attributes the
// selected regular-file payload to the package owning each selected slice.
func ReadChiselManifest(manifestPath, root string) ([]Package, error) {
	f, err := os.Open(manifestPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("open chisel manifest: %w", err)
	}
	defer zr.Close()
	return readChiselManifest(zr, root)
}

func readChiselManifest(r io.Reader, root string) ([]Package, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	packages := make(map[string]*Package)
	var paths []manifestRecord
	seenHeader := false
	for line := 1; scanner.Scan(); line++ {
		if !seenHeader {
			var header struct {
				JSONWall string `json:"jsonwall"`
				Schema   string `json:"schema"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &header); err != nil {
				return nil, fmt.Errorf("decode chisel manifest header: %w", err)
			}
			if header.JSONWall == "" || header.Schema == "" {
				return nil, errors.New("invalid chisel manifest header")
			}
			seenHeader = true
			continue
		}
		var record manifestRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("decode chisel manifest line %d: %w", line, err)
		}
		switch record.Kind {
		case "package":
			if record.Name == "" || record.Version == "" || record.Arch == "" {
				return nil, fmt.Errorf("incomplete package record on manifest line %d", line)
			}
			if len(record.SHA256) != 64 {
				return nil, fmt.Errorf("invalid package digest for %q", record.Name)
			}
			if _, err := hex.DecodeString(record.SHA256); err != nil {
				return nil, fmt.Errorf("invalid package digest for %q: %w", record.Name, err)
			}
			if _, ok := packages[record.Name]; ok {
				return nil, fmt.Errorf("duplicate package record %q", record.Name)
			}
			packages[record.Name] = &Package{Name: record.Name, Version: record.Version, Architecture: record.Arch, SHA256: record.SHA256}
		case "content", "slice":
		case "path":
			if record.Path == "" {
				return nil, fmt.Errorf("incomplete path record on manifest line %d", line)
			}
			paths = append(paths, record)
		default:
			return nil, fmt.Errorf("unknown chisel manifest record kind %q", record.Kind)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read chisel manifest: %w", err)
	}
	if !seenHeader || len(packages) == 0 {
		return nil, errors.New("chisel manifest contains no packages")
	}

	names := make([]string, 0, len(packages))
	for name := range packages {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	if len(paths) == 0 {
		return nil, errors.New("chisel manifest contains no path records")
	}
	type inodeCommitment struct {
		size   int64
		digest string
		info   os.FileInfo
	}
	seenInodes := map[string]inodeCommitment{}
	for _, record := range paths {
		// The manifest commits its own path before its compressed bytes exist;
		// its zero-sized self-record cannot carry a non-circular digest/size.
		if record.Path == "/var/lib/chisel/manifest.wall" {
			continue
		}
		filename, err := rootPath(root, record.Path)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(filename)
		if err != nil {
			return nil, fmt.Errorf("stat chisel path %q: %w", record.Path, err)
		}
		if info.Mode().IsRegular() {
			if info.Size() != record.Size {
				return nil, fmt.Errorf("chisel path %q size is %d, expected %d", record.Path, info.Size(), record.Size)
			}
			wantDigest := record.FinalSHA256
			if wantDigest == "" {
				wantDigest = record.SHA256
			}
			if wantDigest != "" {
				data, err := os.ReadFile(filename)
				if err != nil {
					return nil, fmt.Errorf("read chisel path %q: %w", record.Path, err)
				}
				sum := sha256.Sum256(data)
				if got := hex.EncodeToString(sum[:]); got != wantDigest {
					return nil, fmt.Errorf("chisel path %q digest is %s, expected %s", record.Path, got, wantDigest)
				}
			}
		} else if record.Size != 0 {
			return nil, fmt.Errorf("non-regular chisel path %q has non-zero size", record.Path)
		}

		owners := map[string]*Package{}
		for _, slice := range record.Slices {
			if pkg, ok := packageForSlice(slice, names, packages); ok {
				owners[pkg.Name] = pkg
			}
		}
		if len(owners) == 0 {
			if record.Size == 0 {
				continue
			}
			return nil, fmt.Errorf("chisel path %q has no package slice", record.Path)
		}
		if len(owners) != 1 {
			return nil, fmt.Errorf("chisel path %q spans multiple packages", record.Path)
		}
		for _, pkg := range owners {
			if record.Inode != nil {
				key := fmt.Sprintf("%s\x00%d", pkg.Name, *record.Inode)
				digest := record.FinalSHA256
				if digest == "" {
					digest = record.SHA256
				}
				if previous, ok := seenInodes[key]; ok {
					if previous.size != record.Size || previous.digest != digest || !os.SameFile(previous.info, info) {
						return nil, fmt.Errorf("chisel inode group %d for package %q is inconsistent", *record.Inode, pkg.Name)
					}
					continue
				}
				seenInodes[key] = inodeCommitment{size: record.Size, digest: digest, info: info}
			}
			// Chisel copy aliases omit inode identity and occupy distinct
			// payload paths, while inode-tagged hardlink groups count once.
			pkg.RegularBytes += record.Size
		}
	}

	result := make([]Package, 0, len(packages))
	for _, pkg := range packages {
		result = append(result, *pkg)
	}
	slices.SortFunc(result, func(a, b Package) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		if c := strings.Compare(a.Architecture, b.Architecture); c != 0 {
			return c
		}
		return strings.Compare(a.Version, b.Version)
	})
	return result, nil
}

func packageForSlice(slice string, names []string, packages map[string]*Package) (*Package, bool) {
	for _, name := range names {
		if strings.HasPrefix(slice, name+"_") {
			return packages[name], true
		}
	}
	return nil, false
}

func rootPath(root, manifestPath string) (string, error) {
	cleanPath := filepath.Clean(manifestPath)
	if !strings.HasPrefix(manifestPath, "/") || (manifestPath != cleanPath && manifestPath != cleanPath+"/") {
		return "", fmt.Errorf("invalid chisel content path %q", manifestPath)
	}
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Join(cleanRoot, filepath.FromSlash(strings.TrimPrefix(manifestPath, "/")))
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("chisel content path escapes root: %q", manifestPath)
	}
	return joined, nil
}

// WritePackageEvidence writes the stable package inventory consumed by the
// materializer and image-size reporter. The last fields are selected payload
// bytes and the signed source .deb digest from Chisel's manifest.
func WritePackageEvidence(packages []Package, filename string) error {
	var out strings.Builder
	for _, pkg := range packages {
		if strings.ContainsAny(pkg.Name+pkg.Version+pkg.Architecture, "\t\r\n") {
			return fmt.Errorf("package %q contains a tab or newline", pkg.Name)
		}
		fmt.Fprintf(&out, "%s\t%s\t%s\t%d\tsha256:%s\n", pkg.Name, pkg.Version, pkg.Architecture, pkg.RegularBytes, pkg.SHA256)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filename, []byte(out.String()), 0o444)
}

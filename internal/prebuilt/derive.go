package prebuilt

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const canonicalCompression = "gzip-best-compression-v1"

// Derive verifies one exact prebuilt archive against profile, statically
// inspects its selected payload, and returns a deterministic receiptless
// Homebrew bottle plus complete evidence.
func Derive(reader io.Reader, formulaSource []byte, profile Profile) (*Result, error) {
	normalized, err := normalizeProfile(profile)
	if err != nil {
		return nil, err
	}
	formulaEvidence, err := verifyFormulaSource(formulaSource, normalized)
	if err != nil {
		return nil, err
	}
	source, err := verifySource(reader, normalized)
	if err != nil {
		return nil, err
	}
	elfEvidence, goEvidence, err := inspectPayload(source.payload, normalized)
	if err != nil {
		return nil, err
	}
	bottle, derivationEvidence, err := deriveBottle(source.payload, formulaSource, normalized)
	if err != nil {
		return nil, err
	}
	profileBytes, err := json.Marshal(normalized)
	if err != nil {
		return nil, verificationError(CodeInvalidProfile, "", "canonicalize normalized profile: %v", err)
	}
	evidence := Evidence{
		SchemaVersion: EvidenceSchemaVersion,
		PolicyVersion: normalized.PolicyVersion,
		ProfileSHA256: digestBytes(profileBytes),
		Source:        source.evidence,
		Formula:       formulaEvidence,
		ELF:           elfEvidence,
		GoBuild:       goEvidence,
		Derivation:    derivationEvidence,
	}
	if _, err := CanonicalEvidence(evidence); err != nil {
		return nil, verificationError(CodeDerivation, "", "canonicalize evidence: %v", err)
	}
	return &Result{Bottle: bottle, Evidence: evidence}, nil
}

func verifyFormulaSource(formulaSource []byte, profile Profile) (FormulaEvidence, error) {
	if len(formulaSource) == 0 {
		return FormulaEvidence{}, verificationError(CodeFormulaMismatch, "", "Formula source is empty")
	}
	if int64(len(formulaSource)) > profile.Limits.MaxFormulaBytes {
		return FormulaEvidence{}, verificationError(CodeFormulaMismatch, "", "Formula source exceeds %d bytes", profile.Limits.MaxFormulaBytes)
	}
	if !utf8.Valid(formulaSource) || bytes.IndexByte(formulaSource, 0) >= 0 {
		return FormulaEvidence{}, verificationError(CodeFormulaMismatch, "", "Formula source must be valid UTF-8 without NUL bytes")
	}
	digest := digestBytes(formulaSource)
	if digest != profile.FormulaSHA256 {
		return FormulaEvidence{}, verificationError(CodeFormulaMismatch, "", "Formula digest %s does not match expected digest %s", digest, profile.FormulaSHA256)
	}
	return FormulaEvidence{SHA256: digest, Size: int64(len(formulaSource))}, nil
}

type derivedFile struct {
	path string
	mode uint32
	data []byte
}

func deriveBottle(payload, formulaSource []byte, profile Profile) ([]byte, DerivationEvidence, error) {
	kegPrefix := profile.Name + "/" + profile.PkgVersion
	formulaPath := kegPrefix + "/.brew/" + profile.Name + ".rb"
	executablePath := kegPrefix + "/bin/" + profile.Name
	files := []derivedFile{
		{path: formulaPath, mode: 0o444, data: formulaSource},
		{path: executablePath, mode: 0o555, data: payload},
	}
	if strings.Compare(files[0].path, files[1].path) > 0 {
		files[0], files[1] = files[1], files[0]
	}
	epoch := time.Unix(profile.SourceDateEpoch, 0).UTC()

	var tarBuffer bytes.Buffer
	tarWriter := tar.NewWriter(&tarBuffer)
	inventory := make([]InventoryEntry, 0, len(files))
	for _, file := range files {
		header := &tar.Header{
			Name:       file.path,
			Mode:       int64(file.mode),
			Size:       int64(len(file.data)),
			ModTime:    epoch,
			Typeflag:   tar.TypeReg,
			Uid:        0,
			Gid:        0,
			Uname:      "",
			Gname:      "",
			AccessTime: time.Time{},
			ChangeTime: time.Time{},
			Format:     tar.FormatUSTAR,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			_ = tarWriter.Close()
			return nil, DerivationEvidence{}, verificationError(CodeDerivation, file.path, "write USTAR header: %v", err)
		}
		if _, err := tarWriter.Write(file.data); err != nil {
			_ = tarWriter.Close()
			return nil, DerivationEvidence{}, verificationError(CodeDerivation, file.path, "write USTAR content: %v", err)
		}
		inventory = append(inventory, InventoryEntry{
			Path:   file.path,
			Mode:   file.mode,
			Size:   int64(len(file.data)),
			SHA256: digestBytes(file.data),
		})
	}
	if err := tarWriter.Close(); err != nil {
		return nil, DerivationEvidence{}, verificationError(CodeDerivation, "", "close USTAR archive: %v", err)
	}

	var compressed bytes.Buffer
	gzipWriter, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return nil, DerivationEvidence{}, verificationError(CodeDerivation, "", "create gzip writer: %v", err)
	}
	gzipWriter.Header.ModTime = epoch
	gzipWriter.Header.OS = 255
	gzipWriter.Header.Name = ""
	gzipWriter.Header.Comment = ""
	gzipWriter.Header.Extra = nil
	if _, err := gzipWriter.Write(tarBuffer.Bytes()); err != nil {
		_ = gzipWriter.Close()
		return nil, DerivationEvidence{}, verificationError(CodeDerivation, "", "compress USTAR archive: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, DerivationEvidence{}, verificationError(CodeDerivation, "", "close gzip writer: %v", err)
	}
	if int64(compressed.Len()) > profile.Limits.MaxBottleBytes {
		return nil, DerivationEvidence{}, verificationError(CodeDerivation, "", "derived bottle size %d exceeds %d bytes", compressed.Len(), profile.Limits.MaxBottleBytes)
	}
	inventoryDigest, err := digestInventory(inventory)
	if err != nil {
		return nil, DerivationEvidence{}, verificationError(CodeDerivation, "", "canonicalize derived inventory: %v", err)
	}
	bottle := append([]byte(nil), compressed.Bytes()...)
	return bottle, DerivationEvidence{
		PolicyVersion:   DerivationPolicyVersion,
		Receiptless:     true,
		KegPrefix:       kegPrefix,
		FormulaPath:     formulaPath,
		ExecutablePath:  executablePath,
		SourceDateEpoch: profile.SourceDateEpoch,
		Compression:     canonicalCompression,
		SHA256:          digestBytes(bottle),
		Size:            int64(len(bottle)),
		InventorySHA256: inventoryDigest,
		Inventory:       inventory,
	}, nil
}

package materializer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

const (
	installReceiptFilename       = "INSTALL_RECEIPT.json"
	receiptNormalizationTempName = ".INSTALL_RECEIPT.json.dalec-normalize.tmp"
	receiptNormalizationReason   = "homebrew_generated_runtime_dependencies_incomplete"
)

// ReceiptNormalizationEvidence records the only permitted post-pour receipt
// transformation. The digests bind the exact Homebrew-generated input and the
// deterministic, policy-derived replacement.
type ReceiptNormalizationEvidence struct {
	Formula                      string `json:"formula"`
	ReceiptPath                  string `json:"receipt_path"`
	Reason                       string `json:"reason"`
	BeforeSHA256                 string `json:"before_sha256"`
	AfterSHA256                  string `json:"after_sha256"`
	BeforeRuntimeDependencyCount int    `json:"before_runtime_dependency_count"`
	AfterRuntimeDependencyCount  int    `json:"after_runtime_dependency_count"`
}

func normalizeInstalledReceipt(prefix string, node resolution.Node, closure []resolution.Node, sourceDateEpoch int64) (_ *ReceiptNormalizationEvidence, retErr error) {
	if sourceDateEpoch <= 0 {
		return nil, fmt.Errorf("invalid receipt normalization epoch %d", sourceDateEpoch)
	}
	if err := validateReceiptPathComponent("formula", node.Name); err != nil {
		return nil, err
	}
	if err := validateReceiptPathComponent("pkg_version", node.PkgVersion); err != nil {
		return nil, err
	}

	directory, err := openReceiptKegDirectoryNoFollow(prefix, node.Name, node.PkgVersion)
	if err != nil {
		return nil, fmt.Errorf("open receipt directory for %q: %w", node.Name, err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()

	receipt, err := openBottleNoFollow(directory, installReceiptFilename)
	if err != nil {
		return nil, fmt.Errorf("open generated receipt for %q: %w", node.Name, err)
	}
	defer func() {
		if err := receipt.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	beforeInfo, beforeData, err := readBoundedReceipt(receipt, node.Name)
	if err != nil {
		return nil, err
	}
	beforeInode, beforeLinks := snapshotInodeMeta(beforeInfo)
	if beforeInode == "" || beforeLinks != 1 {
		return nil, fmt.Errorf("generated receipt for %q is not a single-link regular file", node.Name)
	}
	if beforeInfo.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || beforeInfo.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("generated receipt for %q has unsafe mode %v", node.Name, beforeInfo.Mode())
	}

	normalized, err := bottle.NormalizeInstalledReceiptDependencies(beforeData, node, closure)
	if err != nil {
		return nil, fmt.Errorf("normalize installed receipt for %q: %w", node.Name, err)
	}
	if !normalized.Changed {
		return nil, nil
	}
	limit := bottle.DefaultLimits().MaxReceiptBytes
	if len(normalized.Data) == 0 || int64(len(normalized.Data)) > limit {
		return nil, fmt.Errorf("normalized receipt for %q has invalid size %d", node.Name, len(normalized.Data))
	}

	beforeDigest := sha256Digest(beforeData)
	afterDigest := sha256Digest(normalized.Data)
	if beforeDigest == afterDigest {
		return nil, fmt.Errorf("receipt normalization for %q did not change the receipt digest", node.Name)
	}

	temporary, err := createReceiptTemporaryNoFollow(directory, receiptNormalizationTempName, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create root-controlled receipt replacement for %q: %w", node.Name, err)
	}
	temporaryPresent := true
	defer func() {
		if temporary != nil {
			if err := temporary.Close(); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
		if temporaryPresent {
			if err := removeReceiptTemporary(directory, receiptNormalizationTempName); err != nil && !errors.Is(err, os.ErrNotExist) {
				retErr = errors.Join(retErr, err)
			}
		}
	}()

	written, err := io.Copy(temporary, bytes.NewReader(normalized.Data))
	if err != nil {
		return nil, fmt.Errorf("write normalized receipt for %q: %w", node.Name, err)
	}
	if written != int64(len(normalized.Data)) {
		return nil, fmt.Errorf("write normalized receipt for %q: wrote %d bytes, expected %d", node.Name, written, len(normalized.Data))
	}
	if err := temporary.Chown(os.Geteuid(), os.Getegid()); err != nil {
		return nil, fmt.Errorf("own normalized receipt for %q: %w", node.Name, err)
	}
	if err := temporary.Chmod(beforeInfo.Mode().Perm()); err != nil {
		return nil, fmt.Errorf("set normalized receipt mode for %q: %w", node.Name, err)
	}
	epoch := time.Unix(sourceDateEpoch, 0).UTC()
	if err := setReceiptFileTimes(temporary, epoch); err != nil {
		return nil, fmt.Errorf("set normalized receipt timestamp for %q: %w", node.Name, err)
	}
	if err := temporary.Sync(); err != nil {
		return nil, fmt.Errorf("sync normalized receipt for %q: %w", node.Name, err)
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	temporaryInfo, temporaryData, err := readBoundedReceipt(temporary, node.Name)
	if err != nil {
		return nil, fmt.Errorf("verify root-controlled receipt replacement for %q: %w", node.Name, err)
	}
	temporaryUID, temporaryGID, ownershipKnown := snapshotOwnership(temporaryInfo)
	_, temporaryLinks := snapshotInodeMeta(temporaryInfo)
	if !ownershipKnown || int(temporaryUID) != os.Geteuid() || int(temporaryGID) != os.Getegid() || temporaryLinks != 1 {
		return nil, fmt.Errorf("root-controlled receipt replacement for %q has unexpected ownership or links", node.Name)
	}
	if temporaryInfo.Mode().Perm() != beforeInfo.Mode().Perm() || temporaryInfo.ModTime().Unix() != sourceDateEpoch || sha256Digest(temporaryData) != afterDigest {
		return nil, fmt.Errorf("root-controlled receipt replacement for %q did not round-trip deterministically", node.Name)
	}
	if _, err := bottle.VerifyInstalledReceipt(temporaryData, node, closure); err != nil {
		return nil, fmt.Errorf("verify root-controlled receipt replacement for %q before commit: %w", node.Name, err)
	}

	current, err := openBottleNoFollow(directory, installReceiptFilename)
	if err != nil {
		return nil, fmt.Errorf("reopen generated receipt for %q before replacement: %w", node.Name, err)
	}
	currentInfo, currentData, currentErr := readBoundedReceipt(current, node.Name)
	closeErr := current.Close()
	if currentErr != nil {
		return nil, currentErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	currentInode, currentLinks := snapshotInodeMeta(currentInfo)
	if currentInode != beforeInode || currentLinks != 1 || sha256Digest(currentData) != beforeDigest {
		return nil, fmt.Errorf("generated receipt for %q changed before atomic replacement", node.Name)
	}

	if err := temporary.Close(); err != nil {
		temporary = nil
		return nil, fmt.Errorf("close normalized receipt for %q: %w", node.Name, err)
	}
	temporary = nil
	if err := replaceReceiptAtomic(directory, receiptNormalizationTempName, installReceiptFilename); err != nil {
		return nil, fmt.Errorf("atomically replace receipt for %q: %w", node.Name, err)
	}
	temporaryPresent = false
	if err := syncReceiptDirectory(directory); err != nil {
		return nil, fmt.Errorf("sync receipt directory for %q: %w", node.Name, err)
	}

	committed, err := openBottleNoFollow(directory, installReceiptFilename)
	if err != nil {
		return nil, fmt.Errorf("reopen normalized receipt for %q: %w", node.Name, err)
	}
	committedInfo, committedData, committedErr := readBoundedReceipt(committed, node.Name)
	committedCloseErr := committed.Close()
	if committedErr != nil {
		return nil, committedErr
	}
	if committedCloseErr != nil {
		return nil, committedCloseErr
	}
	committedUID, committedGID, committedOwnershipKnown := snapshotOwnership(committedInfo)
	_, committedLinks := snapshotInodeMeta(committedInfo)
	if !committedOwnershipKnown || int(committedUID) != os.Geteuid() || int(committedGID) != os.Getegid() || committedLinks != 1 {
		return nil, fmt.Errorf("normalized receipt for %q is not root-controlled", node.Name)
	}
	if committedInfo.Mode().Perm() != beforeInfo.Mode().Perm() || committedInfo.ModTime().Unix() != sourceDateEpoch || sha256Digest(committedData) != afterDigest {
		return nil, fmt.Errorf("normalized receipt for %q changed during atomic commit", node.Name)
	}
	if _, err := bottle.VerifyInstalledReceipt(committedData, node, closure); err != nil {
		return nil, fmt.Errorf("immediately verify normalized receipt for %q: %w", node.Name, err)
	}

	return &ReceiptNormalizationEvidence{
		Formula:                      node.Name,
		ReceiptPath:                  filepath.ToSlash(filepath.Join("Cellar", node.Name, node.PkgVersion, installReceiptFilename)),
		Reason:                       receiptNormalizationReason,
		BeforeSHA256:                 beforeDigest,
		AfterSHA256:                  afterDigest,
		BeforeRuntimeDependencyCount: normalized.BeforeDependencyCount,
		AfterRuntimeDependencyCount:  normalized.AfterDependencyCount,
	}, nil
}

func readBoundedReceipt(file *os.File, formula string) (os.FileInfo, []byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	limit := bottle.DefaultLimits().MaxReceiptBytes
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > limit {
		return nil, nil, fmt.Errorf("receipt for %q is not a bounded regular file", formula)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) != info.Size() || int64(len(data)) > limit {
		return nil, nil, fmt.Errorf("receipt for %q changed size while being read", formula)
	}
	return info, data, nil
}

func validateReceiptPathComponent(label, value string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\\`) {
		return fmt.Errorf("invalid receipt %s path component %q", label, value)
	}
	return nil
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

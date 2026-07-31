package materializer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sozercan/dalec-homebrew/internal/bottle"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

const (
	coreTapRoot                 = "Homebrew/Library/Taps/homebrew/homebrew-core"
	coreTapFormulaRoot          = coreTapRoot + "/Formula"
	formulaTapStagingName       = ".dalec-formula-staging"
	protectedHomebrewBrew       = "Homebrew/bin/brew"
	protectedHomebrewBrewReal   = "Homebrew/bin/brew.real"
	protectedHomebrewLogicalDir = ".dalec-homebrew"
	protectedHomebrewLogical    = protectedHomebrewLogicalDir + "/brew"
	publishedFormulaDirName     = "Formula"
)

// StagedFormulaEvidence binds one transient on-disk core-tap Formula file to
// the exact source statically verified inside a digest-pinned bottle.
type StagedFormulaEvidence struct {
	Formula    string `json:"formula"`
	BottlePath string `json:"bottle_path"`
	TapPath    string `json:"tap_path"`
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
}

type formulaTapStagePoint string

const (
	formulaTapAfterParentWritable formulaTapStagePoint = "after-parent-writable"
	formulaTapAfterPrivateRoot    formulaTapStagePoint = "after-private-root"
	formulaTapAfterFormulaFile    formulaTapStagePoint = "after-formula-file"
	formulaTapAfterTreeSealed     formulaTapStagePoint = "after-tree-sealed"
	formulaTapBeforePublish       formulaTapStagePoint = "before-publish"
	formulaTapAfterPublish        formulaTapStagePoint = "after-publish"
	formulaTapAfterParentSealed   formulaTapStagePoint = "after-parent-sealed"
)

type formulaTapStageOptions struct {
	ownerUID, ownerGID     int
	runtimeUID, runtimeGID int
	checkpoint             func(formulaTapStagePoint) error
}

func (o formulaTapStageOptions) check(point formulaTapStagePoint) error {
	if o.checkpoint == nil {
		return nil
	}
	if err := o.checkpoint(point); err != nil {
		return fmt.Errorf("Formula staging checkpoint %s: %w", point, err)
	}
	return nil
}

type stagedFormulaInput struct {
	node     resolution.Node
	result   bottle.Result
	source   []byte
	shard    string
	filename string
	tapPath  string
}

func stageVerifiedFormulaClosure(prefix string, record *resolution.Record, verified map[string]bottle.Result, sources map[string][]byte, options formulaTapStageOptions) (evidence []StagedFormulaEvidence, retErr error) {
	inputs, err := verifiedFormulaInputs(record, verified, sources)
	if err != nil {
		return nil, err
	}
	if err := validateProtectedHomebrewRepository(prefix, options, false); err != nil {
		return nil, err
	}

	tapRoot := filepath.Join(prefix, filepath.FromSlash(coreTapRoot))
	parent, err := openFormulaTapDirectoryNoFollow(tapRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	originalParent, err := parent.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat core tap root: %w", err)
	}
	if err := requireFormulaTapEntryAbsent(parent, publishedFormulaDirName); err != nil {
		return nil, err
	}
	if err := requireFormulaTapEntryAbsent(parent, formulaTapStagingName); err != nil {
		return nil, err
	}

	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	currentTreeName := ""
	committed := false
	parentTouched := false
	defer func() {
		if committed {
			return
		}
		evidence = nil
		if !parentTouched {
			return
		}
		if err := rollbackFormulaTapTransaction(parent, currentTreeName, options, originalParent); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("rollback Formula staging transaction: %w", err))
		}
	}()

	parentTouched = true
	if err := makeFormulaTapDirectoryOwnerWritable(parent, options.ownerUID, options.ownerGID); err != nil {
		return nil, fmt.Errorf("make core tap root owner-writable: %w", err)
	}
	if err := options.check(formulaTapAfterParentWritable); err != nil {
		return nil, err
	}

	stagingRoot, err := createFormulaTapDirectoryExclusive(parent, formulaTapStagingName, options.ownerUID, options.ownerGID)
	if err != nil {
		return nil, fmt.Errorf("create private Formula staging root: %w", err)
	}
	currentTreeName = formulaTapStagingName
	defer func() { _ = stagingRoot.Close() }()
	if err := options.check(formulaTapAfterPrivateRoot); err != nil {
		return nil, err
	}

	shards := make(map[string]*os.File)
	defer func() {
		for _, shard := range shards {
			_ = shard.Close()
		}
	}()
	for _, input := range inputs {
		shard := shards[input.shard]
		if shard == nil {
			shard, err = createFormulaTapDirectoryExclusive(stagingRoot, input.shard, options.ownerUID, options.ownerGID)
			if err != nil {
				return nil, fmt.Errorf("create core tap Formula shard %q: %w", input.shard, err)
			}
			shards[input.shard] = shard
		}
		if err := createFormulaTapFileExclusive(shard, input.filename, input.source, options.ownerUID, options.ownerGID, epoch); err != nil {
			return nil, fmt.Errorf("stage Formula %q: %w", input.node.Name, err)
		}
		evidence = append(evidence, StagedFormulaEvidence{
			Formula: input.node.Name, BottlePath: input.result.Formula.Path,
			TapPath: input.tapPath, SHA256: input.result.Formula.SHA256, Size: input.result.Formula.Size,
		})
		if err := options.check(formulaTapAfterFormulaFile); err != nil {
			return nil, err
		}
	}

	shardNames := make([]string, 0, len(shards))
	for name := range shards {
		shardNames = append(shardNames, name)
	}
	slices.Sort(shardNames)
	for _, name := range shardNames {
		if err := sealFormulaTapFile(shards[name], options.ownerUID, options.ownerGID, 0o555, epoch); err != nil {
			return nil, fmt.Errorf("seal core tap Formula shard %q: %w", name, err)
		}
	}
	if err := sealFormulaTapFile(stagingRoot, options.ownerUID, options.ownerGID, 0o555, epoch); err != nil {
		return nil, fmt.Errorf("seal private Formula staging root: %w", err)
	}
	if err := verifyFormulaTree(filepath.Join(tapRoot, formulaTapStagingName), inputs, options, epoch); err != nil {
		return nil, fmt.Errorf("verify private Formula staging tree: %w", err)
	}
	if err := options.check(formulaTapAfterTreeSealed); err != nil {
		return nil, err
	}
	if err := options.check(formulaTapBeforePublish); err != nil {
		return nil, err
	}
	if err := requireFormulaTapEntryAbsent(parent, publishedFormulaDirName); err != nil {
		return nil, err
	}
	if err := publishFormulaTapDirectoryNoReplace(parent, formulaTapStagingName, publishedFormulaDirName); err != nil {
		return nil, fmt.Errorf("atomically publish verified Formula tree: %w", err)
	}
	currentTreeName = publishedFormulaDirName
	if err := options.check(formulaTapAfterPublish); err != nil {
		return nil, err
	}
	if err := syncFormulaTapDirectory(parent); err != nil {
		return nil, fmt.Errorf("sync published Formula tree parent: %w", err)
	}
	if err := sealFormulaTapFile(parent, options.ownerUID, options.ownerGID, 0o555, epoch); err != nil {
		return nil, fmt.Errorf("seal published Formula tree parent: %w", err)
	}
	if err := options.check(formulaTapAfterParentSealed); err != nil {
		return nil, err
	}
	if err := validateProtectedHomebrewRepository(prefix, options, true); err != nil {
		return nil, fmt.Errorf("validate protected Homebrew repository after Formula publication: %w", err)
	}
	if err := verifyFormulaTree(filepath.Join(tapRoot, publishedFormulaDirName), inputs, options, epoch); err != nil {
		return nil, fmt.Errorf("verify published Formula tree: %w", err)
	}

	committed = true
	return evidence, nil
}

func verifiedFormulaInputs(record *resolution.Record, verified map[string]bottle.Result, sources map[string][]byte) ([]stagedFormulaInput, error) {
	if record == nil {
		return nil, errors.New("nil resolution record")
	}
	if len(record.Nodes) == 0 || len(verified) != len(record.Nodes) || len(sources) != len(record.Nodes) {
		return nil, fmt.Errorf("verified Formula closure is incomplete: nodes=%d results=%d sources=%d", len(record.Nodes), len(verified), len(sources))
	}
	if record.SourceDateEpoch <= 0 {
		return nil, fmt.Errorf("invalid Formula staging epoch %d", record.SourceDateEpoch)
	}

	nodes := append([]resolution.Node(nil), record.Nodes...)
	slices.SortFunc(nodes, func(a, b resolution.Node) int { return strings.Compare(a.Name, b.Name) })
	inputs := make([]stagedFormulaInput, 0, len(nodes))
	for _, node := range nodes {
		if node.Name == "" || node.FullName != "homebrew/core/"+node.Name {
			return nil, fmt.Errorf("Formula %q is not a canonical homebrew/core node", node.Name)
		}
		result, ok := verified[node.Name]
		if !ok {
			return nil, fmt.Errorf("Formula %q has no verified bottle result", node.Name)
		}
		source, ok := sources[node.Name]
		if !ok || len(source) == 0 {
			return nil, fmt.Errorf("Formula %q has no verified source bytes", node.Name)
		}
		expectedBottlePath := path.Join(result.KegPrefix, ".brew", node.Name+".rb")
		if result.Name != node.Name || result.PkgVersion != node.PkgVersion || result.Formula.Path != expectedBottlePath {
			return nil, fmt.Errorf("Formula %q verified source identity is inconsistent", node.Name)
		}
		if result.Formula.Size != int64(len(source)) {
			return nil, fmt.Errorf("Formula %q source size %d does not match verified size %d", node.Name, len(source), result.Formula.Size)
		}
		sum := sha256.Sum256(source)
		actualDigest := "sha256:" + hex.EncodeToString(sum[:])
		if actualDigest != result.Formula.SHA256 {
			return nil, fmt.Errorf("Formula %q source digest %s does not match verified digest %s", node.Name, actualDigest, result.Formula.SHA256)
		}
		shard, filename, err := coreTapFormulaLocation(node.Name)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, stagedFormulaInput{
			node: node, result: result, source: source,
			shard: shard, filename: filename,
			tapPath: path.Join("Formula", shard, filename),
		})
	}
	return inputs, nil
}

func coreTapFormulaLocation(name string) (shard, filename string, err error) {
	if name == "" || strings.ContainsAny(name, `/\\`) || name != strings.ToLower(name) {
		return "", "", fmt.Errorf("invalid canonical Formula name %q", name)
	}
	if first := name[0]; (first < 'a' || first > 'z') && (first < '0' || first > '9') {
		return "", "", fmt.Errorf("invalid canonical Formula name %q", name)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune("+_.@-", r) {
			return "", "", fmt.Errorf("invalid canonical Formula name %q", name)
		}
	}
	if strings.HasPrefix(name, "lib") {
		shard = "lib"
	} else {
		shard = name[:1]
	}
	return shard, name + ".rb", nil
}

func validateProtectedHomebrewRepository(prefix string, options formulaTapStageOptions, requireFormula bool) error {
	if err := validateProtectedPrefixAncestors(prefix, options); err != nil {
		return err
	}
	for _, rel := range []string{"Homebrew", "Homebrew/bin", "Homebrew/Library", "Homebrew/Library/Taps", "Homebrew/Library/Taps/homebrew", coreTapRoot, protectedHomebrewLogicalDir} {
		if err := validateSealedFormulaTapDirectory(filepath.Join(prefix, filepath.FromSlash(rel)), options); err != nil {
			return err
		}
	}
	for _, rel := range []string{protectedHomebrewBrew, protectedHomebrewBrewReal} {
		if err := validateSealedHomebrewExecutable(filepath.Join(prefix, filepath.FromSlash(rel)), options); err != nil {
			return err
		}
	}
	logical := filepath.Join(prefix, filepath.FromSlash(protectedHomebrewLogical))
	logicalInfo, err := os.Lstat(logical)
	if err != nil {
		return fmt.Errorf("inspect protected logical brew path: %w", err)
	}
	if logicalInfo.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("protected logical brew path %s is not a symlink", logical)
	}
	if err := requireFormulaTapOwnership(logicalInfo, options.ownerUID, options.ownerGID, logical); err != nil {
		return err
	}
	target, err := os.Readlink(logical)
	if err != nil || filepath.ToSlash(target) != "../"+protectedHomebrewBrewReal {
		return fmt.Errorf("protected logical brew path %s has unexpected target %q", logical, target)
	}
	resolved, err := filepath.EvalSymlinks(logical)
	if err != nil || resolved != filepath.Join(prefix, filepath.FromSlash(protectedHomebrewBrewReal)) {
		return fmt.Errorf("protected logical brew path %s does not resolve to the sealed Homebrew launcher", logical)
	}
	formulaRoot := filepath.Join(prefix, filepath.FromSlash(coreTapFormulaRoot))
	formulaInfo, err := os.Lstat(formulaRoot)
	if !requireFormula {
		if err == nil {
			return fmt.Errorf("live core tap Formula tree already exists before transactional staging")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect absent live core tap Formula tree: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect published core tap Formula tree: %w", err)
	}
	if !formulaInfo.IsDir() || formulaInfo.Mode()&os.ModeSymlink != 0 || formulaInfo.Mode().Perm()&0o222 != 0 {
		return fmt.Errorf("published core tap Formula tree is not a sealed real directory")
	}
	return requireFormulaTapOwnership(formulaInfo, options.ownerUID, options.ownerGID, formulaRoot)
}

func validateProtectedPrefixAncestors(prefix string, options formulaTapStageOptions) error {
	clean := filepath.Clean(prefix)
	if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
		return fmt.Errorf("invalid protected Homebrew prefix %q", prefix)
	}
	chain := []string{}
	for current := clean; ; current = filepath.Dir(current) {
		chain = append(chain, current)
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	slices.Reverse(chain)
	for _, current := range chain {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect protected prefix ancestor %s: %w", current, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("protected prefix ancestor %s is not a real directory", current)
		}
		uid, gid, known := snapshotOwnership(info)
		if !known {
			return fmt.Errorf("protected prefix ancestor %s ownership is unavailable", current)
		}
		if int(uid) != 0 && int(uid) != options.ownerUID {
			return fmt.Errorf("protected prefix ancestor %s is owned by untrusted uid %d", current, uid)
		}
		if int(uid) == options.runtimeUID {
			return fmt.Errorf("protected prefix ancestor %s is owned by runtime uid %d", current, options.runtimeUID)
		}
		mode := info.Mode().Perm()
		runtimeWritable := mode&0o002 != 0 || (int(gid) == options.runtimeGID && mode&0o020 != 0)
		// A sticky directory such as /tmp does not let the runtime identity
		// replace a child entry owned by root or the trusted fixture owner.
		if runtimeWritable && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("protected prefix ancestor %s is writable by runtime identity %d:%d", current, options.runtimeUID, options.runtimeGID)
		}
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return err
	}
	if err := requireFormulaTapOwnership(info, options.ownerUID, options.ownerGID, clean); err != nil {
		return fmt.Errorf("protected prefix anchor: %w", err)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("protected prefix anchor %s is group- or world-writable", clean)
	}
	return nil
}

func validateSealedHomebrewExecutable(name string, options formulaTapStageOptions) error {
	info, err := os.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect protected brew executable %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o222 != 0 {
		return fmt.Errorf("protected brew executable %s is not a sealed executable regular file", name)
	}
	return requireFormulaTapOwnership(info, options.ownerUID, options.ownerGID, name)
}

func validateSealedFormulaTapDirectory(name string, options formulaTapStageOptions) error {
	info, err := os.Lstat(name)
	if err != nil {
		return fmt.Errorf("inspect protected Homebrew path %s: %w", name, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("protected Homebrew path %s is not a real directory", name)
	}
	if err := requireFormulaTapOwnership(info, options.ownerUID, options.ownerGID, name); err != nil {
		return err
	}
	if info.Mode().Perm()&0o222 != 0 {
		return fmt.Errorf("protected Homebrew path %s is writable", name)
	}
	return nil
}

func requireFormulaTapOwnership(info os.FileInfo, uid, gid int, name string) error {
	actualUID, actualGID, known := snapshotOwnership(info)
	if !known || int(actualUID) != uid || int(actualGID) != gid {
		return fmt.Errorf("protected Homebrew path %s is not owned by %d:%d", name, uid, gid)
	}
	return nil
}

func verifyFormulaTree(root string, inputs []stagedFormulaInput, options formulaTapStageOptions, epoch time.Time) error {
	if err := verifyFormulaTapPathMetadata(root, true, 0, options, 0o555, epoch); err != nil {
		return err
	}
	expected := map[string]map[string]stagedFormulaInput{}
	for _, input := range inputs {
		if expected[input.shard] == nil {
			expected[input.shard] = map[string]stagedFormulaInput{}
		}
		expected[input.shard][input.filename] = input
	}
	rootEntries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(rootEntries) != len(expected) {
		return fmt.Errorf("Formula tree has %d shards, expected %d", len(rootEntries), len(expected))
	}
	for _, shardEntry := range rootEntries {
		files, ok := expected[shardEntry.Name()]
		if !ok || !shardEntry.IsDir() || shardEntry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Formula tree contains unexpected shard %q", shardEntry.Name())
		}
		shardPath := filepath.Join(root, shardEntry.Name())
		if err := verifyFormulaTapPathMetadata(shardPath, true, 0, options, 0o555, epoch); err != nil {
			return err
		}
		entries, err := os.ReadDir(shardPath)
		if err != nil {
			return err
		}
		if len(entries) != len(files) {
			return fmt.Errorf("Formula shard %q has %d files, expected %d", shardEntry.Name(), len(entries), len(files))
		}
		for _, entry := range entries {
			input, ok := files[entry.Name()]
			if !ok || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
				return fmt.Errorf("Formula shard %q contains unexpected entry %q", shardEntry.Name(), entry.Name())
			}
			filename := filepath.Join(shardPath, entry.Name())
			if err := verifyFormulaTapPathMetadata(filename, false, int64(len(input.source)), options, 0o444, epoch); err != nil {
				return err
			}
			data, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			if !bytes.Equal(data, input.source) {
				return fmt.Errorf("staged Formula %q content changed", input.node.Name)
			}
		}
	}
	return nil
}

func verifyFormulaTapPathMetadata(name string, directory bool, size int64, options formulaTapStageOptions, mode os.FileMode, epoch time.Time) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != directory || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("Formula tap path %s has an unsafe type", name)
	}
	if !directory && info.Size() != size {
		return fmt.Errorf("Formula tap path %s has size %d, expected %d", name, info.Size(), size)
	}
	if info.Mode().Perm() != mode.Perm() || !info.ModTime().Equal(epoch) {
		return fmt.Errorf("Formula tap path %s metadata is not sealed", name)
	}
	return requireFormulaTapOwnership(info, options.ownerUID, options.ownerGID, name)
}

func requireFormulaTapEntryAbsent(parent *os.File, name string) error {
	if name == "" || filepath.Base(name) != name {
		return fmt.Errorf("invalid Formula tap entry name %q", name)
	}
	_, err := os.Lstat(filepath.Join(parent.Name(), name))
	if err == nil {
		return fmt.Errorf("core tap entry %q already exists", name)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect core tap entry %q: %w", name, err)
	}
	return nil
}

func rollbackFormulaTapTransaction(parent *os.File, currentTreeName string, options formulaTapStageOptions, original os.FileInfo) error {
	var errs []error
	if err := makeFormulaTapDirectoryOwnerWritable(parent, options.ownerUID, options.ownerGID); err != nil {
		errs = append(errs, fmt.Errorf("make parent writable for rollback: %w", err))
	} else if currentTreeName != "" {
		if err := removeFormulaTapTree(parent, currentTreeName); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", currentTreeName, err))
		}
	}
	if err := syncFormulaTapDirectory(parent); err != nil {
		errs = append(errs, fmt.Errorf("sync rollback: %w", err))
	}
	if err := sealFormulaTapFile(parent, options.ownerUID, options.ownerGID, original.Mode().Perm(), original.ModTime()); err != nil {
		errs = append(errs, fmt.Errorf("restore core tap parent metadata: %w", err))
	}
	return errors.Join(errs...)
}

func createFormulaTapFileExclusive(directory *os.File, name string, source []byte, uid, gid int, epoch time.Time) (retErr error) {
	f, err := createFormulaTapRegularNoFollow(directory, name, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	written, err := io.Copy(f, bytes.NewReader(source))
	if err != nil {
		return err
	}
	if written != int64(len(source)) {
		return fmt.Errorf("wrote %d bytes, expected %d", written, len(source))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	read, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if read != int64(len(source)) || !bytes.Equal(h.Sum(nil), sha256Bytes(source)) {
		return fmt.Errorf("staged Formula content did not round-trip exactly")
	}
	if err := sealFormulaTapFile(f, uid, gid, 0o444, epoch); err != nil {
		return err
	}
	return verifySealedFormulaTapFile(f, int64(len(source)), uid, gid, 0o444)
}

func sha256Bytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func verifySealedFormulaTapFile(f *os.File, size int64, uid, gid int, mode os.FileMode) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() != size || info.Mode().Perm() != mode.Perm() {
		return fmt.Errorf("staged Formula file metadata is not sealed")
	}
	actualUID, actualGID, known := snapshotOwnership(info)
	if !known || int(actualUID) != uid || int(actualGID) != gid {
		return fmt.Errorf("staged Formula file is not owned by %d:%d", uid, gid)
	}
	return nil
}

func makeFormulaTapDirectoryOwnerWritable(f *os.File, uid, gid int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("Formula tap parent is not a directory")
	}
	if err := requireFormulaTapOwnership(info, uid, gid, f.Name()); err != nil {
		return err
	}
	if err := f.Chmod(0o755); err != nil {
		return err
	}
	return f.Sync()
}

func sealFormulaTapFile(f *os.File, uid, gid int, mode os.FileMode, epoch time.Time) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	actualUID, actualGID, known := snapshotOwnership(info)
	if !known {
		return fmt.Errorf("Formula tap ownership is unavailable")
	}
	if int(actualUID) != uid || int(actualGID) != gid {
		if err := f.Chown(uid, gid); err != nil {
			return err
		}
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := setFormulaTapFileTimes(f, epoch); err != nil {
		return err
	}
	return f.Sync()
}

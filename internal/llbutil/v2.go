package llbutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/moby/buildkit/client/llb"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sozercan/dalec-homebrew/internal/fetcher"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

// BottleInputsV2 contains exact bottles and transport evidence. HTTPS bottles
// are produced by one isolated networked fetcher exec each; OCI bottles retain
// the stronger ImageBlob replay path.
type BottleInputsV2 struct {
	Bottles       llb.State
	FetchEvidence llb.State
}

func ResolutionStateV2(record *resolution.RecordV2) (llb.State, []byte, error) {
	data, err := resolution.CanonicalV2(record)
	if err != nil {
		return llb.Scratch(), nil, err
	}
	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	return llb.Scratch().File(llb.Mkfile("/resolution.json", 0o444, data, llb.WithCreatedTime(epoch))), data, nil
}

func BottleStatesV2(fetcherRef string, platform ocispec.Platform, record *resolution.RecordV2) (BottleInputsV2, error) {
	if record == nil {
		return BottleInputsV2{}, errors.New("nil V2 resolution")
	}
	if err := validateV2PlatformBinding(record, platform); err != nil {
		return BottleInputsV2{}, err
	}
	if fetcherRef != record.Components.BottleFetcherRef {
		return BottleInputsV2{}, errors.New("bottle fetcher reference differs from V2 record")
	}
	if err := resolution.ValidateV2(record); err != nil {
		return BottleInputsV2{}, err
	}
	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	bottles := llb.Scratch()
	evidence := llb.Scratch()
	seenFiles := map[string]struct{}{}
	for _, id := range record.InstallOrder {
		node, ok := findNodeV2(record.Nodes, id)
		if !ok {
			return BottleInputsV2{}, fmt.Errorf("install node %q is missing", id)
		}
		filename := node.Bottle.Filename
		if _, duplicate := seenFiles[filename]; duplicate {
			return BottleInputsV2{}, fmt.Errorf("duplicate V2 bottle filename %q", filename)
		}
		seenFiles[filename] = struct{}{}
		switch {
		case node.Bottle.Transport.OCI != nil:
			transport := node.Bottle.Transport.OCI
			blob := llb.ImageBlob(transport.Registry+"/"+transport.Repository+"@"+transport.Layer.Digest, llb.Filename(filename), llb.Chmod(0o444))
			bottles = bottles.File(llb.Copy(blob, "/"+filename, "/"+filename, &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch}))
		case node.Bottle.Transport.HTTPS != nil:
			if fetcherRef == "" {
				return BottleInputsV2{}, fmt.Errorf("HTTPS bottle %q requires a release-bound fetcher reference", id)
			}
			fetched, err := httpsBottleStateV2(fetcherRef, platform, record.SourceDateEpoch, node)
			if err != nil {
				return BottleInputsV2{}, err
			}
			bottles = bottles.File(llb.Copy(fetched, "/bottle", "/"+filename, &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch}))
			evidenceName := fmt.Sprintf("%03d.fetch.json", len(seenFiles)-1)
			evidence = evidence.File(llb.Copy(fetched, "/evidence.json", "/"+evidenceName, &llb.CopyInfo{CreateDestPath: true, CreatedTime: &epoch}))
		default:
			return BottleInputsV2{}, fmt.Errorf("node %q has no bottle transport", id)
		}
	}
	return BottleInputsV2{Bottles: bottles, FetchEvidence: evidence}, nil
}

func httpsBottleStateV2(fetcherRef string, platform ocispec.Platform, sourceDateEpoch int64, node resolution.NodeV2) (llb.State, error) {
	request, err := fetchRequestV2(node)
	if err != nil {
		return llb.Scratch(), err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return llb.Scratch(), err
	}
	epoch := time.Unix(sourceDateEpoch, 0).UTC()
	requestState := llb.Scratch().File(llb.Mkfile("/request.json", 0o444, data, llb.WithCreatedTime(epoch)))
	worker := llb.Image(fetcherRef, llb.Platform(platform))
	run := worker.Run(
		llb.Args([]string{"/dalec-homebrew-bottle-fetcher", "--request", "/run/dalec-homebrew/request.json", "--output", "/out/bottle", "--evidence", "/out/evidence.json"}),
		llb.User("0:0"),
		llb.AddMount("/run/dalec-homebrew", requestState, llb.Readonly),
		llb.AddMount("/out", llb.Scratch()),
		llb.WithCustomName("fetch Homebrew bottle "+node.ID.String()),
	)
	return run.GetMount("/out"), nil
}

func fetchRequestV2(node resolution.NodeV2) (fetcher.Request, error) {
	transport := node.Bottle.Transport.HTTPS
	if transport == nil {
		return fetcher.Request{}, errors.New("V2 node does not use HTTPS transport")
	}
	sha := strings.TrimPrefix(transport.SHA256, "sha256:")
	if len(sha) != 64 || sha == transport.SHA256 {
		return fetcher.Request{}, fmt.Errorf("V2 HTTPS bottle %q has non-canonical sha256 digest", node.ID)
	}
	if path.Base(transport.Filename) != transport.Filename || transport.Filename != node.Bottle.Filename {
		return fetcher.Request{}, fmt.Errorf("V2 HTTPS bottle %q filename mismatch", node.ID)
	}
	request := fetcher.Request{SchemaVersion: fetcher.RequestSchemaVersion, FetchPolicyVersion: fetcher.FetchPolicyVersion, ArtifactID: node.ID.String(), URL: transport.URL, ExpectedSize: transport.ExpectedSize, SHA256: sha, Filename: transport.Filename, AllowedRedirectHosts: append([]string(nil), transport.AllowedRedirectHosts...)}
	if err := fetcher.ValidateRequest(request); err != nil {
		return fetcher.Request{}, fmt.Errorf("V2 HTTPS fetch request: %w", err)
	}
	return request, nil
}

func validateV2PlatformBinding(record *resolution.RecordV2, platform ocispec.Platform) error {
	if record == nil {
		return errors.New("nil V2 resolution")
	}
	if record.Input.Platform.OS != platform.OS || record.Input.Platform.Architecture != platform.Architecture || record.Input.Platform.Variant != platform.Variant {
		return fmt.Errorf("LLB platform %s/%s does not match V2 record %s/%s", platform.OS, platform.Architecture, record.Input.Platform.OS, record.Input.Platform.Architecture)
	}
	return nil
}

func findNodeV2(nodes []resolution.NodeV2, id resolution.FormulaID) (resolution.NodeV2, bool) {
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return resolution.NodeV2{}, false
}

// PreparedV2 is the immutable output of the prepare phase. Prefix contains the
// seeded Homebrew checkout and staged tap trees; State contains verified bottle
// copies, fetch evidence, the trust store, and preparation evidence.
type PreparedV2 struct {
	Prefix llb.State
	State  llb.State
}

func PrepareMaterializationV2(materializerRef string, platform ocispec.Platform, record *resolution.RecordV2, inputs BottleInputsV2) (PreparedV2, error) {
	if materializerRef == "" {
		return PreparedV2{}, errors.New("empty materializer reference")
	}
	if record == nil || materializerRef != record.Components.MaterializerRef {
		return PreparedV2{}, errors.New("materializer reference differs from V2 record")
	}
	if err := validateV2PlatformBinding(record, platform); err != nil {
		return PreparedV2{}, err
	}
	if err := resolution.ValidateV2(record); err != nil {
		return PreparedV2{}, err
	}
	worker := llb.Image(materializerRef, llb.Platform(platform))
	seed := llb.Scratch().File(llb.Copy(worker, DefaultHomebrewPrefixV2, "/", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true}))
	recordState, _, err := ResolutionStateV2(record)
	if err != nil {
		return PreparedV2{}, err
	}
	run := worker.Run(
		llb.Args([]string{"/usr/local/bin/dalec-homebrew-materializer", "prepare-v2", "--resolution", "/run/dalec-homebrew/input/resolution.json", "--bottles", "/run/dalec-homebrew/bottles", "--fetch-evidence", "/run/dalec-homebrew/fetch-evidence", "--prefix", DefaultHomebrewPrefixV2, "--prepared-root", "/prepared"}),
		llb.User("0:0"), llb.Network(llb.NetModeNone),
		llb.AddMount("/run/dalec-homebrew/input", recordState, llb.Readonly),
		llb.AddMount("/run/dalec-homebrew/bottles", inputs.Bottles, llb.Readonly),
		llb.AddMount("/run/dalec-homebrew/fetch-evidence", inputs.FetchEvidence, llb.Readonly),
		llb.AddMount(DefaultHomebrewPrefixV2, seed),
		llb.AddMount("/prepared", llb.Scratch()),
		llb.WithLinuxResources(llb.LinuxResources{Memory: 8 << 30, MemorySwap: 8 << 30, CPUQuota: 400000, CPUPeriod: 100000}),
		llb.WithCustomName("prepare verified Homebrew V2 closure"),
	)
	prefix := ensurePreparedPrefixDirectoriesV2(run.GetMount(DefaultHomebrewPrefixV2))
	return PreparedV2{Prefix: prefix, State: run.GetMount("/prepared")}, nil
}

const DefaultHomebrewPrefixV2 = "/home/linuxbrew/.linuxbrew"

var preparedPrefixDirectoriesV2 = []string{
	"Caskroom",
	"Cellar",
	"Frameworks",
	"bin",
	"etc",
	"include",
	"lib",
	"opt",
	"sbin",
	"share",
	"var",
}

func ensurePreparedPrefixDirectoriesV2(state llb.State) llb.State {
	for _, directory := range preparedPrefixDirectoriesV2 {
		state = state.File(llb.Mkdir("/"+directory, 0o755, llb.WithParents(true), llb.WithUIDGID(1000, 1000)))
	}
	return state
}

// InstalledV2 contains the cumulative prefix state and one delta evidence file
// per install-order entry.
type InstalledV2 struct {
	Prefix   llb.State
	Evidence llb.State
}

func InstallPreparedV2(materializerRef string, platform ocispec.Platform, record *resolution.RecordV2, prepared PreparedV2) (InstalledV2, error) {
	if materializerRef == "" {
		return InstalledV2{}, errors.New("empty materializer reference")
	}
	if record == nil || materializerRef != record.Components.MaterializerRef {
		return InstalledV2{}, errors.New("materializer reference differs from V2 record")
	}
	if err := validateV2PlatformBinding(record, platform); err != nil {
		return InstalledV2{}, err
	}
	if err := resolution.ValidateV2(record); err != nil {
		return InstalledV2{}, err
	}
	recordState, _, err := ResolutionStateV2(record)
	if err != nil {
		return InstalledV2{}, err
	}
	prefix := prepared.Prefix
	evidence := llb.Scratch()
	for index, id := range record.InstallOrder {
		node, ok := findNodeV2(record.Nodes, id)
		if !ok {
			return InstalledV2{}, fmt.Errorf("install node %q is missing", id)
		}
		worker := llb.Image(materializerRef, llb.Platform(platform))
		installInput := llb.Scratch().
			File(llb.Copy(prepared.State, "/bottles/"+node.Bottle.Filename, "/bottles/"+node.Bottle.Filename, &llb.CopyInfo{CreateDestPath: true})).
			File(llb.Copy(prepared.State, "/homebrew-config", "/homebrew-config", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true})).
			File(llb.Copy(prepared.State, "/preparation.json", "/preparation.json", &llb.CopyInfo{CreateDestPath: true}))
		evidenceName := fmt.Sprintf("%03d.json", index)
		run := worker.Run(
			llb.Args([]string{"/usr/local/bin/dalec-homebrew-materializer", "install-one-v2", "--resolution", "/run/dalec-homebrew/input/resolution.json", "--id", id.String(), "--bottle", "/run/dalec-homebrew/prepared/bottles/" + node.Bottle.Filename, "--prefix", DefaultHomebrewPrefixV2, "--homebrew-config", "/run/dalec-homebrew/prepared/homebrew-config", "--preparation", "/run/dalec-homebrew/prepared/preparation.json", "--evidence", "/evidence/" + evidenceName}),
			llb.User("0:0"), llb.Network(llb.NetModeNone),
			llb.AddMount("/run/dalec-homebrew/input", recordState, llb.Readonly),
			llb.AddMount("/run/dalec-homebrew/prepared", installInput, llb.Readonly),
			llb.AddMount(DefaultHomebrewPrefixV2, prefix),
			llb.AddMount("/evidence", evidence),
			llb.AddMount("/home/linuxbrew/.cache", llb.Scratch()),
			llb.AddMount("/tmp", llb.Scratch()),
			llb.AddMount("/var/tmp", llb.Scratch()),
			llb.WithLinuxResources(llb.LinuxResources{Memory: 8 << 30, MemorySwap: 8 << 30, CPUQuota: 400000, CPUPeriod: 100000}),
			llb.WithCustomName("offline install Homebrew bottle "+id.String()),
		)
		prefix = run.GetMount(DefaultHomebrewPrefixV2)
		evidence = run.GetMount("/evidence")
	}
	return InstalledV2{Prefix: prefix, Evidence: evidence}, nil
}

func FinalizeMaterializationV2(materializerRef string, platform ocispec.Platform, record *resolution.RecordV2, prepared PreparedV2, installed InstalledV2) (llb.State, error) {
	if materializerRef == "" {
		return llb.Scratch(), errors.New("empty materializer reference")
	}
	if record == nil || materializerRef != record.Components.MaterializerRef {
		return llb.Scratch(), errors.New("materializer reference differs from V2 record")
	}
	if err := validateV2PlatformBinding(record, platform); err != nil {
		return llb.Scratch(), err
	}
	if err := resolution.ValidateV2(record); err != nil {
		return llb.Scratch(), err
	}
	recordState, _, err := ResolutionStateV2(record)
	if err != nil {
		return llb.Scratch(), err
	}
	worker := llb.Image(materializerRef, llb.Platform(platform))
	run := worker.Run(
		llb.Args([]string{"/usr/local/bin/dalec-homebrew-materializer", "finalize-v2", "--resolution", "/run/dalec-homebrew/input/resolution.json", "--prefix", DefaultHomebrewPrefixV2, "--output", "/out", "--preparation", "/run/dalec-homebrew/prepared/preparation.json", "--install-evidence", "/run/dalec-homebrew/install-evidence"}),
		llb.User("0:0"), llb.Network(llb.NetModeNone),
		llb.AddMount("/run/dalec-homebrew/input", recordState, llb.Readonly),
		llb.AddMount("/run/dalec-homebrew/prepared", prepared.State, llb.Readonly),
		llb.AddMount("/run/dalec-homebrew/install-evidence", installed.Evidence, llb.Readonly),
		llb.AddMount(DefaultHomebrewPrefixV2, installed.Prefix),
		llb.AddMount("/out", llb.Scratch()),
		llb.AddMount("/tmp", llb.Scratch()),
		llb.AddMount("/var/tmp", llb.Scratch()),
		llb.WithLinuxResources(llb.LinuxResources{Memory: 8 << 30, MemorySwap: 8 << 30, CPUQuota: 400000, CPUPeriod: 100000}),
		llb.WithCustomName("finalize verified Homebrew V2 runtime"),
	)
	return run.GetMount("/out"), nil
}

func MaterializeV2(materializerRef, fetcherRef string, platform ocispec.Platform, record *resolution.RecordV2) (llb.State, error) {
	inputs, err := BottleStatesV2(fetcherRef, platform, record)
	if err != nil {
		return llb.Scratch(), err
	}
	prepared, err := PrepareMaterializationV2(materializerRef, platform, record, inputs)
	if err != nil {
		return llb.Scratch(), err
	}
	installed, err := InstallPreparedV2(materializerRef, platform, record, prepared)
	if err != nil {
		return llb.Scratch(), err
	}
	return FinalizeMaterializationV2(materializerRef, platform, record, prepared, installed)
}

func AssembleRuntimeV2(runtimeBaseRef string, platform ocispec.Platform, materialized llb.State, record *resolution.RecordV2) (llb.State, error) {
	if runtimeBaseRef == "" {
		return llb.Scratch(), fmt.Errorf("empty runtime base reference")
	}
	if record == nil {
		return llb.Scratch(), errors.New("nil V2 resolution")
	}
	if runtimeBaseRef != record.Components.RuntimeBaseRef {
		return llb.Scratch(), errors.New("runtime base reference differs from V2 record")
	}
	if err := validateV2PlatformBinding(record, platform); err != nil {
		return llb.Scratch(), err
	}
	epoch := time.Unix(record.SourceDateEpoch, 0).UTC()
	base := llb.Image(runtimeBaseRef, llb.Platform(platform))
	return base.File(llb.Copy(materialized, "/", "/", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true, CreatedTime: &epoch})), nil
}

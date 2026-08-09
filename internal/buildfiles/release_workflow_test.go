package buildfiles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReleaseTagWorkflowDispatcher(t *testing.T) {
	workflow := workflowYAML(t, "release-tag.yml")
	on := yamlMappingValue(t, workflow, "on")
	requireYAMLMappingKeys(t, on, "push")
	push := yamlMappingValue(t, on, "push")
	requireYAMLMappingKeys(t, push, "tags")
	requireYAMLStringSequence(t, yamlMappingValue(t, push, "tags"), "v*.*.*")
	if got := yamlStringSequence(t, yamlMappingValue(t, push, "tags")); len(got) != 1 || got[0] != "v*.*.*" {
		t.Fatalf("release tag workflow tags = %v, want [v*.*.*]", got)
	}
	requireYAMLMappingKeys(t, yamlMappingValue(t, workflow, "permissions"))

	jobs := yamlMappingValue(t, workflow, "jobs")
	requireYAMLMappingKeys(t, jobs, "dispatch")
	dispatch := yamlMappingValue(t, jobs, "dispatch")
	if got := yamlScalarValue(t, yamlMappingValue(t, dispatch, "if")); got != "github.event.created && !github.event.deleted && github.ref_type == 'tag' && startsWith(github.ref_name, 'v')" {
		t.Fatalf("release tag dispatcher guard = %q", got)
	}
	jobPermissions := yamlMappingValue(t, dispatch, "permissions")
	requireYAMLMappingKeys(t, jobPermissions, "actions")
	if got := yamlScalarValue(t, yamlMappingValue(t, jobPermissions, "actions")); got != "write" {
		t.Fatalf("release tag dispatcher actions permission = %q, want write", got)
	}

	steps := yamlMappingValue(t, dispatch, "steps")
	if steps.Kind != yaml.SequenceNode {
		t.Fatalf("release tag dispatcher steps YAML kind = %v, want sequence", steps.Kind)
	}
	for _, step := range steps.Content {
		if _, ok := yamlMappingLookup(t, step, "uses"); ok {
			t.Fatalf("release tag dispatcher contains a uses step: %q", yamlOptionalScalar(t, step, "name"))
		}
		if run := yamlOptionalScalar(t, step, "run"); strings.Contains(strings.ToLower(run), "checkout") {
			t.Fatalf("release tag dispatcher step %q performs a checkout", yamlOptionalScalar(t, step, "name"))
		}
	}

	dispatchStep := workflowStepByName(t, workflow, "dispatch", "Dispatch release workflow from main")
	dispatchEnvironment := yamlMappingValue(t, dispatchStep, "env")
	wantEnvironment := map[string]string{
		"EXPECTED_SHA": "${{ github.sha }}",
		"GH_TOKEN":     "${{ github.token }}",
		"RELEASE_TAG":  "${{ github.ref_name }}",
		"TAG_CREATED":  "${{ github.event.created }}",
		"TAG_DELETED":  "${{ github.event.deleted }}",
	}
	wantEnvironmentKeys := make([]string, 0, len(wantEnvironment))
	for key := range wantEnvironment {
		wantEnvironmentKeys = append(wantEnvironmentKeys, key)
	}
	requireYAMLMappingKeys(t, dispatchEnvironment, wantEnvironmentKeys...)
	for key, want := range wantEnvironment {
		if got := yamlScalarValue(t, yamlMappingValue(t, dispatchEnvironment, key)); got != want {
			t.Fatalf("release tag dispatcher environment %s = %q, want %q", key, got, want)
		}
	}

	temporary := t.TempDir()
	argumentsPath := filepath.Join(temporary, "gh-arguments")
	fakeGH := filepath.Join(temporary, "gh")
	if err := os.WriteFile(fakeGH, []byte("#!/bin/sh\nset -eu\nprintf '%s\\n' \"$@\" > \"$GH_ARGUMENTS\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	expectedSHA := strings.Repeat("a", 40)
	dispatchScript := yamlScalarValue(t, yamlMappingValue(t, dispatchStep, "run"))
	baseEnvironment := map[string]string{
		"EXPECTED_SHA":      expectedSHA,
		"GH_ARGUMENTS":      argumentsPath,
		"GH_TOKEN":          "unused",
		"GITHUB_EVENT_NAME": "push",
		"GITHUB_REF":        "refs/tags/v1.2.3",
		"GITHUB_REF_TYPE":   "tag",
		"GITHUB_REPOSITORY": "sozercan/dalec-homebrew",
		"PATH":              temporary + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RELEASE_TAG":       "v1.2.3",
		"TAG_CREATED":       "true",
		"TAG_DELETED":       "false",
	}
	output, err := runWorkflowShell(t, dispatchScript, baseEnvironment)
	if err != nil {
		t.Fatalf("release tag dispatcher failed: %v\n%s", err, output)
	}
	data, err := os.ReadFile(argumentsPath)
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	requireArgumentSequence(t, arguments, "api", "--method", "POST")
	requireArgument(t, arguments, "repos/sozercan/dalec-homebrew/actions/workflows/release.yml/dispatches")
	requireArgumentSequence(t, arguments, "-f", "ref=main")
	requireArgumentSequence(t, arguments, "-f", "inputs[tag]=v1.2.3")
	requireArgumentSequence(t, arguments, "-f", "inputs[expected_sha]="+expectedSHA)

	for _, tt := range []struct {
		name    string
		created string
		deleted string
	}{
		{name: "tag update", created: "false", deleted: "false"},
		{name: "tag deletion", created: "false", deleted: "true"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			environment := maps.Clone(baseEnvironment)
			environment["TAG_CREATED"] = tt.created
			environment["TAG_DELETED"] = tt.deleted
			if output, err := runWorkflowShell(t, dispatchScript, environment); err == nil {
				t.Fatalf("non-creation tag event dispatched a release\n%s", output)
			}
		})
	}
}

func TestReleaseWorkflowRequestTriggers(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")
	on := yamlMappingValue(t, workflow, "on")
	requireYAMLMappingKeys(t, on, "repository_dispatch", "workflow_dispatch")

	repositoryDispatch := yamlMappingValue(t, on, "repository_dispatch")
	requireYAMLMappingKeys(t, repositoryDispatch, "types")
	requireYAMLStringSequence(t, yamlMappingValue(t, repositoryDispatch, "types"), "release")

	workflowDispatch := yamlMappingValue(t, on, "workflow_dispatch")
	requireYAMLMappingKeys(t, workflowDispatch, "inputs")
	inputs := yamlMappingValue(t, workflowDispatch, "inputs")
	requireYAMLMappingKeys(t, inputs, "expected_sha", "tag")
	expectedSHAInput := yamlMappingValue(t, inputs, "expected_sha")
	if got := yamlScalarValue(t, yamlMappingValue(t, expectedSHAInput, "required")); got != "false" {
		t.Fatalf("workflow_dispatch expected_sha required = %q, want false", got)
	}
	if got := yamlScalarValue(t, yamlMappingValue(t, expectedSHAInput, "type")); got != "string" {
		t.Fatalf("workflow_dispatch expected_sha type = %q, want string", got)
	}
	tagInput := yamlMappingValue(t, inputs, "tag")
	if got := yamlScalarValue(t, yamlMappingValue(t, tagInput, "required")); got != "true" {
		t.Fatalf("workflow_dispatch tag required = %q, want true", got)
	}
	if got := yamlScalarValue(t, yamlMappingValue(t, tagInput, "type")); got != "string" {
		t.Fatalf("workflow_dispatch tag type = %q, want string", got)
	}

	requestStep := workflowStepByName(t, workflow, "prepare", "Validate release request")
	requestEnvironment := yamlMappingValue(t, requestStep, "env")
	requireYAMLMappingKeys(t, requestEnvironment, "INPUT_EXPECTED_SHA", "INPUT_TAG")
	if got := yamlScalarValue(t, yamlMappingValue(t, requestEnvironment, "INPUT_EXPECTED_SHA")); got != "${{ inputs.expected_sha || github.event.client_payload.expected_sha }}" {
		t.Fatalf("release request expected SHA expression = %q", got)
	}
	if got := yamlScalarValue(t, yamlMappingValue(t, requestEnvironment, "INPUT_TAG")); got != "${{ inputs.tag || github.event.client_payload.tag }}" {
		t.Fatalf("release request tag expression = %q", got)
	}

	requestScript := yamlScalarValue(t, yamlMappingValue(t, requestStep, "run"))
	expectedSHA := strings.Repeat("a", 40)
	valid := []struct {
		event       string
		expectedSHA string
	}{
		{event: "workflow_dispatch", expectedSHA: expectedSHA},
		{event: "repository_dispatch"},
	}
	for _, tt := range valid {
		t.Run("valid "+tt.event, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "github-output")
			output, err := runWorkflowShell(t, requestScript, map[string]string{
				"GITHUB_EVENT_NAME":  tt.event,
				"GITHUB_OUTPUT":      outputPath,
				"GITHUB_REF":         "refs/heads/main",
				"INPUT_EXPECTED_SHA": tt.expectedSHA,
				"INPUT_TAG":          "v1.2.3",
				"PATH":               os.Getenv("PATH"),
			})
			if err != nil {
				t.Fatalf("valid %s request rejected: %v\n%s", tt.event, err, output)
			}
			outputs := readWorkflowOutputs(t, outputPath)
			if outputs["tag"] != "v1.2.3" || outputs["expected_sha"] != tt.expectedSHA || outputs["stable"] != "true" {
				t.Fatalf("%s outputs = %v", tt.event, outputs)
			}
		})
	}

	invalid := []struct {
		name        string
		event       string
		ref         string
		expectedSHA string
	}{
		{name: "workflow run", event: "workflow_run", ref: "refs/heads/main", expectedSHA: expectedSHA},
		{name: "create event", event: "create", ref: "refs/heads/main", expectedSHA: expectedSHA},
		{name: "wrong ref", event: "workflow_dispatch", ref: "refs/tags/v1.2.3", expectedSHA: expectedSHA},
		{name: "invalid expected SHA", event: "workflow_dispatch", ref: "refs/heads/main", expectedSHA: "not-a-commit"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			output, err := runWorkflowShell(t, requestScript, map[string]string{
				"GITHUB_EVENT_NAME":  tt.event,
				"GITHUB_OUTPUT":      filepath.Join(t.TempDir(), "github-output"),
				"GITHUB_REF":         tt.ref,
				"INPUT_EXPECTED_SHA": tt.expectedSHA,
				"INPUT_TAG":          "v1.2.3",
				"PATH":               os.Getenv("PATH"),
			})
			if err == nil {
				t.Fatalf("invalid release request accepted\n%s", output)
			}
		})
	}
}

func TestReleaseWorkflowBindsLiveTagToPushedSHA(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")
	verifyStep := workflowStepByName(t, workflow, "prepare", "Verify tag and main reachability")
	verifyEnvironment := yamlMappingValue(t, verifyStep, "env")
	if got := yamlScalarValue(t, yamlMappingValue(t, verifyEnvironment, "EXPECTED_SHA")); got != "${{ steps.request.outputs.expected_sha }}" {
		t.Fatalf("verify EXPECTED_SHA = %q", got)
	}
	verifyScript := yamlScalarValue(t, yamlMappingValue(t, verifyStep, "run"))
	for _, contract := range []string{
		`tag_sha=$(git rev-parse "$RELEASE_TAG^{commit}")`,
		`if [[ -n "$EXPECTED_SHA" && "$tag_sha" != "$EXPECTED_SHA" ]]; then`,
	} {
		if !strings.Contains(verifyScript, contract) {
			t.Fatalf("verify step does not bind the live tag to the pushed SHA; missing %q", contract)
		}
	}

	temporary := t.TempDir()
	fakeGit := filepath.Join(temporary, "git")
	if err := os.WriteFile(fakeGit, []byte(`#!/bin/sh
set -eu
case "$1" in
  show-ref) exit 0 ;;
  rev-parse) printf '%s\n' "$LIVE_SHA" ;;
  *) exit 2 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeGH := filepath.Join(temporary, "gh")
	if err := os.WriteFile(fakeGH, []byte("#!/bin/sh\nset -eu\nprintf '%s\\n' identical\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	liveSHA := strings.Repeat("a", 40)
	tests := []struct {
		name        string
		expectedSHA string
		wantErr     bool
	}{
		{name: "matching pushed SHA", expectedSHA: liveSHA},
		{name: "mismatched pushed SHA", expectedSHA: strings.Repeat("b", 40), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputPath := filepath.Join(t.TempDir(), "github-output")
			output, err := runWorkflowShell(t, verifyScript, map[string]string{
				"EXPECTED_SHA":      tt.expectedSHA,
				"GITHUB_OUTPUT":     outputPath,
				"GITHUB_REPOSITORY": "sozercan/dalec-homebrew",
				"LIVE_SHA":          liveSHA,
				"PATH":              temporary + string(os.PathListSeparator) + os.Getenv("PATH"),
				"RELEASE_TAG":       "v1.2.3",
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("tag SHA verification error = %v, wantErr %v\n%s", err, tt.wantErr, output)
			}
			if !tt.wantErr {
				outputs := readWorkflowOutputs(t, outputPath)
				if outputs["sha"] != liveSHA {
					t.Fatalf("verified tag SHA = %q, want %q", outputs["sha"], liveSHA)
				}
			}
		})
	}
}

func TestReleaseWorkflowProvenanceRunValidation(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")
	validationStep := workflowStepByName(t, workflow, "sign", "Validate release tuple before signing")
	validationScript := yamlScalarValue(t, yamlMappingValue(t, validationStep, "run"))
	validationScript = provenanceRunValidationShell(t, validationScript)

	temporary := t.TempDir()
	responsePath := filepath.Join(temporary, "gh-response.json")
	fakeGH := filepath.Join(temporary, "gh")
	if err := os.WriteFile(fakeGH, []byte("#!/bin/sh\nset -eu\ncat \"$GH_RESPONSE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workflowSHA := strings.Repeat("a", 40)
	tests := []struct {
		name       string
		event      string
		headBranch string
		headSHA    string
		path       string
		wantErr    bool
	}{
		{name: "repository dispatch", event: "repository_dispatch", headBranch: "main", headSHA: workflowSHA, path: ".github/workflows/release.yml"},
		{name: "workflow dispatch", event: "workflow_dispatch", headBranch: "main", headSHA: workflowSHA, path: ".github/workflows/release.yml@main"},
		{name: "workflow run", event: "workflow_run", headBranch: "main", headSHA: workflowSHA, path: ".github/workflows/release.yml@refs/heads/main", wantErr: true},
		{name: "untrusted event", event: "push", headBranch: "main", headSHA: workflowSHA, path: ".github/workflows/release.yml", wantErr: true},
		{name: "wrong branch", event: "workflow_dispatch", headBranch: "release", headSHA: workflowSHA, path: ".github/workflows/release.yml", wantErr: true},
		{name: "wrong SHA", event: "workflow_dispatch", headBranch: "main", headSHA: strings.Repeat("b", 40), path: ".github/workflows/release.yml", wantErr: true},
		{name: "wrong path", event: "workflow_dispatch", headBranch: "main", headSHA: workflowSHA, path: ".github/workflows/release-tag.yml", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := json.Marshal(map[string]any{
				"id":          123,
				"run_attempt": 2,
				"event":       tt.event,
				"head_branch": tt.headBranch,
				"head_sha":    tt.headSHA,
				"path":        tt.path,
				"repository":  map[string]any{"full_name": "sozercan/dalec-homebrew"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(responsePath, response, 0o600); err != nil {
				t.Fatal(err)
			}
			script := "set -euo pipefail\nrun_id=123\nrun_attempt=2\nworkflow_sha=" + workflowSHA + "\n" + validationScript
			output, err := runWorkflowShell(t, script, map[string]string{
				"GH_RESPONSE":       responsePath,
				"GITHUB_REPOSITORY": "sozercan/dalec-homebrew",
				"PATH":              temporary + string(os.PathListSeparator) + os.Getenv("PATH"),
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("provenance run validation error = %v, wantErr %v\n%s", err, tt.wantErr, output)
			}
		})
	}
}

func TestReleaseWorkflowSpecInventory(t *testing.T) {
	workflow := releaseWorkflowText(t)
	match := regexp.MustCompile(`(?m)^  RELEASE_SPECS: (.+)$`).FindStringSubmatch(workflow)
	if len(match) != 2 {
		t.Fatal("release workflow does not define RELEASE_SPECS")
	}
	got := strings.Fields(match[1])
	sort.Strings(got)

	paths, err := filepath.Glob(filepath.Join(repositoryRoot(t), "examples", "live-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]string, 0, len(paths))
	for _, path := range paths {
		want = append(want, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	}
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("RELEASE_SPECS = %v, want every focused live spec %v", got, want)
	}
	if strings.Count(workflow, `"$RELEASE_SPECS"`) < 3 {
		t.Fatal("release workflow does not reuse RELEASE_SPECS across build and validation paths")
	}

	amd64Only := regexp.MustCompile(`(?m)^  RELEASE_AMD64_ONLY_SPECS: (.+)$`).FindStringSubmatch(workflow)
	if len(amd64Only) != 2 {
		t.Fatal("release workflow does not define RELEASE_AMD64_ONLY_SPECS")
	}
	if got, want := strings.Fields(amd64Only[1]), []string{"ci-noncore-multi-package"}; !slices.Equal(got, want) {
		t.Fatalf("RELEASE_AMD64_ONLY_SPECS = %v, want %v", got, want)
	}
	if strings.Count(workflow, `"$RELEASE_AMD64_ONLY_SPECS"`) < 3 {
		t.Fatal("release workflow does not reuse RELEASE_AMD64_ONLY_SPECS across build and validation paths")
	}
}

func TestReleaseRuntimeEvidenceAssertionsAcceptOnlyCoherentSchemas(t *testing.T) {
	workflow := releaseWorkflowText(t)
	match := regexp.MustCompile(`(?m)^  RELEASE_SPECS: (.+)$`).FindStringSubmatch(workflow)
	if len(match) != 2 {
		t.Fatal("release workflow does not define RELEASE_SPECS")
	}

	releaseSpecs := make(map[string]struct{})
	for _, spec := range strings.Fields(match[1]) {
		releaseSpecs[spec] = struct{}{}
	}
	evidenceSpecs := []string{
		"live-glibc",
		"live-graphviz",
		"live-python",
		"live-redis",
	}
	wantFixtureSchemas := []string{
		"dalec-homebrew-runtime-manifest/v1",
		"dalec-homebrew-resolution/v1",
		"dalec-homebrew-runtime-inventory/v1",
		"dalec-homebrew-prune-manifest/v2",
		"dalec-homebrew-runtime-manifest/v2",
		"dalec-homebrew-resolution/v2",
		"dalec-homebrew-runtime-inventory/v2",
		"dalec-homebrew-prune-manifest/v3",
	}
	wantFixtureMaterializationAssertions := []string{
		`materialization_v1="$evidence_dir/materialization.json"`,
		`materialization_v2="$evidence_dir/materialization-v2.json"`,
		`test ! -e "$materialization_v2"`,
		`test ! -e "$materialization_v1"`,
		`test ! -L "$materialization_v2"`,
		`test ! -L "$materialization_v1"`,
		`test -f "$materialization_path"`,
		`test ! -L "$materialization_path"`,
		`test -s "$materialization_path"`,
		`test "$(stat -c '%a' "$materialization_path")" = 444`,
		`grep -Fq 'verified_bottles' "$materialization_path"`,
		`grep -Fq 'dalec-homebrew-materialization/v2' "$materialization_path"`,
		`! grep -Fq 'dalec-homebrew-materialization/v2' "$materialization_path"`,
	}

	for _, spec := range evidenceSpecs {
		if _, ok := releaseSpecs[spec]; !ok {
			t.Errorf("%s is not included in RELEASE_SPECS", spec)
		}
		path := filepath.Join(repositoryRoot(t), "examples", spec+".yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte("/usr/share/dalec-homebrew/manifest.json:")) {
			t.Errorf("%s does not assert the runtime evidence manifest", spec)
		}
		for _, schema := range wantFixtureSchemas {
			if !bytes.Contains(data, []byte(schema)) {
				t.Errorf("%s does not accept coherent runtime evidence schema %q", spec, schema)
			}
		}
		if !bytes.Contains(data, []byte("runtime evidence schemas are not a coherent V1 or V2 tuple")) {
			t.Errorf("%s does not reject mixed runtime evidence schemas", spec)
		}
		for _, assertion := range wantFixtureMaterializationAssertions {
			if !bytes.Contains(data, []byte(assertion)) {
				t.Errorf("%s does not bind materialization evidence with %q", spec, assertion)
			}
		}
		if bytes.Contains(data, []byte("/usr/share/dalec-homebrew/materialization.json:\n")) {
			t.Errorf("%s still requires the V1 materialization filename unconditionally", spec)
		}
	}

	validatorPath := filepath.Join(repositoryRoot(t), "scripts", "vm-live-validate.sh")
	validator, err := os.ReadFile(validatorPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range wantFixtureSchemas {
		if !bytes.Contains(validator, []byte(schema)) {
			t.Errorf("vm-live-validate.sh does not accept coherent runtime evidence schema %q", schema)
		}
	}
	if !bytes.Contains(validator, []byte("runtime evidence schemas are not a coherent V1 or V2 tuple")) {
		t.Error("vm-live-validate.sh does not reject mixed runtime evidence schemas")
	}
	for _, assertion := range []string{
		`materialization_path=$EVIDENCE_DIR/materialization.json`,
		`materialization_path=$EVIDENCE_DIR/materialization-v2.json`,
		`[ ! -e "$EVIDENCE_DIR/materialization-v2.json" ]`,
		`[ ! -e "$EVIDENCE_DIR/materialization.json" ]`,
		`[ ! -L "$EVIDENCE_DIR/materialization-v2.json" ]`,
		`[ ! -L "$EVIDENCE_DIR/materialization.json" ]`,
		`[ -f "$materialization_path" ]`,
		`[ ! -L "$materialization_path" ]`,
		`[ -s "$materialization_path" ]`,
		`assert_root_owned_non_user_writable "$materialization_path"`,
		`grep -Fq '"verified_bottles"' "$materialization_path"`,
		`grep -Fq '"schema_version":"dalec-homebrew-materialization/v2"' "$materialization_path"`,
		`! grep -Fq 'dalec-homebrew-materialization/v2' "$materialization_path"`,
	} {
		if !bytes.Contains(validator, []byte(assertion)) {
			t.Errorf("vm-live-validate.sh does not bind materialization evidence with %q", assertion)
		}
	}
	if bytes.Contains(validator, []byte("  materialization.json \\\n")) {
		t.Error("vm-live-validate.sh still requires the V1 materialization filename unconditionally")
	}
}

func TestVMLiveValidateAcceptsOnlyCoherentEvidenceSchemas(t *testing.T) {
	validatorPath := filepath.Join(repositoryRoot(t), "scripts", "vm-live-validate.sh")
	validator, err := os.ReadFile(validatorPath)
	if err != nil {
		t.Fatal(err)
	}

	startMarker := []byte(`if grep -Fq '"schema_version":"dalec-homebrew-runtime-manifest/v1"'`)
	if count := bytes.Count(validator, startMarker); count != 1 {
		t.Fatalf("vm-live-validate.sh V1 schema check count = %d, want 1", count)
	}
	start := bytes.Index(validator, startMarker)
	endMarker := []byte("\nfi\ngrep -Fq '\"spdxVersion\":\"SPDX-2.3\"'")
	end := bytes.Index(validator[start:], endMarker)
	if end == -1 {
		t.Fatal("vm-live-validate.sh does not terminate the evidence schema check before the SPDX check")
	}
	schemaCheck := validator[start : start+end+len("\nfi")]

	documents := []struct {
		name string
		v1   string
		v2   string
	}{
		{name: "manifest.json", v1: "dalec-homebrew-runtime-manifest/v1", v2: "dalec-homebrew-runtime-manifest/v2"},
		{name: "resolution.json", v1: "dalec-homebrew-resolution/v1", v2: "dalec-homebrew-resolution/v2"},
		{name: "runtime-inventory.json", v1: "dalec-homebrew-runtime-inventory/v1", v2: "dalec-homebrew-runtime-inventory/v2"},
		{name: "prune-manifest.json", v1: "dalec-homebrew-prune-manifest/v2", v2: "dalec-homebrew-prune-manifest/v3"},
	}

	writeDocuments := func(t *testing.T, evidenceDir string, mask int) {
		t.Helper()
		for i, document := range documents {
			schema := document.v1
			if mask&(1<<i) != 0 {
				schema = document.v2
			}
			body := []byte(`{"schema_version":"` + schema + `"}`)
			if err := os.WriteFile(filepath.Join(evidenceDir, document.name), body, 0o444); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeMaterialization := func(t *testing.T, evidenceDir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(evidenceDir, name), []byte(body), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	runCheck := func(t *testing.T, mask int, materializations, symlinks map[string]string, wantPass bool) {
		t.Helper()
		evidenceDir := t.TempDir()
		writeDocuments(t, evidenceDir, mask)
		for name, body := range materializations {
			writeMaterialization(t, evidenceDir, name, body)
		}
		for name, target := range symlinks {
			if err := os.Symlink(target, filepath.Join(evidenceDir, name)); err != nil {
				t.Fatal(err)
			}
		}

		script := "set -eu\nfail() { exit 1; }\nassert_root_owned_non_user_writable() { :; }\n" + string(schemaCheck)
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = append(os.Environ(), "EVIDENCE_DIR="+evidenceDir)
		output, err := cmd.CombinedOutput()
		if gotPass := err == nil; gotPass != wantPass {
			t.Fatalf("evidence check pass = %v, want %v: %v\n%s", gotPass, wantPass, err, output)
		}
	}

	const (
		v1Materialization = `{"verified_bottles":[]}`
		v2Materialization = `{"schema_version":"dalec-homebrew-materialization/v2","verified_bottles":[]}`
	)
	allV2 := 1<<len(documents) - 1
	for mask := 0; mask < 1<<len(documents); mask++ {
		versions := make([]string, 0, len(documents))
		for i := range documents {
			version := "v1"
			if mask&(1<<i) != 0 {
				version = "v2"
			}
			versions = append(versions, version)
		}
		t.Run(strings.Join(versions, "-"), func(t *testing.T) {
			materializations := map[string]string{"materialization.json": v1Materialization}
			if mask == allV2 {
				materializations = map[string]string{"materialization-v2.json": v2Materialization}
			}
			runCheck(t, mask, materializations, nil, mask == 0 || mask == allV2)
		})
	}

	for _, test := range []struct {
		name             string
		mask             int
		materializations map[string]string
		symlinks         map[string]string
	}{
		{name: "v1-missing", mask: 0},
		{name: "v2-missing", mask: allV2},
		{name: "v1-wrong-filename", mask: 0, materializations: map[string]string{"materialization-v2.json": v1Materialization}},
		{name: "v2-wrong-filename", mask: allV2, materializations: map[string]string{"materialization.json": v2Materialization}},
		{name: "v1-both-filenames", mask: 0, materializations: map[string]string{"materialization.json": v1Materialization, "materialization-v2.json": v2Materialization}},
		{name: "v2-both-filenames", mask: allV2, materializations: map[string]string{"materialization.json": v1Materialization, "materialization-v2.json": v2Materialization}},
		{name: "v1-v2-content", mask: 0, materializations: map[string]string{"materialization.json": v2Materialization}},
		{name: "v2-v1-content", mask: allV2, materializations: map[string]string{"materialization-v2.json": v1Materialization}},
		{name: "v1-empty", mask: 0, materializations: map[string]string{"materialization.json": ""}},
		{name: "v2-missing-verified-bottles", mask: allV2, materializations: map[string]string{"materialization-v2.json": `{"schema_version":"dalec-homebrew-materialization/v2"}`}},
		{name: "v1-selected-symlink", mask: 0, materializations: map[string]string{"materialization-target.json": v1Materialization}, symlinks: map[string]string{"materialization.json": "materialization-target.json"}},
		{name: "v2-selected-symlink", mask: allV2, materializations: map[string]string{"materialization-target.json": v2Materialization}, symlinks: map[string]string{"materialization-v2.json": "materialization-target.json"}},
		{name: "v1-dangling-v2-symlink", mask: 0, materializations: map[string]string{"materialization.json": v1Materialization}, symlinks: map[string]string{"materialization-v2.json": "missing"}},
		{name: "v2-dangling-v1-symlink", mask: allV2, materializations: map[string]string{"materialization-v2.json": v2Materialization}, symlinks: map[string]string{"materialization.json": "missing"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runCheck(t, test.mask, test.materializations, test.symlinks, false)
		})
	}
}

func TestReleaseWorkflowBindsExternalDalecFrontend(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")

	buildScript := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "build", "Build children and assemble component indexes"), "run"))
	for _, want := range []string{
		`dalec_frontend_index=$(jq -er .dalec_frontend.index dist/release/inputs.json)`,
		`expected_ref=$(jq -er --arg platform "$platform" '.dalec_frontend.platforms[$platform]' dist/release/inputs.json)`,
		`actual_digest=$(platform_digest "$dalec_frontend_index" "$platform")`,
		`(.platform.os // "") != "unknown"`,
	} {
		if !strings.Contains(buildScript, want) {
			t.Fatalf("release build does not validate upstream Dalec frontend binding with %q", want)
		}
	}

	integrationScript := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "integration", "Build and test a runtime from the released tuple"), "run"))
	for _, want := range []string{
		`dalec_frontend_ref=$(jq -er --arg platform "$PLATFORM" '.dalec_frontend.platforms[$platform]' dist/release/inputs.json)`,
		`dalec_frontend_route=$(jq -er .dalec_frontend.route dist/release/inputs.json)`,
		`DALEC_HOMEBREW_LIVE_DALEC_FRONTEND_REF="$dalec_frontend_ref"`,
		`DALEC_HOMEBREW_LIVE_TARGET="$dalec_frontend_route"`,
	} {
		if !strings.Contains(integrationScript, want) {
			t.Fatalf("release integration does not use upstream Dalec frontend binding with %q", want)
		}
	}

	provenanceScript := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "sign", "Generate SLSA provenance predicate"), "run"))
	for _, want := range []string{
		`dalec_frontend: $inputs[0].dalec_frontend`,
		`uri: ("oci://" + $dalec_frontend.repository)`,
		`digest: {sha256: $dalec_frontend.digest}`,
	} {
		if !strings.Contains(provenanceScript, want) {
			t.Fatalf("release provenance does not bind upstream Dalec frontend with %q", want)
		}
	}

	validationScript := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "sign", "Validate release tuple before signing"), "run"))
	for _, want := range []string{
		`.schema_version == "dalec-homebrew-release-inputs/v2"`,
		`test("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")`,
		`.invocation.parameters.dalec_frontend == $inputs[0].dalec_frontend`,
		`.materials[1].uri == ("oci://" + $dalec_frontend.repository)`,
		`upstream Dalec frontend $platform child`,
	} {
		if !strings.Contains(validationScript, want) {
			t.Fatalf("release tuple validation does not bind upstream Dalec frontend with %q", want)
		}
	}

	ownedSigningScript := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "sign", "Sign images and attach provenance and SBOM attestations"), "run"))
	if strings.Contains(ownedSigningScript, "dalec_frontend") || strings.Contains(ownedSigningScript, "project-dalec") {
		t.Fatal("release signing treats the external Dalec frontend as a repository-owned component")
	}
	promotionScript := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "promote", "Stage assets, promote digests, and publish release"), "run"))
	if strings.Contains(promotionScript, "dalec_frontend") || strings.Contains(promotionScript, "project-dalec") {
		t.Fatal("release promotion treats the external Dalec frontend as a repository-owned component")
	}
}

func TestReleaseWorkflowV2ComponentContract(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")
	buildScript := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "build", "Build children and assemble component indexes"), "run"))
	orderedBuildTokens := []string{
		"for target in runtime-base-amd64 runtime-base-arm64; do",
		"runtime_base_index=$(create_index",
		"bottle-fetcher-amd64 bottle-fetcher-arm64",
		"bottle_fetcher_index=$(create_index",
		"catalog_extractor_index=$(create_index",
		"go run ./cmd/v2-bindings",
		"for target in materializer-amd64 materializer-arm64; do",
		"materializer_index=$(create_index",
		"docker buildx bake frontend",
	}
	previous := -1
	for _, token := range orderedBuildTokens {
		index := strings.Index(buildScript, token)
		if index <= previous {
			t.Fatalf("V2 release build token %q is missing or out of order", token)
		}
		previous = index
	}

	jobs := yamlMappingValue(t, workflow, "jobs")
	evidence := yamlMappingValue(t, jobs, "evidence")
	include := yamlMappingValue(t, yamlMappingValue(t, yamlMappingValue(t, evidence, "strategy"), "matrix"), "include")
	if include.Kind != yaml.SequenceNode {
		t.Fatalf("evidence matrix include kind = %v, want sequence", include.Kind)
	}
	gotSubjects := make([]string, 0, len(include.Content))
	for _, entry := range include.Content {
		component := yamlScalarValue(t, yamlMappingValue(t, entry, "component"))
		platform := yamlScalarValue(t, yamlMappingValue(t, entry, "platform"))
		slug := yamlScalarValue(t, yamlMappingValue(t, entry, "slug"))
		if slug != component+"-"+strings.ReplaceAll(platform, "/", "-") {
			t.Fatalf("evidence matrix slug %q does not bind %s on %s", slug, component, platform)
		}
		gotSubjects = append(gotSubjects, component+" "+platform)
	}
	sort.Strings(gotSubjects)
	wantSubjects := make([]string, 0, 10)
	for _, component := range []string{"frontend", "runtime-base", "bottle-fetcher", "catalog-extractor", "materializer"} {
		for _, platform := range []string{"linux/amd64", "linux/arm64"} {
			wantSubjects = append(wantSubjects, component+" "+platform)
		}
	}
	sort.Strings(wantSubjects)
	if !slices.Equal(gotSubjects, wantSubjects) {
		t.Fatalf("evidence matrix subjects = %v, want %v", gotSubjects, wantSubjects)
	}

	for _, entry := range []struct {
		job  string
		step string
	}{
		{job: "sign", step: "Sign images and attach provenance and SBOM attestations"},
		{job: "promote", step: "Reverify release signatures and attestations"},
	} {
		script := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, entry.job, entry.step), "run"))
		for _, count := range []string{`(( ${#subjects[@]} == 15 ))`, `(( platform_subject_count == 10 ))`} {
			if strings.Count(script, count) != 1 {
				t.Fatalf("%s/%s does not enforce %q exactly once", entry.job, entry.step, count)
			}
		}
	}

	componentLoop := "for component in runtime-base bottle-fetcher catalog-extractor materializer frontend; do"
	presign := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "sign", "Pin source state and validate release tags before signing"), "run"))
	if !strings.Contains(presign, componentLoop) {
		t.Fatal("pre-sign validation does not cover all five V2 components")
	}
	promotion := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "promote", "Stage assets, promote digests, and publish release"), "run"))
	if !strings.Contains(promotion, "components=(runtime-base bottle-fetcher catalog-extractor materializer frontend)") {
		t.Fatal("promotion does not cover all five V2 components")
	}
	recovery := yamlScalarValue(t, yamlMappingValue(t, workflowStepByName(t, workflow, "promote", "Verify signed bundle checksums"), "run"))
	if !strings.Contains(recovery, "expected_contract_count=$((30 + 2 * ${#specs[@]} + ${#amd64_only_specs[@]}))") {
		t.Fatal("recovery asset contract does not count five-component evidence and amd64-only runtime evidence")
	}
}

func TestReleaseWorkflowDalecFrontendProvenance(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")
	step := workflowStepByName(t, workflow, "sign", "Generate SLSA provenance predicate")
	script := yamlScalarValue(t, yamlMappingValue(t, step, "run"))

	root := repositoryRoot(t)
	pinData, err := os.ReadFile(filepath.Join(root, "release", "dalec-frontend.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pin map[string]any
	if err := json.Unmarshal(pinData, &pin); err != nil {
		t.Fatal(err)
	}

	temporary := t.TempDir()
	releaseDir := filepath.Join(temporary, "dist", "release")
	if err := os.MkdirAll(releaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(releaseDir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("inputs.json", map[string]any{
		"source_date_epoch": "1781049600",
		"dalec_frontend":    pin,
	})
	write("digests.json", map[string]any{
		"source": map[string]any{
			"build_invocation": "https://github.com/sozercan/dalec-homebrew/actions/runs/123/attempts/1",
		},
	})

	sha := strings.Repeat("a", 40)
	output, err := runWorkflowShell(t, "cd \"$WORKDIR\"\n"+script, map[string]string{
		"BINFMT_IMAGE":        "example.invalid/binfmt@sha256:" + strings.Repeat("b", 64),
		"BUILDKIT_IMAGE":      "example.invalid/buildkit@sha256:" + strings.Repeat("c", 64),
		"BUILDX_SHA256_AMD64": strings.Repeat("d", 64),
		"BUILDX_SHA256_ARM64": strings.Repeat("e", 64),
		"BUILDX_VERSION":      "v0.36.0",
		"GITHUB_REPOSITORY":   "sozercan/dalec-homebrew",
		"PATH":                os.Getenv("PATH"),
		"RELEASE_SHA":         sha,
		"RELEASE_TAG":         "v1.2.3",
		"SYFT_IMAGE":          "example.invalid/syft@sha256:" + strings.Repeat("f", 64),
		"TRIVY_IMAGE":         "example.invalid/trivy@sha256:" + strings.Repeat("1", 64),
		"WORKDIR":             temporary,
		"WORKFLOW_REF":        "sozercan/dalec-homebrew/.github/workflows/release.yml@refs/heads/main",
		"WORKFLOW_SHA":        sha,
	})
	if err != nil {
		t.Fatalf("generate provenance: %v\n%s", err, output)
	}

	data, err := os.ReadFile(filepath.Join(releaseDir, "provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var provenance struct {
		Invocation struct {
			Parameters map[string]any `json:"parameters"`
		} `json:"invocation"`
		Materials []struct {
			URI    string            `json:"uri"`
			Digest map[string]string `json:"digest"`
		} `json:"materials"`
	}
	if err := json.Unmarshal(data, &provenance); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(provenance.Invocation.Parameters["dalec_frontend"], pin) {
		t.Fatalf("provenance Dalec frontend parameter = %#v, want %#v", provenance.Invocation.Parameters["dalec_frontend"], pin)
	}
	if len(provenance.Materials) != 2 {
		t.Fatalf("provenance materials = %#v", provenance.Materials)
	}
	if provenance.Materials[1].URI != "oci://ghcr.io/project-dalec/dalec/frontend" ||
		provenance.Materials[1].Digest["sha256"] != strings.TrimPrefix(testDalecIndex, "ghcr.io/project-dalec/dalec/frontend@sha256:") {
		t.Fatalf("upstream Dalec provenance material = %#v", provenance.Materials[1])
	}
}

func TestReleaseWorkflowStrictJSONGate(t *testing.T) {
	script := releaseWorkflowPython(t, "PYJSON")
	valid := writeWorkflowFixture(t, "valid.json", []byte(`{"ok":{"nested":true}}`))
	if output, err := runWorkflowPython(t, script, valid); err != nil {
		t.Fatalf("valid JSON rejected: %v\n%s", err, output)
	}

	tests := []struct {
		name string
		data []byte
	}{
		{name: "duplicate", data: []byte(`{"ok":1,"ok":2}`)},
		{name: "nested duplicate", data: []byte(`{"ok":{"nested":1,"nested":2}}`)},
		{name: "second document", data: []byte(`{} {}`)},
		{name: "non JSON constant", data: []byte(`{"value":NaN}`)},
		{name: "oversized", data: bytes.Repeat([]byte(" "), (4<<20)+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeWorkflowFixture(t, "record.json", tt.data)
			if output, err := runWorkflowPython(t, script, path); err == nil {
				t.Fatalf("invalid JSON accepted\n%s", output)
			}
		})
	}
}

func TestReleaseWorkflowRuntimeEvidenceExtractor(t *testing.T) {
	script := releaseWorkflowPython(t, "PYARCHIVE")
	t.Run("valid", func(t *testing.T) {
		archive := writeRuntimeEvidenceArchive(t, []tarEntry{
			{name: "./", typeflag: tar.TypeDir},
			{name: "./inventory.json", body: []byte(`{}`)},
			{name: "./resolution.json", body: []byte(`{"schema_version":"dalec-homebrew-resolution/v1"}`)},
		})
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err != nil {
			t.Fatalf("valid archive rejected: %v\n%s", err, output)
		}
		data, err := os.ReadFile(outputPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte("dalec-homebrew-resolution/v1")) {
			t.Fatalf("unexpected extracted resolution: %s", data)
		}
	})

	tests := []struct {
		name    string
		entries []tarEntry
	}{
		{name: "missing", entries: []tarEntry{{name: "./inventory.json", body: []byte(`{}`)}}},
		{name: "duplicate member", entries: []tarEntry{
			{name: "./resolution.json", body: []byte(`{}`)},
			{name: "./resolution.json", body: []byte(`{}`)},
		}},
		{name: "duplicate JSON", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{"a":1,"a":2}`)}}},
		{name: "bare alias", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "resolution.json", body: []byte(`{}`)}}},
		{name: "redundant segment alias", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "././resolution.json", body: []byte(`{}`)}}},
		{name: "parent segment alias", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "evidence/../resolution.json", body: []byte(`{}`)}}},
		{name: "absolute alias", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "/resolution.json", body: []byte(`{}`)}}},
		{name: "absolute normalized alias", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "/evidence/../resolution.json", body: []byte(`{}`)}}},
		{name: "traversal member", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "../../escape", body: []byte(`bad`)}}},
		{name: "noncanonical member", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "./evidence/../inventory.json", body: []byte(`{}`)}}},
		{name: "unicode control", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "./badname", body: []byte(`{}`)}}},
		{name: "non-resolution symlink", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "./link", typeflag: tar.TypeSymlink, linkname: "../../escape"}}},
		{name: "hardlink", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "./hardlink", typeflag: tar.TypeLink, linkname: "./resolution.json"}}},
		{name: "fifo", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "./fifo", typeflag: tar.TypeFifo}}},
		{name: "character device", entries: []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}, {name: "./device", typeflag: tar.TypeChar}}},
		{name: "symlink", entries: []tarEntry{{name: "./resolution.json", typeflag: tar.TypeSymlink, linkname: "elsewhere"}}},
		{name: "oversized", entries: []tarEntry{{name: "./resolution.json", body: bytes.Repeat([]byte(" "), (16<<20)+1)}}},
		{name: "declared expansion limit", entries: []tarEntry{{name: "./large-evidence.json", body: bytes.Repeat([]byte("0"), (32<<20)+1)}, {name: "./resolution.json", body: []byte(`{}`)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			archive := writeRuntimeEvidenceArchive(t, tt.entries)
			outputPath := filepath.Join(t.TempDir(), "resolution.json")
			if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
				t.Fatalf("invalid archive accepted\n%s", output)
			}
		})
	}

	t.Run("raw trailing space", func(t *testing.T) {
		payload := rawUSTARPayload(t, []rawTarEntry{{name: "./resolution.json ", body: []byte(`{}`), typeflag: '0'}}, 2)
		archive := writeWorkflowGzipPayload(t, payload)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("raw trailing-space path accepted\n%s", output)
		}
	})

	t.Run("reserved header padding", func(t *testing.T) {
		payload := rawUSTARPayload(t, []rawTarEntry{{name: "./resolution.json", body: []byte(`{}`), typeflag: '0', reserved: []byte{1}}}, 2)
		archive := writeWorkflowGzipPayload(t, payload)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("nonzero reserved header padding accepted\n%s", output)
		}
	})

	t.Run("regular trailing slash", func(t *testing.T) {
		payload := rawUSTARPayload(t, []rawTarEntry{
			{name: "./resolution.json", body: []byte(`{}`), typeflag: '0'},
			{name: "./inventory.json/", body: []byte(`{}`), typeflag: '0'},
		}, 2)
		archive := writeWorkflowGzipPayload(t, payload)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("regular file with trailing slash accepted\n%s", output)
		}
	})

	t.Run("regular root", func(t *testing.T) {
		payload := rawUSTARPayload(t, []rawTarEntry{
			{name: "./resolution.json", body: []byte(`{}`), typeflag: '0'},
			{name: "./", body: []byte(`{}`), typeflag: '0'},
		}, 2)
		archive := writeWorkflowGzipPayload(t, payload)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("regular root member accepted\n%s", output)
		}
	})

	t.Run("PAX path extension", func(t *testing.T) {
		payload := rawUSTARPayload(t, []rawTarEntry{
			{name: "./PaxHeaders/resolution.json", body: []byte("27 path=./resolution.json/\n"), typeflag: 'x'},
			{name: "./resolution.json", body: []byte(`{}`), typeflag: '0'},
		}, 2)
		archive := writeWorkflowGzipPayload(t, payload)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("PAX path extension accepted\n%s", output)
		}
	})

	t.Run("PAX expansion limit", func(t *testing.T) {
		archive := writeRuntimeEvidenceArchive(t, []tarEntry{{
			name:       "./resolution.json",
			body:       []byte(`{}`),
			paxRecords: map[string]string{"comment": strings.Repeat("x", (64<<10)+1)},
		}})
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPythonWithExpandedLimit(t, script, 64<<10, archive, outputPath); err == nil {
			t.Fatalf("oversized PAX extension accepted\n%s", output)
		}
	})

	t.Run("malformed header before one end marker", func(t *testing.T) {
		payload := singleResolutionTarPayload(t, 0)
		payload = append(payload, bytes.Repeat([]byte("x"), 512)...)
		payload = append(payload, make([]byte, 512)...)
		archive := writeWorkflowGzipPayload(t, payload)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("malformed terminating header accepted\n%s", output)
		}
	})

	t.Run("missing tar end markers", func(t *testing.T) {
		archive := writeWorkflowGzipPayload(t, singleResolutionTarPayload(t, 0))
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("archive without tar end markers accepted\n%s", output)
		}
	})

	t.Run("single tar end marker", func(t *testing.T) {
		archive := writeWorkflowGzipPayload(t, singleResolutionTarPayload(t, 1))
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("archive with one tar end marker accepted\n%s", output)
		}
	})

	t.Run("concatenated duplicate", func(t *testing.T) {
		first := writeRuntimeEvidenceArchive(t, []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}})
		second := writeRuntimeEvidenceArchive(t, []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}})
		archive := concatenateWorkflowFiles(t, first, second)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("concatenated duplicate archive accepted\n%s", output)
		}
	})

	t.Run("bad gzip tail", func(t *testing.T) {
		archive := writeRuntimeEvidenceArchive(t, []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}})
		file, err := os.OpenFile(archive, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("not-a-gzip-member")); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("archive with bad gzip tail accepted\n%s", output)
		}
	})

	t.Run("zero bytes after gzip", func(t *testing.T) {
		archive := writeRuntimeEvidenceArchive(t, []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}})
		file, err := os.OpenFile(archive, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(make([]byte, 512)); err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, archive, outputPath); err == nil {
			t.Fatalf("zero bytes after gzip accepted\n%s", output)
		}
	})

	t.Run("empty gzip member tail", func(t *testing.T) {
		archive := writeRuntimeEvidenceArchive(t, []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}})
		empty := writeWorkflowGzipPayload(t, nil)
		concatenated := concatenateWorkflowFiles(t, archive, empty)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, concatenated, outputPath); err == nil {
			t.Fatalf("empty gzip member tail accepted\n%s", output)
		}
	})

	t.Run("valid gzip invalid tar tail", func(t *testing.T) {
		archive := writeRuntimeEvidenceArchive(t, []tarEntry{{name: "./resolution.json", body: []byte(`{}`)}})
		tail := writeWorkflowGzipPayload(t, bytes.Repeat([]byte("x"), 512))
		concatenated := concatenateWorkflowFiles(t, archive, tail)
		outputPath := filepath.Join(t.TempDir(), "resolution.json")
		if output, err := runWorkflowPython(t, script, concatenated, outputPath); err == nil {
			t.Fatalf("valid-gzip invalid-tar tail accepted\n%s", output)
		}
	})
}

func TestReleaseWorkflowMetadataReconciliationContract(t *testing.T) {
	workflow := releaseWorkflowText(t)
	for _, contract := range []string{
		`"schema_version": "dalec-homebrew-release-metadata/v2"`,
		`reference_identity["generated_at_source"] != "http-last-modified"`,
		`"authenticated homebrew/core metadata identity differs across release integrations"`,
		`"observations": observations`,
		`accepted = min(observations`,
	} {
		if !strings.Contains(workflow, contract) {
			t.Fatalf("release metadata reconciliation is missing %q", contract)
		}
	}
}

func TestReleaseWorkflowRuntimeEvidenceArtifactLayout(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")
	upload := yamlMappingValue(t, workflowStepByName(t, workflow, "integration", "Upload runtime evidence"), "with")
	for key, want := range map[string]string{
		"name": "runtime-evidence-${{ matrix.slug }}",
		"path": "runtime-evidence-${{ matrix.slug }}-*.tar.gz",
	} {
		if got := yamlScalarValue(t, yamlMappingValue(t, upload, key)); got != want {
			t.Fatalf("runtime evidence upload %s = %q, want %q", key, got, want)
		}
	}

	download := yamlMappingValue(t, workflowStepByName(t, workflow, "sign", "Download runtime evidence"), "with")
	for key, want := range map[string]string{
		"pattern":        "runtime-evidence-*",
		"path":           "dist/release/runtime-evidence",
		"merge-multiple": "true",
	} {
		if got := yamlScalarValue(t, yamlMappingValue(t, download, key)); got != want {
			t.Fatalf("runtime evidence download %s = %q, want %q", key, got, want)
		}
	}
}

func TestReleaseWorkflowRuntimeEvidenceValidation(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")
	step := workflowStepByName(t, workflow, "sign", "Validate runtime evidence and metadata snapshot")
	script := yamlScalarValue(t, yamlMappingValue(t, step, "run"))

	digest := func(value string) string {
		return "sha256:" + strings.Repeat(value, 64)
	}
	frontendRepository := "registry.example/frontend"
	runtimeBaseRepository := "registry.example/runtime-base"
	bottleFetcherRepository := "registry.example/bottle-fetcher"
	catalogExtractorRepository := "registry.example/catalog-extractor"
	materializerRepository := "registry.example/materializer"
	frontendChildren := map[string]string{
		"linux/amd64": digest("1"),
		"linux/arm64": digest("2"),
	}
	frontendIndex := digest("3")
	runtimeBaseIndex := digest("4")
	runtimeBaseChildren := map[string]string{
		"linux/amd64": digest("5"),
		"linux/arm64": digest("6"),
	}
	materializerIndex := digest("7")
	materializerChildren := map[string]string{
		"linux/amd64": digest("8"),
		"linux/arm64": digest("9"),
	}
	bottleFetcherIndex := digest("a")
	bottleFetcherChildren := map[string]string{
		"linux/amd64": digest("b"),
		"linux/arm64": digest("c"),
	}
	catalogExtractorIndex := digest("d")
	catalogExtractorChildren := map[string]string{
		"linux/amd64": digest("e"),
		"linux/arm64": digest("f"),
	}

	digests := map[string]any{
		"components": map[string]any{
			"frontend": map[string]any{
				"repository": frontendRepository,
				"index":      frontendIndex,
				"platforms":  frontendChildren,
			},
			"runtime-base": map[string]any{
				"repository": runtimeBaseRepository,
				"index":      runtimeBaseIndex,
				"platforms":  runtimeBaseChildren,
			},
			"bottle-fetcher": map[string]any{
				"repository": bottleFetcherRepository,
				"index":      bottleFetcherIndex,
				"platforms":  bottleFetcherChildren,
			},
			"catalog-extractor": map[string]any{
				"repository": catalogExtractorRepository,
				"index":      catalogExtractorIndex,
				"platforms":  catalogExtractorChildren,
			},
			"materializer": map[string]any{
				"repository": materializerRepository,
				"index":      materializerIndex,
				"platforms":  materializerChildren,
			},
		},
	}
	manifest := map[string]any{
		"policy_version":                       "homebrew-runtime-v2",
		"tap_policy_digest":                    digest("1"),
		"executable_runtime_policy_digest":     digest("2"),
		"supported_catalog_policy_versions":    []any{"tap-catalog-v1"},
		"supported_fetch_policy_versions":      []any{"homebrew-bottle-fetch-v1"},
		"supported_provenance_policy_versions": []any{"provenance-v1"},
	}
	homebrewCommit := strings.Repeat("a", 40)
	inputs := map[string]any{
		"homebrew_commit":          homebrewCommit,
		"portable_ruby_version":    "ruby",
		"verification_keys_digest": "keys",
		"dalec_module":             "dalec",
		"buildkit_module":          "buildkit",
	}

	makeRecord := func(platform, generatedAt string, sourceDateEpoch int64) map[string]any {
		architecture := strings.TrimPrefix(platform, "linux/")
		metadataDigest := digest("0")
		return map[string]any{
			"schema_version":    "dalec-homebrew-resolution/v2",
			"input":             map[string]any{"platform": map[string]any{"os": "linux", "architecture": architecture}},
			"policy_version":    "homebrew-runtime-v2",
			"source_date_epoch": sourceDateEpoch,
			"metadata_sources": []any{map[string]any{
				"tap":                 "homebrew/core",
				"commit":              homebrewCommit,
				"signer":              map[string]any{"key_id": "homebrew-1", "algorithm": "PS512", "verified": true},
				"documents":           []any{map[string]any{"name": "formula", "digest": metadataDigest, "envelope_digest": digest("1")}, map[string]any{"name": "migrations", "digest": digest("2"), "envelope_digest": digest("3")}},
				"generated_at":        generatedAt,
				"generated_at_source": "http-last-modified",
				"fetched_at":          "2026-08-03T00:01:00Z",
				"sequence":            sourceDateEpoch,
				"rollback":            map[string]any{"policy": "homebrew-core-generated-at-v1", "sequence_floor": 0, "state_digest": metadataDigest},
			}},
			"components": map[string]any{
				"frontend_index_ref":                   frontendRepository + "@" + frontendIndex,
				"frontend_ref":                         frontendRepository + "@" + frontendChildren[platform],
				"runtime_base_ref":                     runtimeBaseRepository + "@" + runtimeBaseChildren[platform],
				"materializer_ref":                     materializerRepository + "@" + materializerChildren[platform],
				"bottle_fetcher_ref":                   bottleFetcherRepository + "@" + bottleFetcherIndex,
				"catalog_extractor_ref":                catalogExtractorRepository + "@" + catalogExtractorIndex,
				"catalog_service_origin":               "",
				"ingestion_jws_key_policy_digest":      "",
				"tap_policy_digest":                    manifest["tap_policy_digest"],
				"executable_runtime_policy_digest":     manifest["executable_runtime_policy_digest"],
				"homebrew_commit":                      homebrewCommit,
				"ruby_runtime":                         "ruby",
				"verification_keys_digest":             "keys",
				"dalec_module":                         "dalec",
				"buildkit_module":                      "buildkit",
				"supported_catalog_policy_versions":    manifest["supported_catalog_policy_versions"],
				"supported_fetch_policy_versions":      manifest["supported_fetch_policy_versions"],
				"supported_provenance_policy_versions": manifest["supported_provenance_policy_versions"],
			},
		}
	}
	coreSource := func(record map[string]any) map[string]any {
		return record["metadata_sources"].([]any)[0].(map[string]any)
	}
	makeNoncoreRecord := func() map[string]any {
		record := makeRecord("linux/amd64", "2026-08-03T00:00:00Z", 1785715200)
		record["metadata_sources"] = append(record["metadata_sources"].([]any), map[string]any{
			"tap":                    "svt/avtools",
			"generated_at":           "2026-08-03T00:00:00Z",
			"fetched_at":             "2026-08-03T00:01:00Z",
			"catalog_policy_version": "tap-catalog-v1",
			"extraction": map[string]any{
				"policy_version": "build-local-tap-extraction-v1",
				"extractor_ref":  catalogExtractorRepository + "@" + catalogExtractorIndex,
			},
		})
		record["nodes"] = []any{map[string]any{
			"id": "svt/avtools/libdf",
			"bottle": map[string]any{"transport": map[string]any{
				"oci":   nil,
				"https": map[string]any{"fetch_policy_version": "homebrew-bottle-fetch-v1"},
			}},
		}}
		return record
	}
	makeMaterialization := func() map[string]any {
		return map[string]any{
			"schema_version": "dalec-homebrew-materialization/v2",
			"preparation": map[string]any{
				"schema_version": "dalec-homebrew-preparation/v2",
				"fetch_evidence": []any{map[string]any{
					"artifact_id":          "svt/avtools/libdf",
					"schema_version":       "bottle-fetch-evidence/v1",
					"fetch_policy_version": "homebrew-bottle-fetch-v1",
				}},
			},
		}
	}

	type validationCase struct {
		name                  string
		flat                  bool
		omitPlatform          string
		omitNoncore           bool
		releaseSpecs          string
		mutate                func(map[string]map[string]any)
		mutateMaterialization func(map[string]any)
		wantError             string
	}
	tests := []validationCase{
		{name: "download artifact layout"},
		{name: "signed bundle layout", flat: true},
		{
			name: "runtime base index ref",
			mutate: func(records map[string]map[string]any) {
				records["linux/amd64"]["components"].(map[string]any)["runtime_base_ref"] = runtimeBaseRepository + "@" + runtimeBaseIndex
			},
			wantError: "runtime evidence binding mismatch for live-test on linux/amd64",
		},
		{
			name: "materializer index ref",
			mutate: func(records map[string]map[string]any) {
				records["linux/amd64"]["components"].(map[string]any)["materializer_ref"] = materializerRepository + "@" + materializerIndex
			},
			wantError: "runtime evidence binding mismatch for live-test on linux/amd64",
		},
		{
			name: "catalog extractor execution evidence",
			mutate: func(records map[string]map[string]any) {
				sources := records["noncore"]["metadata_sources"].([]any)
				sources[1].(map[string]any)["extraction"].(map[string]any)["extractor_ref"] = catalogExtractorRepository + "@" + digest("0")
			},
			wantError: "does not prove published catalog extractor execution",
		},
		{
			name: "bottle fetcher execution evidence",
			mutateMaterialization: func(materialization map[string]any) {
				materialization["preparation"].(map[string]any)["fetch_evidence"] = []any{}
			},
			wantError: "does not prove published bottle fetcher execution",
		},
		{
			name: "HTTP metadata timestamp drift",
			mutate: func(records map[string]map[string]any) {
				coreSource(records["linux/arm64"])["generated_at"] = "2026-08-03T00:00:01Z"
				coreSource(records["linux/arm64"])["sequence"] = int64(1785715201)
				records["linux/arm64"]["source_date_epoch"] = int64(1785715201)
			},
		},
		{
			name: "signed metadata timestamp drift",
			mutate: func(records map[string]map[string]any) {
				for _, record := range records {
					coreSource(record)["generated_at_source"] = "signed-payload"
				}
				coreSource(records["linux/arm64"])["generated_at"] = "2026-08-03T00:00:01Z"
				coreSource(records["linux/arm64"])["sequence"] = int64(1785715201)
				records["linux/arm64"]["source_date_epoch"] = int64(1785715201)
			},
			wantError: "signed homebrew/core generated_at differs across release integrations",
		},
		{
			name: "authenticated metadata drift",
			mutate: func(records map[string]map[string]any) {
				documents := coreSource(records["linux/arm64"])["documents"].([]any)
				documents[0].(map[string]any)["digest"] = digest("f")
			},
			wantError: "authenticated homebrew/core metadata identity differs across release integrations",
		},
		{
			name: "metadata commit differs from release input",
			mutate: func(records map[string]map[string]any) {
				for _, record := range records {
					coreSource(record)["commit"] = strings.Repeat("b", 40)
				}
			},
			wantError: "homebrew/core commit does not match release input",
		},
		{
			name: "missing timestamp source",
			mutate: func(records map[string]map[string]any) {
				delete(coreSource(records["linux/arm64"]), "generated_at_source")
			},
			wantError: "homebrew/core metadata source has unexpected fields",
		},
		{
			name: "resolution epoch mismatch",
			mutate: func(records map[string]map[string]any) {
				records["linux/arm64"]["source_date_epoch"] = int64(1785715199)
			},
			wantError: "resolution source_date_epoch does not match the earliest metadata source",
		},
		{
			name:         "duplicate spec inventory",
			releaseSpecs: "live-test live-test",
			wantError:    "duplicate metadata observation for live-test on linux/amd64",
		},
		{
			name:         "missing archive",
			omitPlatform: "linux/arm64",
			wantError:    "missing or empty runtime evidence archive: dist/release/runtime-evidence/runtime-evidence-linux-arm64-live-test.tar.gz",
		},
		{
			name:        "missing amd64-only archive",
			omitNoncore: true,
			wantError:   "missing or empty runtime evidence archive: dist/release/runtime-evidence/runtime-evidence-linux-amd64-ci-noncore-multi-package.tar.gz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			releaseDirectory := filepath.Join(root, "dist", "release")
			runtimeEvidenceDirectory := filepath.Join(releaseDirectory, "runtime-evidence")
			if tt.flat {
				runtimeEvidenceDirectory = releaseDirectory
			}
			if err := os.MkdirAll(runtimeEvidenceDirectory, 0o700); err != nil {
				t.Fatal(err)
			}

			writeJSON := func(name string, value any) {
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(releaseDirectory, name), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			writeJSON("digests.json", digests)
			writeJSON("components.json", manifest)
			writeJSON("inputs.json", inputs)

			records := map[string]map[string]any{
				"linux/amd64": makeRecord("linux/amd64", "2026-08-03T00:00:00Z", 1785715200),
				"linux/arm64": makeRecord("linux/arm64", "2026-08-03T00:00:00Z", 1785715200),
				"noncore":     makeNoncoreRecord(),
			}
			materialization := makeMaterialization()
			if tt.mutate != nil {
				tt.mutate(records)
			}
			if tt.mutateMaterialization != nil {
				tt.mutateMaterialization(materialization)
			}
			for _, platform := range []string{"linux/amd64", "linux/arm64"} {
				if platform == tt.omitPlatform {
					continue
				}
				body, err := json.Marshal(records[platform])
				if err != nil {
					t.Fatal(err)
				}
				archive := writeRuntimeEvidenceArchive(t, []tarEntry{{name: "./resolution.json", body: body}})
				archiveData, err := os.ReadFile(archive)
				if err != nil {
					t.Fatal(err)
				}
				name := "runtime-evidence-" + strings.ReplaceAll(platform, "/", "-") + "-live-test.tar.gz"
				if err := os.WriteFile(filepath.Join(runtimeEvidenceDirectory, name), archiveData, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if !tt.omitNoncore {
				resolutionBody, err := json.Marshal(records["noncore"])
				if err != nil {
					t.Fatal(err)
				}
				materializationBody, err := json.Marshal(materialization)
				if err != nil {
					t.Fatal(err)
				}
				archive := writeRuntimeEvidenceArchive(t, []tarEntry{
					{name: "./resolution.json", body: resolutionBody},
					{name: "./materialization-v2.json", body: materializationBody},
				})
				archiveData, err := os.ReadFile(archive)
				if err != nil {
					t.Fatal(err)
				}
				name := "runtime-evidence-linux-amd64-ci-noncore-multi-package.tar.gz"
				if err := os.WriteFile(filepath.Join(runtimeEvidenceDirectory, name), archiveData, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			runnerTemp := filepath.Join(root, "runner-temp")
			if err := os.MkdirAll(runnerTemp, 0o700); err != nil {
				t.Fatal(err)
			}
			releaseSpecs := tt.releaseSpecs
			if releaseSpecs == "" {
				releaseSpecs = "live-test"
			}
			output, err := runWorkflowShellInDir(t, root, script, map[string]string{
				"MAX_RELEASE_ASSET_BYTES":             strconv.Itoa(1 << 20),
				"MAX_RUNTIME_EVIDENCE_EXPANDED_BYTES": strconv.Itoa(1 << 20),
				"PATH":                                os.Getenv("PATH"),
				"RELEASE_AMD64_ONLY_SPECS":            "ci-noncore-multi-package",
				"RELEASE_SHA":                         strings.Repeat("f", 40),
				"RELEASE_SPECS":                       releaseSpecs,
				"RELEASE_TAG":                         "v0.1.4",
				"RUNNER_TEMP":                         runnerTemp,
			})
			if tt.wantError != "" {
				if err == nil {
					t.Fatalf("invalid runtime evidence accepted\n%s", output)
				}
				if !strings.Contains(string(output), tt.wantError) {
					t.Fatalf("runtime evidence error = %q, want substring %q", output, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid runtime evidence rejected: %v\n%s", err, output)
			}

			data, err := os.ReadFile(filepath.Join(releaseDirectory, "metadata-snapshot.json"))
			if err != nil {
				t.Fatal(err)
			}
			var snapshot struct {
				SchemaVersion    string `json:"schema_version"`
				AcceptedSnapshot struct {
					GeneratedAt     string `json:"generated_at"`
					SourceDateEpoch int64  `json:"source_date_epoch"`
				} `json:"accepted_snapshot"`
				Observations []struct {
					Spec                      string `json:"spec"`
					Platform                  string `json:"platform"`
					GeneratedAt               string `json:"generated_at"`
					CoreGeneratedAtEpoch      int64  `json:"core_generated_at_epoch"`
					ResolutionSourceDateEpoch int64  `json:"resolution_source_date_epoch"`
				} `json:"observations"`
			}
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.SchemaVersion != "dalec-homebrew-release-metadata/v2" {
				t.Fatalf("metadata snapshot schema = %q", snapshot.SchemaVersion)
			}
			if got, want := snapshot.AcceptedSnapshot.GeneratedAt, "2026-08-03T00:00:00Z"; got != want {
				t.Fatalf("accepted snapshot generated_at = %q, want %q", got, want)
			}
			if snapshot.AcceptedSnapshot.SourceDateEpoch != 1785715200 {
				t.Fatalf("accepted snapshot epoch = %d", snapshot.AcceptedSnapshot.SourceDateEpoch)
			}
			if len(snapshot.Observations) != 3 ||
				snapshot.Observations[0].Spec != "live-test" || snapshot.Observations[0].Platform != "linux/amd64" ||
				snapshot.Observations[1].Spec != "live-test" || snapshot.Observations[1].Platform != "linux/arm64" ||
				snapshot.Observations[2].Spec != "ci-noncore-multi-package" || snapshot.Observations[2].Platform != "linux/amd64" {
				t.Fatalf("metadata observations are not deterministic: %+v", snapshot.Observations)
			}
			if got, want := snapshot.Observations[1].GeneratedAt, coreSource(records["linux/arm64"])["generated_at"]; got != want {
				t.Fatalf("arm64 metadata observation = %q, want %q", got, want)
			}
		})
	}
}

func TestReleaseWorkflowResolutionBindingFilter(t *testing.T) {
	filter := releaseWorkflowShellFilter(t, "resolution_binding_filter")
	manifest := writeWorkflowFixture(t, "components.json", []byte(`{
		"policy_version":"homebrew-runtime-v2",
		"tap_policy_digest":"sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"executable_runtime_policy_digest":"sha256:2222222222222222222222222222222222222222222222222222222222222222",
		"supported_catalog_policy_versions":["catalog-v1"],
		"supported_fetch_policy_versions":["fetch-v1"],
		"supported_provenance_policy_versions":["provenance-v1"]
	}`))
	inputs := writeWorkflowFixture(t, "inputs.json", []byte(`{
		"homebrew_commit":"homebrew",
		"portable_ruby_version":"ruby",
		"verification_keys_digest":"keys",
		"dalec_module":"dalec",
		"buildkit_module":"buildkit"
	}`))
	frontend := "registry.example/frontend@sha256:" + strings.Repeat("a", 64)
	frontendIndex := "registry.example/frontend@sha256:" + strings.Repeat("d", 64)
	runtimeBase := "registry.example/runtime-base@sha256:" + strings.Repeat("b", 64)
	materializer := "registry.example/materializer@sha256:" + strings.Repeat("c", 64)
	bottleFetcher := "registry.example/bottle-fetcher@sha256:" + strings.Repeat("e", 64)
	catalogExtractor := "registry.example/catalog-extractor@sha256:" + strings.Repeat("f", 64)

	tests := []struct {
		name     string
		platform map[string]any
		want     bool
	}{
		{name: "absent variant", platform: map[string]any{"os": "linux", "architecture": "amd64"}, want: true},
		{name: "empty variant", platform: map[string]any{"os": "linux", "architecture": "amd64", "variant": ""}, want: true},
		{name: "null variant", platform: map[string]any{"os": "linux", "architecture": "amd64", "variant": nil}},
		{name: "boolean variant", platform: map[string]any{"os": "linux", "architecture": "amd64", "variant": false}},
		{name: "nonempty variant", platform: map[string]any{"os": "linux", "architecture": "amd64", "variant": "v8"}},
		{name: "extra identity key", platform: map[string]any{"os": "linux", "architecture": "amd64", "os.version": "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := map[string]any{
				"schema_version": "dalec-homebrew-resolution/v2",
				"input":          map[string]any{"platform": tt.platform},
				"policy_version": "homebrew-runtime-v2",
				"components": map[string]any{
					"frontend_index_ref":                   frontendIndex,
					"frontend_ref":                         frontend,
					"runtime_base_ref":                     runtimeBase,
					"materializer_ref":                     materializer,
					"bottle_fetcher_ref":                   bottleFetcher,
					"catalog_extractor_ref":                catalogExtractor,
					"catalog_service_origin":               "",
					"ingestion_jws_key_policy_digest":      "",
					"tap_policy_digest":                    "sha256:" + strings.Repeat("1", 64),
					"executable_runtime_policy_digest":     "sha256:" + strings.Repeat("2", 64),
					"homebrew_commit":                      "homebrew",
					"ruby_runtime":                         "ruby",
					"verification_keys_digest":             "keys",
					"dalec_module":                         "dalec",
					"buildkit_module":                      "buildkit",
					"supported_catalog_policy_versions":    []any{"catalog-v1"},
					"supported_fetch_policy_versions":      []any{"fetch-v1"},
					"supported_provenance_policy_versions": []any{"provenance-v1"},
				},
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(
				"jq", "-e",
				"--arg", "platform", "linux/amd64",
				"--arg", "frontend_index", frontendIndex,
				"--arg", "frontend", frontend,
				"--arg", "runtime_base", runtimeBase,
				"--arg", "materializer", materializer,
				"--arg", "bottle_fetcher", bottleFetcher,
				"--arg", "catalog_extractor", catalogExtractor,
				"--slurpfile", "manifest", manifest,
				"--slurpfile", "inputs", inputs,
				filter,
			)
			cmd.Stdin = bytes.NewReader(data)
			output, err := cmd.CombinedOutput()
			if (err == nil) != tt.want {
				t.Fatalf("binding filter success = %v, want %v\n%s", err == nil, tt.want, output)
			}
		})
	}
}

func TestReleaseWorkflowVulnerabilityEvidencePolicy(t *testing.T) {
	workflow := workflowYAML(t, "release.yml")
	scanStep := workflowStepByName(t, workflow, "evidence", "Generate fixed critical vulnerability report")
	if _, ok := yamlMappingLookup(t, scanStep, "continue-on-error"); ok {
		t.Fatal("vulnerability scanner must not use continue-on-error")
	}
	scanScript := yamlScalarValue(t, yamlMappingValue(t, scanStep, "run"))
	for _, contract := range []string{
		`--format json`,
		`--output "/evidence/vulnerability-${SLUG}.json"`,
		`--severity CRITICAL`,
		`--ignore-unfixed`,
		`--exit-code 0`,
	} {
		if !strings.Contains(scanScript, contract) {
			t.Fatalf("vulnerability scanner is missing %q", contract)
		}
	}
	if strings.Contains(scanScript, "--exit-code 1") || strings.Contains(scanScript, "continue-on-error") || strings.Contains(scanScript, "|| true") {
		t.Fatal("vulnerability scanner masks findings or operational failures")
	}

	summaryStep := workflowStepByName(t, workflow, "evidence", "Summarize fixed critical vulnerability findings")
	if _, ok := yamlMappingLookup(t, summaryStep, "continue-on-error"); ok {
		t.Fatal("vulnerability summary must not use continue-on-error")
	}
	summaryScript := yamlScalarValue(t, yamlMappingValue(t, summaryStep, "run"))
	for _, contract := range []string{
		`test -s "$report"`,
		`jq -c -e --arg subject "$SUBJECT"`,
		`sort_by([.id, .package, .installed, .fixed, .severity, .target])`,
		`--argjson field_limit "$summary_field_limit"`,
		`Vulnerability findings are release evidence and do not block signing or promotion.`,
	} {
		if !strings.Contains(summaryScript, contract) {
			t.Fatalf("vulnerability summary is missing %q", contract)
		}
	}

	uploadStep := workflowStepByName(t, workflow, "evidence", "Upload evidence")
	if got := yamlScalarValue(t, yamlMappingValue(t, uploadStep, "if")); got != "always()" {
		t.Fatalf("evidence upload condition = %q, want always()", got)
	}
	uploadWith := yamlMappingValue(t, uploadStep, "with")
	if got := yamlScalarValue(t, yamlMappingValue(t, uploadWith, "path")); got != "evidence" {
		t.Fatalf("evidence upload path = %q, want evidence", got)
	}

	expected := "registry.example/component@sha256:" + strings.Repeat("d", 64)
	runSummary := func(t *testing.T, report []byte) (string, error) {
		t.Helper()
		tmp := t.TempDir()
		evidence := filepath.Join(tmp, "evidence")
		if err := os.Mkdir(evidence, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(evidence, "vulnerability-component-linux-amd64.json"), report, 0o600); err != nil {
			t.Fatal(err)
		}
		summary := filepath.Join(tmp, "summary.md")
		output, err := runWorkflowShell(t, "cd \"$WORKDIR\"\n"+summaryScript, map[string]string{
			"COMPONENT":               "component",
			"GITHUB_STEP_SUMMARY":     summary,
			"MAX_RELEASE_ASSET_BYTES": "1048576",
			"PATH":                    os.Getenv("PATH"),
			"PLATFORM":                "linux/amd64",
			"RUNNER_TEMP":             tmp,
			"SLUG":                    "component-linux-amd64",
			"SUBJECT":                 expected,
			"WORKDIR":                 tmp,
		})
		if err != nil {
			return string(output), err
		}
		data, err := os.ReadFile(summary)
		if err != nil {
			t.Fatal(err)
		}
		return string(data), nil
	}

	longPackage := strings.Repeat("x", 300)
	findingReport := validVulnerabilityReport(expected, map[string]any{
		"Target": "portable-ruby|`<>&\ntarget",
		"Vulnerabilities": []any{
			map[string]any{
				"VulnerabilityID":  "CVE-2026-0002",
				"PkgName":          "pkg|`<>&\nname",
				"InstalledVersion": "1.0",
				"FixedVersion":     "2.0",
				"Severity":         "CRITICAL",
			},
			map[string]any{
				"VulnerabilityID":  "CVE-2026-0001",
				"PkgName":          longPackage,
				"InstalledVersion": "3.0",
				"FixedVersion":     "4.0",
				"Severity":         "CRITICAL",
			},
		},
	})
	findingData, err := json.Marshal(findingReport)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := runSummary(t, findingData)
	if err != nil {
		t.Fatalf("well-formed findings failed summary: %v\n%s", err, summary)
	}
	markdown := func(value string) string {
		var escaped strings.Builder
		for _, codepoint := range value {
			if codepoint < 32 ||
				(codepoint >= 127 && codepoint <= 159) ||
				(codepoint >= 8234 && codepoint <= 8238) ||
				(codepoint >= 8294 && codepoint <= 8297) {
				codepoint = ' '
			}
			escaped.WriteString("&#")
			escaped.WriteString(strconv.Itoa(int(codepoint)))
			escaped.WriteByte(';')
		}
		return escaped.String()
	}
	for _, contract := range []string{
		"Fixed critical findings: **2**",
		markdown("pkg|`<>&\nname"),
		markdown("portable-ruby|`<>&\ntarget"),
		markdown(strings.Repeat("x", 256) + "…"),
		"Summary fields are limited to 256 characters",
		"Vulnerability findings are release evidence and do not block signing or promotion.",
	} {
		if !strings.Contains(summary, contract) {
			t.Fatalf("vulnerability summary is missing %q\n%s", contract, summary)
		}
	}
	first := strings.Index(summary, markdown("CVE-2026-0001"))
	second := strings.Index(summary, markdown("CVE-2026-0002"))
	if first < 0 || second < 0 || first >= second {
		t.Fatalf("vulnerability findings are not deterministically ordered\n%s", summary)
	}
	if strings.Contains(summary, "pkg|`<>&\nname") || strings.Contains(summary, "portable-ruby|`<>&\ntarget") {
		t.Fatalf("vulnerability summary contains unescaped scanner output\n%s", summary)
	}
	if strings.Contains(summary, markdown(longPackage)) {
		t.Fatalf("vulnerability summary contains an unbounded scanner field\n%s", summary)
	}

	if output, err := runSummary(t, []byte(`{"SchemaVersion":`)); err == nil {
		t.Fatalf("malformed vulnerability report was summarized\n%s", output)
	}
	wrongSchema := validVulnerabilityReport(expected, map[string]any{"Target": "ubuntu", "Vulnerabilities": []any{}})
	wrongSchema["SchemaVersion"] = 1
	wrongSchemaData, err := json.Marshal(wrongSchema)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := runSummary(t, wrongSchemaData); err == nil {
		t.Fatalf("wrong vulnerability schema was summarized\n%s", output)
	}
}

func TestReleaseWorkflowVulnerabilityReportValidation(t *testing.T) {
	script := releaseWorkflowPython(t, "PYVULN")
	expected := "registry.example/component@sha256:" + strings.Repeat("d", 64)
	valid := []map[string]any{
		validVulnerabilityReport(expected, map[string]any{"Target": "ubuntu", "Vulnerabilities": nil}),
		validVulnerabilityReport(expected, map[string]any{"Target": "ubuntu", "Vulnerabilities": []any{}}),
		validVulnerabilityReport(expected, map[string]any{"Target": "ubuntu", "Vulnerabilities": []any{map[string]any{"VulnerabilityID": "CVE-1"}}}),
		validVulnerabilityReport(expected, map[string]any{"Target": "application"}),
	}
	for i, report := range valid {
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		path := writeWorkflowFixture(t, "clean.json", data)
		if output, err := runWorkflowPython(t, script, path, expected); err != nil {
			t.Fatalf("valid report %d rejected: %v\n%s", i, err, output)
		}
	}

	invalid := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "empty results", mutate: func(report map[string]any) { report["Results"] = []any{} }},
		{name: "malformed results", mutate: func(report map[string]any) { report["Results"] = map[string]any{} }},
		{name: "missing target", mutate: func(report map[string]any) { report["Results"] = []any{map[string]any{"Vulnerabilities": []any{}}} }},
		{name: "malformed vulnerabilities", mutate: func(report map[string]any) {
			report["Results"] = []any{map[string]any{"Target": "ubuntu", "Vulnerabilities": map[string]any{}}}
		}},
		{name: "malformed vulnerability", mutate: func(report map[string]any) {
			report["Results"] = []any{map[string]any{"Target": "ubuntu", "Vulnerabilities": []any{"CVE-1"}}}
		}},
		{name: "wrong schema", mutate: func(report map[string]any) { report["SchemaVersion"] = 1 }},
		{name: "wrong artifact name", mutate: func(report map[string]any) {
			report["ArtifactName"] = "registry.example/other@sha256:" + strings.Repeat("e", 64)
		}},
		{name: "wrong artifact type", mutate: func(report map[string]any) { report["ArtifactType"] = "filesystem" }},
		{name: "missing repo digest", mutate: func(report map[string]any) { report["Metadata"] = map[string]any{"RepoDigests": []any{}} }},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			report := validVulnerabilityReport(expected, map[string]any{"Target": "ubuntu", "Vulnerabilities": []any{}})
			tt.mutate(report)
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			path := writeWorkflowFixture(t, "report.json", data)
			if output, err := runWorkflowPython(t, script, path, expected); err == nil {
				t.Fatalf("invalid report accepted\n%s", output)
			}
		})
	}

	t.Run("duplicate", func(t *testing.T) {
		data := []byte(`{"SchemaVersion":2,"ArtifactName":"` + expected + `","ArtifactName":"` + expected + `","ArtifactType":"container_image","Metadata":{"RepoDigests":["` + expected + `"]},"Results":[{"Target":"ubuntu","Vulnerabilities":[]}]}`)
		path := writeWorkflowFixture(t, "report.json", data)
		if output, err := runWorkflowPython(t, script, path, expected); err == nil {
			t.Fatalf("duplicate report accepted\n%s", output)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		path := writeWorkflowFixture(t, "report.json", bytes.Repeat([]byte(" "), (1<<20)+1))
		if output, err := runWorkflowPython(t, script, path, expected); err == nil {
			t.Fatalf("oversized report accepted\n%s", output)
		}
	})
}

func validVulnerabilityReport(subject string, result map[string]any) map[string]any {
	return map[string]any{
		"SchemaVersion": 2,
		"ArtifactName":  subject,
		"ArtifactType":  "container_image",
		"Metadata":      map[string]any{"RepoDigests": []any{subject}},
		"Results":       []any{result},
	}
}

func releaseWorkflowShellFilter(t *testing.T, variable string) string {
	t.Helper()
	workflow := releaseWorkflowText(t)
	startToken := "          " + variable + "='\n"
	start := strings.Index(workflow, startToken)
	if start < 0 {
		t.Fatalf("workflow shell filter %s is absent", variable)
	}
	body := workflow[start+len(startToken):]
	endToken := "\n          '\n"
	end := strings.Index(body, endToken)
	if end < 0 {
		t.Fatalf("workflow shell filter %s is unterminated", variable)
	}
	lines := strings.Split(body[:end], "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "            ") {
			t.Fatalf("workflow jq line is not indented as expected: %q", line)
		}
		lines[i] = strings.TrimPrefix(line, "            ")
	}
	return strings.Join(lines, "\n")
}

func workflowYAML(t *testing.T, name string) *yaml.Node {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github", "workflows", name))
	if err != nil {
		t.Fatal(err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 {
		t.Fatalf("%s root YAML kind = %v with %d children, want one document", name, document.Kind, len(document.Content))
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		t.Fatalf("%s root YAML kind = %v, want mapping", name, root.Kind)
	}
	return root
}

func yamlMappingLookup(t *testing.T, mapping *yaml.Node, key string) (*yaml.Node, bool) {
	t.Helper()
	if mapping.Kind != yaml.MappingNode || len(mapping.Content)%2 != 0 {
		t.Fatalf("YAML node kind = %v with %d children, want mapping", mapping.Kind, len(mapping.Content))
	}
	for index := 0; index < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1], true
		}
	}
	return nil, false
}

func yamlMappingValue(t *testing.T, mapping *yaml.Node, key string) *yaml.Node {
	t.Helper()
	value, ok := yamlMappingLookup(t, mapping, key)
	if !ok {
		t.Fatalf("YAML mapping does not contain %q", key)
	}
	return value
}

func requireYAMLMappingKeys(t *testing.T, mapping *yaml.Node, want ...string) {
	t.Helper()
	if mapping.Kind != yaml.MappingNode || len(mapping.Content)%2 != 0 {
		t.Fatalf("YAML node kind = %v with %d children, want mapping", mapping.Kind, len(mapping.Content))
	}
	got := make([]string, 0, len(mapping.Content)/2)
	for index := 0; index < len(mapping.Content); index += 2 {
		got = append(got, mapping.Content[index].Value)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("YAML mapping keys = %v, want %v", got, want)
	}
}

func yamlScalarValue(t *testing.T, node *yaml.Node) string {
	t.Helper()
	if node.Kind != yaml.ScalarNode {
		t.Fatalf("YAML node kind = %v, want scalar", node.Kind)
	}
	return node.Value
}

func yamlOptionalScalar(t *testing.T, mapping *yaml.Node, key string) string {
	t.Helper()
	value, ok := yamlMappingLookup(t, mapping, key)
	if !ok {
		return ""
	}
	return yamlScalarValue(t, value)
}

func yamlStringSequence(t *testing.T, sequence *yaml.Node) []string {
	t.Helper()
	if sequence.Kind != yaml.SequenceNode {
		t.Fatalf("YAML node kind = %v, want sequence", sequence.Kind)
	}
	values := make([]string, 0, len(sequence.Content))
	for _, node := range sequence.Content {
		values = append(values, yamlScalarValue(t, node))
	}
	return values
}

func requireYAMLStringSequence(t *testing.T, sequence *yaml.Node, want ...string) {
	t.Helper()
	got := yamlStringSequence(t, sequence)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("YAML sequence = %v, want %v", got, want)
	}
}

func workflowStepByName(t *testing.T, workflow *yaml.Node, jobName, stepName string) *yaml.Node {
	t.Helper()
	jobs := yamlMappingValue(t, workflow, "jobs")
	job := yamlMappingValue(t, jobs, jobName)
	steps := yamlMappingValue(t, job, "steps")
	if steps.Kind != yaml.SequenceNode {
		t.Fatalf("workflow job %q steps YAML kind = %v, want sequence", jobName, steps.Kind)
	}
	for _, step := range steps.Content {
		if yamlOptionalScalar(t, step, "name") == stepName {
			return step
		}
	}
	t.Fatalf("workflow job %q does not contain step %q", jobName, stepName)
	return nil
}

func readWorkflowOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			t.Fatalf("invalid workflow output line %q", line)
		}
		if _, exists := outputs[key]; exists {
			t.Fatalf("duplicate workflow output %q", key)
		}
		outputs[key] = value
	}
	return outputs
}

func provenanceRunValidationShell(t *testing.T, script string) string {
	t.Helper()
	startToken := `run_json=$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$run_id/attempts/$run_attempt")`
	start := strings.Index(script, startToken)
	if start < 0 {
		t.Fatal("release workflow does not fetch the provenance workflow run")
	}
	validation := script[start:]
	endToken := `' <<<"$run_json" >/dev/null`
	end := strings.Index(validation, endToken)
	if end < 0 {
		t.Fatal("release workflow provenance run validation is unterminated")
	}
	return validation[:end+len(endToken)] + "\n"
}

func runWorkflowShell(t *testing.T, script string, environment map[string]string) ([]byte, error) {
	t.Helper()
	return runWorkflowShellInDir(t, "", script, environment)
}

func runWorkflowShellInDir(t *testing.T, directory, script string, environment map[string]string) ([]byte, error) {
	t.Helper()
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	variables := make([]string, 0, len(keys))
	for _, key := range keys {
		variables = append(variables, key+"="+environment[key])
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = directory
	cmd.Env = variables
	return cmd.CombinedOutput()
}

func requireArgument(t *testing.T, arguments []string, want string) {
	t.Helper()
	for _, argument := range arguments {
		if argument == want {
			return
		}
	}
	t.Fatalf("arguments %q do not contain %q", arguments, want)
}

func requireArgumentSequence(t *testing.T, arguments []string, want ...string) {
	t.Helper()
	for start := 0; start+len(want) <= len(arguments); start++ {
		if strings.Join(arguments[start:start+len(want)], "\n") == strings.Join(want, "\n") {
			return
		}
	}
	t.Fatalf("arguments %q do not contain sequence %q", arguments, want)
}

func releaseWorkflowText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func releaseWorkflowPython(t *testing.T, marker string) string {
	t.Helper()
	workflow := releaseWorkflowText(t)
	startToken := "<<'" + marker + "'\n"
	start := strings.Index(workflow, startToken)
	if start < 0 {
		t.Fatalf("workflow Python marker %s is absent", marker)
	}
	body := workflow[start+len(startToken):]
	endToken := "\n          " + marker
	end := strings.Index(body, endToken)
	if end < 0 {
		t.Fatalf("workflow Python marker %s is unterminated", marker)
	}
	lines := strings.Split(body[:end], "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "          ") {
			t.Fatalf("workflow Python line is not indented as expected: %q", line)
		}
		lines[i] = strings.TrimPrefix(line, "          ")
	}
	return strings.Join(lines, "\n") + "\n"
}

func runWorkflowPython(t *testing.T, script string, args ...string) ([]byte, error) {
	t.Helper()
	return runWorkflowPythonWithExpandedLimit(t, script, 32<<20, args...)
}

func runWorkflowPythonWithExpandedLimit(t *testing.T, script string, expandedLimit int, args ...string) ([]byte, error) {
	t.Helper()
	scriptPath := writeWorkflowFixture(t, "validator.py", []byte(script))
	cmd := exec.Command("python3", append([]string{scriptPath}, args...)...)
	cmd.Env = append(os.Environ(),
		"MAX_RELEASE_ASSET_BYTES=1048576",
		"MAX_RUNTIME_EVIDENCE_EXPANDED_BYTES="+strconv.Itoa(expandedLimit),
	)
	return cmd.CombinedOutput()
}

func writeWorkflowFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type rawTarEntry struct {
	name     string
	body     []byte
	typeflag byte
	reserved []byte
}

func rawUSTARPayload(t *testing.T, entries []rawTarEntry, zeroBlocks int) []byte {
	t.Helper()
	var payload bytes.Buffer
	for _, entry := range entries {
		if len(entry.name) > 100 {
			t.Fatalf("raw tar fixture name is too long: %q", entry.name)
		}
		header := make([]byte, 512)
		copy(header[0:100], entry.name)
		writeTarOctal(t, header[124:136], int64(len(entry.body)))
		header[156] = entry.typeflag
		copy(header[257:263], []byte("ustar\x00"))
		copy(header[263:265], []byte("00"))
		copy(header[500:512], entry.reserved)
		for i := 148; i < 156; i++ {
			header[i] = ' '
		}
		checksum := int64(0)
		for _, value := range header {
			checksum += int64(value)
		}
		text := strconv.FormatInt(checksum, 8)
		if len(text) > 6 {
			t.Fatalf("raw tar fixture checksum is too large: %s", text)
		}
		copy(header[148+6-len(text):154], text)
		header[154] = 0
		header[155] = ' '
		payload.Write(header)
		payload.Write(entry.body)
		if padding := (-len(entry.body)) & 511; padding > 0 {
			payload.Write(make([]byte, padding))
		}
	}
	payload.Write(make([]byte, zeroBlocks*512))
	return payload.Bytes()
}

func writeTarOctal(t *testing.T, field []byte, value int64) {
	t.Helper()
	text := strconv.FormatInt(value, 8)
	if len(text)+1 > len(field) {
		t.Fatalf("raw tar fixture value %d does not fit", value)
	}
	for i := range field {
		field[i] = '0'
	}
	copy(field[len(field)-1-len(text):len(field)-1], text)
	field[len(field)-1] = 0
}

func singleResolutionTarPayload(t *testing.T, zeroBlocks int) []byte {
	t.Helper()
	var buffer bytes.Buffer
	tw := tar.NewWriter(&buffer)
	body := []byte(`{}`)
	if err := tw.WriteHeader(&tar.Header{Name: "./resolution.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	// Do not call Close: append exactly the requested number of end-marker blocks.
	if padding := (-len(body)) & 511; padding > 0 {
		buffer.Write(make([]byte, padding))
	}
	buffer.Write(make([]byte, zeroBlocks*512))
	return buffer.Bytes()
}

func writeWorkflowGzipPayload(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func concatenateWorkflowFiles(t *testing.T, paths ...string) string {
	t.Helper()
	outputPath := filepath.Join(t.TempDir(), "concatenated.tar.gz")
	output, err := os.Create(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	return outputPath
}

type tarEntry struct {
	name       string
	body       []byte
	typeflag   byte
	linkname   string
	paxRecords map[string]string
}

func writeRuntimeEvidenceArchive(t *testing.T, entries []tarEntry) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime-evidence.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		typeflag := entry.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:       entry.name,
			Mode:       0o644,
			Size:       int64(len(entry.body)),
			Typeflag:   typeflag,
			Linkname:   entry.linkname,
			PAXRecords: entry.paxRecords,
		}
		if typeflag != tar.TypeReg && typeflag != tar.TypeRegA {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tarWriter.Write(entry.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

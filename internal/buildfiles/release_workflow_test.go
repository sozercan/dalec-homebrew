package buildfiles

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	if got := yamlScalarValue(t, yamlMappingValue(t, dispatch, "if")); got != "github.ref_type == 'tag' && startsWith(github.ref_name, 'v')" {
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
	output, err := runWorkflowShell(t, dispatchScript, map[string]string{
		"EXPECTED_SHA":      expectedSHA,
		"GH_ARGUMENTS":      argumentsPath,
		"GH_TOKEN":          "unused",
		"GITHUB_EVENT_NAME": "push",
		"GITHUB_REF":        "refs/tags/v1.2.3",
		"GITHUB_REF_TYPE":   "tag",
		"GITHUB_REPOSITORY": "sozercan/dalec-homebrew",
		"PATH":              temporary + string(os.PathListSeparator) + os.Getenv("PATH"),
		"RELEASE_TAG":       "v1.2.3",
	})
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

func TestReleaseWorkflowMetadataFilter(t *testing.T) {
	filter := releaseWorkflowShellFilter(t, "metadata_filter")
	base := validResolutionMetadataRecord()
	baseOutput := runWorkflowJQ(t, filter, base)

	nonIdentityDrift := cloneJSONRecord(t, base)
	metadata := nonIdentityDrift["metadata"].(map[string]any)
	metadata["fetched_at"] = "2026-08-03T00:01:00Z"
	metadata["signatures"] = []any{map[string]any{"key_id": "different"}}
	if output := runWorkflowJQ(t, filter, nonIdentityDrift); !bytes.Equal(baseOutput, output) {
		t.Fatalf("non-identity metadata changed the normalized snapshot:\nbase: %s\nnew:  %s", baseOutput, output)
	}

	identityDrift := cloneJSONRecord(t, base)
	identityDrift["metadata"].(map[string]any)["generated_at"] = "2026-08-03T00:02:00Z"
	if output := runWorkflowJQ(t, filter, identityDrift); bytes.Equal(baseOutput, output) {
		t.Fatal("identity metadata drift did not change the normalized snapshot")
	}

	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "unknown field",
			mutate: func(record map[string]any) {
				record["metadata"].(map[string]any)["unexpected"] = true
			},
		},
		{
			name: "missing fetched at",
			mutate: func(record map[string]any) {
				delete(record["metadata"].(map[string]any), "fetched_at")
			},
		},
		{
			name: "malformed digest",
			mutate: func(record map[string]any) {
				record["metadata"].(map[string]any)["digest"] = "sha256:not-a-digest"
			},
		},
		{
			name: "malformed signatures",
			mutate: func(record map[string]any) {
				record["metadata"].(map[string]any)["formula_signatures"] = map[string]any{}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			record := cloneJSONRecord(t, base)
			tt.mutate(record)
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("jq", "-S", "-e", filter)
			cmd.Stdin = bytes.NewReader(data)
			if output, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("invalid metadata accepted\n%s", output)
			}
		})
	}
}

func TestReleaseWorkflowResolutionBindingFilter(t *testing.T) {
	filter := releaseWorkflowShellFilter(t, "resolution_binding_filter")
	manifest := writeWorkflowFixture(t, "components.json", []byte(`{"policy_version":"policy-v1"}`))
	inputs := writeWorkflowFixture(t, "inputs.json", []byte(`{
		"homebrew_commit":"homebrew",
		"portable_ruby_version":"ruby",
		"verification_keys_digest":"keys",
		"dalec_module":"dalec",
		"buildkit_module":"buildkit"
	}`))
	frontend := "registry.example/frontend@sha256:" + strings.Repeat("a", 64)
	runtimeBase := "registry.example/runtime-base@sha256:" + strings.Repeat("b", 64)
	materializer := "registry.example/materializer@sha256:" + strings.Repeat("c", 64)

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
				"schema_version": "dalec-homebrew-resolution/v1",
				"input":          map[string]any{"platform": tt.platform},
				"policy_version": "policy-v1",
				"components": map[string]any{
					"frontend_ref":             frontend,
					"runtime_base_ref":         runtimeBase,
					"materializer_ref":         materializer,
					"homebrew_commit":          "homebrew",
					"ruby_runtime":             "ruby",
					"verification_keys_digest": "keys",
					"dalec_module":             "dalec",
					"buildkit_module":          "buildkit",
				},
			}
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(
				"jq", "-e",
				"--arg", "platform", "linux/amd64",
				"--arg", "frontend", frontend,
				"--arg", "runtime_base", runtimeBase,
				"--arg", "materializer", materializer,
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

func TestReleaseWorkflowVulnerabilityGate(t *testing.T) {
	script := releaseWorkflowPython(t, "PYVULN")
	expected := "registry.example/component@sha256:" + strings.Repeat("d", 64)
	valid := []map[string]any{
		validVulnerabilityReport(expected, map[string]any{"Target": "ubuntu", "Vulnerabilities": nil}),
		validVulnerabilityReport(expected, map[string]any{"Target": "ubuntu", "Vulnerabilities": []any{}}),
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
		{
			name: "finding",
			mutate: func(report map[string]any) {
				report["Results"] = []any{map[string]any{"Target": "ubuntu", "Vulnerabilities": []any{map[string]any{"VulnerabilityID": "CVE-1"}}}}
			},
		},
		{name: "empty results", mutate: func(report map[string]any) { report["Results"] = []any{} }},
		{name: "malformed results", mutate: func(report map[string]any) { report["Results"] = map[string]any{} }},
		{name: "missing target", mutate: func(report map[string]any) { report["Results"] = []any{map[string]any{"Vulnerabilities": []any{}}} }},
		{name: "malformed vulnerabilities", mutate: func(report map[string]any) {
			report["Results"] = []any{map[string]any{"Target": "ubuntu", "Vulnerabilities": map[string]any{}}}
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

func validResolutionMetadataRecord() map[string]any {
	digest := "sha256:" + strings.Repeat("a", 64)
	return map[string]any{
		"schema_version": "dalec-homebrew-resolution/v1",
		"metadata": map[string]any{
			"digest":                     digest,
			"formula_digest":             digest,
			"migration_digest":           digest,
			"formula_envelope_digest":    digest,
			"migration_envelope_digest":  digest,
			"formula_freshness_source":   "signed-payload",
			"migration_freshness_source": "http-last-modified",
			"generated_at":               "2026-08-03T00:00:00Z",
			"fetched_at":                 "2026-08-03T00:00:01Z",
			"formula_url":                "https://formulae.brew.sh/api/formula.jws.json",
			"migration_url":              "https://formulae.brew.sh/api/formula_renames.jws.json",
			"signatures":                 []any{},
			"formula_signatures":         []any{},
			"migration_signatures":       []any{},
		},
	}
}

func cloneJSONRecord(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func runWorkflowJQ(t *testing.T, filter string, value map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("jq", "-S", "-e", filter)
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("workflow jq filter failed: %v\n%s", err, output)
	}
	return output
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

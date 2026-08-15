package runtimefs

import (
	"slices"
	"strings"
	"testing"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

func TestValidateInventoryPolicyBindsV2Attribution(t *testing.T) {
	dep := inventoryVerifierNode("dep", "homebrew/core/dep", "homebrew/core/dep")
	requested := inventoryVerifierNode("requested", "homebrew/core/requested", "homebrew/core/requested")
	other := inventoryVerifierNode("other", "homebrew/core/other", "homebrew/core/other")
	nodes := []resolution.Node{dep, requested, other}
	record := inventoryVerifierRecord(resolution.PolicyVersionV2, nodes)
	policy := inventoryVerifierPolicy(nodes, true, "requested")

	for name, entry := range map[string]InventoryEntry{
		"claims requested package": inventoryVerifierEntry("Cellar/dep/1/include/dep.h", TypeRegular, requested.Name, requested.FullName),
		"claims other package":     inventoryVerifierEntry("Cellar/dep/1/include/dep.h", TypeRegular, other.Name, other.FullName),
		"wrong formula identity":   inventoryVerifierEntry("Cellar/dep/1/include/dep.h", TypeRegular, dep.Name, requested.FullName),
		"missing formula identity": inventoryVerifierEntry("Cellar/dep/1/include/dep.h", TypeRegular, dep.Name, ""),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateInventoryPolicy(entry, record, policy, nil); errorCode(err) != CodeVerification {
				t.Fatalf("error=%v code=%s, want %s", err, errorCode(err), CodeVerification)
			}
		})
	}

	transitive := inventoryVerifierEntry("Cellar/dep/1/include/dep.h", TypeRegular, dep.Name, dep.FullName)
	if err := validateInventoryPolicy(transitive, record, policy, nil); errorCode(err) != CodeVerification {
		t.Fatalf("correctly attributed transitive header error=%v code=%s, want %s", err, errorCode(err), CodeVerification)
	}
	unattributedDirectory := inventoryVerifierEntry("Cellar/dep/1/include", TypeDirectory, "", "")
	if err := validateInventoryPolicy(unattributedDirectory, record, policy, nil); errorCode(err) != CodeUnattributed {
		t.Fatalf("unattributed Cellar directory error=%v code=%s, want %s", err, errorCode(err), CodeUnattributed)
	}

	requestedHeader := inventoryVerifierEntry("Cellar/requested/1/include/requested.h", TypeRegular, requested.Name, requested.FullName)
	if err := validateInventoryPolicy(requestedHeader, record, policy, nil); err != nil {
		t.Fatalf("requested header rejected: %v", err)
	}

	global := inventoryVerifierEntry("lib/libdep.so", TypeRegular, dep.Name, dep.FullName)
	if err := validateInventoryPolicy(global, record, policy, nil); err != nil {
		t.Fatalf("globally attributed V2 entry rejected: %v", err)
	}
	global.FormulaID = ""
	if err := validateInventoryPolicy(global, record, policy, nil); errorCode(err) != CodeVerification {
		t.Fatalf("global missing formula error=%v code=%s, want %s", err, errorCode(err), CodeVerification)
	}
}

func TestValidateInventoryPolicyUsesCanonicalFullNameAndPreservesV1(t *testing.T) {
	legacy := inventoryVerifierNode("legacy", "homebrew/core/legacy", "")
	v1Record := inventoryVerifierRecord(resolution.PolicyVersionV1, []resolution.Node{legacy})
	v1Policy := inventoryVerifierPolicy([]resolution.Node{legacy}, false, "legacy")
	v1Entry := inventoryVerifierEntry("Cellar/legacy/1/include/legacy.h", TypeRegular, legacy.Name, "")
	if err := validateInventoryPolicy(v1Entry, v1Record, v1Policy, nil); err != nil {
		t.Fatalf("legacy V1 inventory rejected: %v", err)
	}
	v1Entry.FormulaID = legacy.FullName
	if err := validateInventoryPolicy(v1Entry, v1Record, v1Policy, nil); errorCode(err) != CodeVerification {
		t.Fatalf("V1 formula identity error=%v code=%s, want %s", err, errorCode(err), CodeVerification)
	}

	renamed := inventoryVerifierNode("widget", "acme/tools/widget-renamed", "acme/tools/widget")
	v2Record := inventoryVerifierRecord(resolution.PolicyVersionV2, []resolution.Node{renamed})
	v2Policy := inventoryVerifierPolicy([]resolution.Node{renamed}, false, "widget")
	v2Entry := inventoryVerifierEntry("bin/widget", TypeRegular, renamed.Name, renamed.FullName)
	if err := validateInventoryPolicy(v2Entry, v2Record, v2Policy, nil); err != nil {
		t.Fatalf("canonical V2 full-name attribution rejected: %v", err)
	}
	v2Entry.FormulaID = renamed.PolicyFormulaID
	if err := validateInventoryPolicy(v2Entry, v2Record, v2Policy, nil); errorCode(err) != CodeVerification {
		t.Fatalf("policy identity substituted for canonical full name: error=%v code=%s", err, errorCode(err))
	}
}

func TestPlanMinimalInventoryProfileRejectsMatchingGlobalAliases(t *testing.T) {
	dep := inventoryVerifierNode("dep", "homebrew/core/dep", "homebrew/core/dep")
	policy := inventoryVerifierPolicy([]resolution.Node{dep}, true)
	target := inventoryVerifierEntry("Cellar/dep/1/lib/pkgconfig/dep.pc", TypeRegular, dep.Name, dep.FullName)
	target.SHA256, target.Size = strings.Repeat("a", 64), 4
	protected := inventoryVerifierEntry("Cellar/dep/1/libexec/config/dep.pc", TypeSymlink, dep.Name, dep.FullName)
	protected.LinkTarget = "../../lib/pkgconfig/dep.pc"
	copy := inventoryVerifierEntry("lib/pkgconfig/dep.pc", TypeRegular, dep.Name, dep.FullName)
	copy.SHA256, copy.Size = target.SHA256, target.Size
	alias := inventoryVerifierEntry("share/cmake/dep.pc", TypeSymlink, dep.Name, dep.FullName)
	alias.LinkTarget = "../../lib/pkgconfig/dep.pc"

	// Put the symlink before its matching regular copy. Classification must not
	// depend on the caller's order.
	entries := []InventoryEntry{alias, protected, copy, target}
	plan, err := planMinimalInventoryProfile(entries, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, forbidden := plan.forbidden[inventoryEntryAttribution(target)]; forbidden {
		t.Fatal("protected target remained forbidden")
	}
	for _, entry := range []InventoryEntry{copy, alias} {
		if reason := plan.forbidden[inventoryEntryAttribution(entry)]; reason != PruneRuntimeBuild {
			t.Fatalf("%s reason=%q, want %q", entry.Path, reason, PruneRuntimeBuild)
		}
	}

	slices.SortFunc(entries, func(a, b InventoryEntry) int { return strings.Compare(a.Path, b.Path) })
	record := inventoryVerifierRecord(resolution.PolicyVersionV2, []resolution.Node{dep})
	if err := validateInventoryEntriesPolicy(entries, record, policy); errorCode(err) != CodeVerification {
		t.Fatalf("error=%v code=%s, want %s", err, errorCode(err), CodeVerification)
	}
}

func TestPlanMinimalInventoryProfileRetainsProtectedDirectoryClosure(t *testing.T) {
	app := inventoryVerifierNode("app", "homebrew/core/app", "homebrew/core/app")
	dep := inventoryVerifierNode("dep", "homebrew/core/dep", "homebrew/core/dep")
	nodes := []resolution.Node{app, dep}
	policy := inventoryVerifierPolicy(nodes, true, app.Name)

	rootAlias := inventoryVerifierEntry("Cellar/app/1/libexec/runtime-headers", TypeSymlink, app.Name, app.FullName)
	rootAlias.LinkTarget = "../../../dep/1/include/runtime"
	targetDirectory := inventoryVerifierEntry("Cellar/dep/1/include/runtime", TypeDirectory, dep.Name, dep.FullName)
	header := inventoryVerifierEntry("Cellar/dep/1/include/runtime/api.h", TypeRegular, dep.Name, dep.FullName)
	nestedAlias := inventoryVerifierEntry("Cellar/dep/1/include/runtime/config-link", TypeSymlink, dep.Name, dep.FullName)
	nestedAlias.LinkTarget = "../../lib/pkgconfig/dep.pc"
	externalTarget := inventoryVerifierEntry("Cellar/dep/1/lib/pkgconfig/dep.pc", TypeRegular, dep.Name, dep.FullName)

	entries := []InventoryEntry{externalTarget, nestedAlias, header, targetDirectory, rootAlias}
	plan, err := planMinimalInventoryProfile(entries, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []InventoryEntry{targetDirectory, header, nestedAlias, externalTarget} {
		if reason, forbidden := plan.forbidden[inventoryEntryAttribution(entry)]; forbidden {
			t.Fatalf("%s remained forbidden as %q", entry.Path, reason)
		}
	}

	slices.SortFunc(entries, func(a, b InventoryEntry) int { return strings.Compare(a.Path, b.Path) })
	record := inventoryVerifierRecord(resolution.PolicyVersionV2, nodes)
	if err := validateInventoryEntriesPolicy(entries, record, policy); err != nil {
		t.Fatalf("protected directory closure rejected: %v", err)
	}
}

func TestPlanMinimalInventoryProfileRetainsFormulaShareDocAliasTargets(t *testing.T) {
	dep := inventoryVerifierNode("dep", "homebrew/core/dep", "homebrew/core/dep")
	policy := inventoryVerifierPolicy([]resolution.Node{dep}, true)
	headerTarget := inventoryVerifierEntry("Cellar/dep/1/include/api.h", TypeRegular, dep.Name, dep.FullName)
	headerAlias := inventoryVerifierEntry("Cellar/dep/1/share/doc/dep/include/api.h", TypeSymlink, dep.Name, dep.FullName)
	headerAlias.LinkTarget = "../../../../include/api.h"
	archiveTarget := inventoryVerifierEntry("Cellar/dep/1/lib/libdep.a", TypeRegular, dep.Name, dep.FullName)
	archiveAlias := inventoryVerifierEntry("Cellar/dep/1/share/doc/dep/lib/libdep.a", TypeSymlink, dep.Name, dep.FullName)
	archiveAlias.LinkTarget = "../../../../lib/libdep.a"
	entries := []InventoryEntry{headerTarget, headerAlias, archiveTarget, archiveAlias}

	plan, err := planMinimalInventoryProfile(entries, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if reason, forbidden := plan.forbidden[inventoryEntryAttribution(entry)]; forbidden {
			t.Fatalf("%s remained forbidden as %q", entry.Path, reason)
		}
	}

	record := inventoryVerifierRecord(resolution.PolicyVersionV2, []resolution.Node{dep})
	if err := validateInventoryEntriesPolicy(entries, record, policy); err != nil {
		t.Fatalf("Formula share/doc alias closure rejected: %v", err)
	}
}

func TestPlanMinimalInventoryProfileRejectsUnboundedAlias(t *testing.T) {
	dep := inventoryVerifierNode("dep", "homebrew/core/dep", "homebrew/core/dep")
	policy := inventoryVerifierPolicy([]resolution.Node{dep}, true)
	target := inventoryVerifierEntry("Cellar/dep/1/include/dep.h", TypeRegular, dep.Name, dep.FullName)
	alias := inventoryVerifierEntry("Cellar/dep/1/bin/runtime-header", TypeSymlink, dep.Name, dep.FullName)
	alias.LinkTarget = "../include/dep.h"
	entries := []InventoryEntry{alias, target}
	plan, err := planMinimalInventoryProfile(entries, policy)
	if err != nil {
		t.Fatal(err)
	}
	if reason := plan.forbidden[inventoryEntryAttribution(target)]; reason != PruneRuntimeHeaders {
		t.Fatalf("target reason=%q, want %q", reason, PruneRuntimeHeaders)
	}
}

func TestValidateInventoryPolicyRetainsToolchainDevelopmentClosure(t *testing.T) {
	app := inventoryVerifierNode("app", "homebrew/core/app", "homebrew/core/app")
	app.Dependencies = []resolution.Requirement{{Name: "open-mpi"}, {Name: "unrelated"}}
	openMPI := inventoryVerifierNode("open-mpi", "homebrew/core/open-mpi", "homebrew/core/open-mpi")
	openMPI.Dependencies = []resolution.Requirement{{Name: "gcc"}}
	gcc := inventoryVerifierNode("gcc", "homebrew/core/gcc", "homebrew/core/gcc")
	unrelated := inventoryVerifierNode("unrelated", "homebrew/core/unrelated", "homebrew/core/unrelated")
	unrelated.ExecutablePaths = []string{"bin/mpicc"}
	nodes := []resolution.Node{app, openMPI, gcc, unrelated}
	policy := inventoryVerifierPolicy(nodes, true, app.Name)
	record := inventoryVerifierRecord(resolution.PolicyVersionV2, nodes)

	for _, rel := range []string{
		"Cellar/gcc/1/include/gcc.h",
		"Cellar/gcc/1/lib/pkgconfig/gcc.pc",
		"Cellar/gcc/1/lib/libgcc.a",
	} {
		entry := inventoryVerifierEntry(rel, TypeRegular, gcc.Name, gcc.FullName)
		if err := validateInventoryPolicy(entry, record, policy, nil); err != nil {
			t.Fatalf("toolchain development path %q rejected: %v", rel, err)
		}
	}
	unrelatedHeader := inventoryVerifierEntry("Cellar/unrelated/1/include/unrelated.h", TypeRegular, unrelated.Name, unrelated.FullName)
	if err := validateInventoryPolicy(unrelatedHeader, record, policy, nil); errorCode(err) != CodeVerification {
		t.Fatalf("unrelated header error=%v code=%s, want %s", err, errorCode(err), CodeVerification)
	}
	gccManual := inventoryVerifierEntry("Cellar/gcc/1/share/man/man1/gcc.1", TypeRegular, gcc.Name, gcc.FullName)
	if err := validateInventoryPolicy(gccManual, record, policy, nil); errorCode(err) != CodeVerification {
		t.Fatalf("toolchain manual error=%v code=%s, want %s", err, errorCode(err), CodeVerification)
	}
}

func inventoryVerifierNode(name, fullName, policyFormulaID string) resolution.Node {
	return resolution.Node{Name: name, FullName: fullName, PolicyFormulaID: policyFormulaID, PkgVersion: "1"}
}

func inventoryVerifierRecord(policyVersion string, nodes []resolution.Node) *resolution.Record {
	return &resolution.Record{
		PolicyVersion: policyVersion,
		Nodes:         nodes,
		Runtime:       resolution.RuntimePolicy{UID: 1000, GID: 1000},
	}
}

func inventoryVerifierPolicy(nodes []resolution.Node, minimal bool, requested ...string) *normalizedPolicy {
	byName := make(map[string]resolution.Node, len(nodes))
	for _, node := range nodes {
		byName[node.Name] = node
	}
	requestedSet := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		requestedSet[name] = struct{}{}
	}
	allowlist := normalizedAllowlist{Cellar: true, Opt: true, Bin: true, Sbin: true, Lib: true, Share: true}
	if minimal {
		allowlist.PruningProfile = policyv2.RuntimeProfileMinimalV1
		allowlist.PruningRules = policyv2.MinimalV1RuntimePruneRules()
	}
	policy := &normalizedPolicy{
		installPrefix: DefaultInstallPrefix,
		allowlist:     allowlist,
		nodes:         byName,
		requested:     requestedSet,
	}
	policy.toolchainDev = toolchainDevelopmentClosure(byName, allowlist)
	return policy
}

func inventoryVerifierEntry(rel string, entryType EntryType, packageName, formulaID string) InventoryEntry {
	mode := "0444"
	if entryType == TypeDirectory {
		mode = "0555"
	} else if entryType == TypeSymlink {
		mode = "0777"
	}
	return InventoryEntry{
		Path:      rel,
		Type:      entryType,
		Mode:      mode,
		Package:   packageName,
		FormulaID: formulaID,
	}
}

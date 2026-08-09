package policy

import (
	"fmt"
	"path"
	"slices"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
	policyv2 "github.com/sozercan/dalec-homebrew/policy/v2"
)

const (
	gdkPixbufLoadersCachePath = "lib/gdk-pixbuf-2.0/2.10.0/loaders.cache"
	sharedMimeDatabasePath    = "share/mime"
	nodeNPMRuntimePath        = "lib/node_modules/npm"
)

// RuntimeAllowlist is the V1 package-state policy. Code and global links are
// retained for the complete verified closure. Configuration and writable state
// are narrower: only Formula-named subtrees are approved by default. Formulae
// requiring a different etc/var layout fail closed until an explicit policy
// exception is added here and versioned with the release.
func RuntimeAllowlist(record *resolution.Record) (runtimefs.Allowlist, []string) {
	allow := runtimefs.Allowlist{Cellar: true, Opt: true, Bin: true, Sbin: true, Lib: true, Share: true}
	var writable []string
	if record == nil {
		return allow, nil
	}
	for _, node := range record.Nodes {
		allow.Etc = append(allow.Etc,
			runtimefs.PathRule{Path: node.Name, Package: node.Name},
			runtimefs.PathRule{Path: node.Name + ".conf", Package: node.Name},
			runtimefs.PathRule{Path: node.Name + ".d", Package: node.Name},
		)
		if runtimePolicyAllows(node, "shared-etc-fonts", "fontconfig") {
			// Fontconfig's default runtime configuration uses the shared
			// HOMEBREW_PREFIX/etc/fonts layout rather than a formula-named
			// subtree. The files are still verified bottle content and remain
			// root-owned and non-writable in the final runtime.
			allow.Etc = append(allow.Etc, runtimefs.PathRule{
				Path:     "fonts",
				Package:  node.Name,
				Required: true,
			})
		}
		allow.Var = append(allow.Var, runtimefs.PathRule{Path: node.Name, Package: node.Name, Writable: true, Required: true})
		writable = append(writable, path.Join(runtimefs.DefaultInstallPrefix, "var", node.Name))
		if runtimePolicyOwnsGeneratedGlobalPath(node, gdkPixbufLoadersCachePath, "gdk-pixbuf") {
			// Homebrew's authenticated gdk-pixbuf install step generates this
			// runtime module registry below the enabled global lib root. The
			// materializer validates its complete structure and binds every
			// referenced loader back to a keg in the resolved closure.
			allow.Owners = append(allow.Owners, runtimefs.PathRule{
				Path:     gdkPixbufLoadersCachePath,
				Package:  node.Name,
				Required: true,
			})
		}
		if runtimePolicyOwnsGeneratedGlobalPath(node, nodeNPMRuntimePath, "node") {
			// Node's verified post-install step copies its bottled private npm
			// tree into the global lib tree and writes one prefix-bound npmrc.
			// The materializer validates the complete copy before this fallback
			// attribution is applied.
			allow.Owners = append(allow.Owners, runtimefs.PathRule{
				Path:     nodeNPMRuntimePath,
				Package:  node.Name,
				Required: true,
			})
		}
		if runtimePolicyOwnsGeneratedGlobalPath(node, sharedMimeDatabasePath, "shared-mime-info") {
			// update-mime-database expands the verified package XML into a
			// shared runtime database. The materializer validates the complete
			// generated tree and rejects unverified writers before this fallback
			// attribution is applied.
			allow.Owners = append(allow.Owners, runtimefs.PathRule{
				Path:     sharedMimeDatabasePath,
				Package:  node.Name,
				Required: true,
			})
		}
	}
	return allow, writable
}

func runtimePolicyAllows(node resolution.Node, rule, legacyName string) bool {
	if node.PolicyFormulaID != "" {
		return policyv2.HasEmbeddedRule(node.PolicyFormulaID, rule)
	}
	return node.Name == legacyName && node.FullName == "homebrew/core/"+legacyName
}

func runtimePolicyOwnsGeneratedGlobalPath(node resolution.Node, generatedPath, legacyName string) bool {
	if node.PolicyFormulaID != "" {
		return policyv2.HasEmbeddedGeneratedGlobalPath(node.PolicyFormulaID, generatedPath)
	}
	return node.Name == legacyName && node.FullName == "homebrew/core/"+legacyName
}

func BindRuntimePolicy(record *resolution.Record) (runtimefs.Allowlist, error) {
	allow, writable := RuntimeAllowlist(record)
	if len(record.Runtime.WritablePaths) > 0 && !slices.Equal(record.Runtime.WritablePaths, writable) {
		return runtimefs.Allowlist{}, fmt.Errorf("runtime writable paths do not match V1 policy")
	}
	record.Runtime.WritablePaths = writable
	digest, err := runtimefs.PolicyDigest(allow, runtimefs.DefaultInstallPrefix, record.Nodes)
	if err != nil {
		return runtimefs.Allowlist{}, err
	}
	if record.PruningPolicyDigest != "" && record.PruningPolicyDigest != digest {
		return runtimefs.Allowlist{}, fmt.Errorf("pruning policy digest %s does not match V1 policy %s", record.PruningPolicyDigest, digest)
	}
	record.PruningPolicyDigest = digest
	return allow, nil
}

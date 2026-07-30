package policy

import (
	"fmt"
	"path"
	"slices"

	"github.com/sozercan/dalec-homebrew/internal/resolution"
	"github.com/sozercan/dalec-homebrew/internal/runtimefs"
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
		allow.Var = append(allow.Var, runtimefs.PathRule{Path: node.Name, Package: node.Name, Writable: true, Required: true})
		writable = append(writable, path.Join(runtimefs.DefaultInstallPrefix, "var", node.Name))
	}
	return allow, writable
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

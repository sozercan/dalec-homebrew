package runtime

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/project-dalec/dalec"
	"github.com/sozercan/dalec-homebrew/internal/resolution"
)

const (
	Prefix          = "/home/linuxbrew/.linuxbrew"
	DefaultUser     = "linuxbrew"
	DefaultUID      = 1000
	DefaultGID      = 1000
	RuntimeManifest = "/usr/share/dalec-homebrew/manifest.json"
)

type Identity struct {
	User string
	UID  int
	GID  int
}

func BuildImageConfig(base *dalec.DockerImageSpec, img *dalec.ImageConfig, requested []resolution.RequestedRoot, nodes []resolution.Node) (*dalec.DockerImageSpec, Identity, []string, error) {
	if base == nil {
		return nil, Identity{}, nil, errors.New("nil runtime base image configuration")
	}
	out := *base
	out.Config = base.Config
	out.Config.Env = append([]string(nil), base.Config.Env...)
	out.Config.Labels = cloneMap(base.Config.Labels)
	out.Config.Volumes = cloneMap(base.Config.Volumes)

	generated, err := GeneratedPATH(requested, nodes, EnvValue(base.Config.Env, "PATH"))
	if err != nil {
		return nil, Identity{}, nil, err
	}
	userEnv := []string(nil)
	if img != nil {
		userEnv = img.Env
	}
	out.Config.Env = MergeEnv(base.Config.Env, []string{"PATH=" + strings.Join(generated, ":")}, userEnv)
	if EnvValue(out.Config.Env, "PATH") == "" {
		return nil, Identity{}, nil, errors.New("final PATH must not be empty")
	}

	copyForMerge := cloneImageConfig(img)
	if copyForMerge != nil {
		copyForMerge.Env = nil
	}
	if err := dalec.MergeImageConfig(&out.Config, copyForMerge); err != nil {
		return nil, Identity{}, nil, err
	}
	if img == nil || img.User == "" {
		out.Config.User = DefaultUser
	}
	identity, err := ParseIdentity(out.Config.User)
	if err != nil {
		return nil, Identity{}, nil, err
	}
	if out.Config.WorkingDir == "" {
		out.Config.WorkingDir = "/home/linuxbrew"
	}
	return &out, identity, generated, nil
}

func GeneratedPATH(requested []resolution.RequestedRoot, nodes []resolution.Node, basePATH string) ([]string, error) {
	byName := make(map[string]resolution.Node, len(nodes))
	for _, n := range nodes {
		byName[n.Name] = n
	}
	var out []string
	executables := map[string]string{}
	for _, root := range requested {
		n, ok := byName[root.Canonical]
		if !ok {
			return nil, fmt.Errorf("requested root %q is absent from closure", root.Canonical)
		}
		for _, p := range n.ExecutablePaths {
			base := path.Base(p)
			if previous, ok := executables[base]; ok && previous != n.Name {
				return nil, fmt.Errorf("requested executable collision %q between %q and %q", base, previous, n.Name)
			}
			executables[base] = n.Name
		}
		if !n.KegOnly {
			continue
		}
		out = appendUnique(out, Prefix+"/opt/"+n.Name+"/bin", Prefix+"/opt/"+n.Name+"/sbin")
	}
	out = appendUnique(out, Prefix+"/bin", Prefix+"/sbin")
	for _, p := range strings.Split(basePATH, ":") {
		if p != "" {
			out = appendUnique(out, p)
		}
	}
	return out, nil
}

func ParseIdentity(value string) (Identity, error) {
	if value == "" || value == DefaultUser || value == DefaultUser+":"+DefaultUser {
		return Identity{User: DefaultUser, UID: DefaultUID, GID: DefaultGID}, nil
	}
	parts := strings.Split(value, ":")
	if len(parts) > 2 || parts[0] == "" {
		return Identity{}, fmt.Errorf("malformed image.user %q", value)
	}
	uid, err := strconv.Atoi(parts[0])
	if err != nil {
		return Identity{}, fmt.Errorf("unknown named image.user %q; V1 only owns %q", value, DefaultUser)
	}
	if len(parts) == 1 {
		return Identity{}, fmt.Errorf("numeric image.user %q must specify an explicit numeric gid", value)
	}
	gid := 0
	if len(parts) == 2 {
		if parts[1] == "" {
			return Identity{}, fmt.Errorf("malformed image.user %q", value)
		}
		gid, err = strconv.Atoi(parts[1])
		if err != nil {
			return Identity{}, fmt.Errorf("numeric user requires a numeric group in %q", value)
		}
	}
	if uid <= 0 || gid <= 0 {
		return Identity{}, fmt.Errorf("image.user must be non-root, got %q", value)
	}
	return Identity{User: value, UID: uid, GID: gid}, nil
}

// MergeEnv merges by variable key while preserving the first key position.
// Later values replace earlier values and malformed entries are retained as
// distinct literal entries.
func MergeEnv(groups ...[]string) []string {
	var out []string
	positions := map[string]int{}
	for _, group := range groups {
		for _, entry := range group {
			key, _, ok := strings.Cut(entry, "=")
			if !ok || key == "" {
				out = append(out, entry)
				continue
			}
			if i, exists := positions[key]; exists {
				out[i] = entry
				continue
			}
			positions[key] = len(out)
			out = append(out, entry)
		}
	}
	return out
}

func EnvValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

func cloneImageConfig(in *dalec.ImageConfig) *dalec.ImageConfig {
	if in == nil {
		return nil
	}
	out := *in
	out.Env = append([]string(nil), in.Env...)
	out.Labels = cloneMap(in.Labels)
	out.Volumes = cloneMap(in.Volumes)
	out.Bases = append([]dalec.BaseImage(nil), in.Bases...)
	return &out
}

func cloneMap[V any](in map[string]V) map[string]V {
	if in == nil {
		return nil
	}
	out := make(map[string]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func appendUnique(dst []string, values ...string) []string {
	for _, v := range values {
		if v != "" && !slices.Contains(dst, v) {
			dst = append(dst, v)
		}
	}
	return dst
}

// BuildImageConfigV2 preserves Formula-ID graph identity while translating the
// already collision-checked rack names into runtime PATH entries.
func BuildImageConfigV2(base *dalec.DockerImageSpec, img *dalec.ImageConfig, record *resolution.RecordV2) (*dalec.DockerImageSpec, Identity, []string, error) {
	projected, _, err := resolution.ProjectV2ForRuntime(record)
	if err != nil {
		return nil, Identity{}, nil, err
	}
	return BuildImageConfig(base, img, projected.Requested, projected.Nodes)
}

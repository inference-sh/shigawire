package builtin

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
)

func registerApt(r *module.Registry) {
	r.Register("apt", module.Typed(newApt))
	r.Register("apt_repo", module.Typed(newAptRepo))
}

type aptArgs struct {
	// Name is the packages: a name, name=version, name:arch.
	Name List `yaml:"name"`
	// State is present (the default), absent or purged.
	State string `yaml:"state"`
	// UpdateCache: the package lists are updated, before an install that
	// has something to install; with no name, every run unless
	// cache_valid_time says they are new enough.
	UpdateCache bool `yaml:"update_cache"`
	// CacheValidTime is how old the lists may be before an update
	// ("1h"); unset is 0, updated once in a run.
	CacheValidTime time.Duration `yaml:"cache_valid_time"`
}

var pkgName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(:[a-z0-9-]+)?(=[A-Za-z0-9.+~:-]+)?$`)

func newApt(env module.Env, a aptArgs) (host.Resource, error) {
	if env.Apt == nil {
		return nil, errors.New("apt: the run has no Apt")
	}
	state, err := choice("state", a.State, "present", "absent", "purged")
	if err != nil {
		return nil, err
	}
	for _, n := range a.Name {
		if !pkgName.MatchString(n) {
			return nil, errors.New("name " + `"` + n + `"` + ": not a package name")
		}
		// Removal matches the installed packages' bare names.
		if state != "present" && strings.ContainsAny(n, ":=") {
			return nil, errors.New("name " + `"` + n + `"` + ": state " + state + " takes a bare package name, with no :arch or =version")
		}
	}
	update := env.Apt.Update(a.CacheValidTime)
	switch {
	case len(a.Name) == 0 && !a.UpdateCache:
		return nil, errors.New("name or update_cache is required")
	case len(a.Name) == 0:
		return update, nil
	case state == "present" && a.UpdateCache:
		return env.Apt.InstallUpdating(a.CacheValidTime, a.Name...), nil
	case state == "present":
		return env.Apt.Install(a.Name...), nil
	case a.UpdateCache:
		return nil, errors.New("update_cache with state " + state + ": an update is for an install")
	}
	return env.Apt.Remove(host.Names(a.Name...), state == "purged"), nil
}

type aptRepoArgs struct {
	// Name names the files: /etc/apt/sources.list.d/<name>.sources and
	// the key in keyring_dir.
	Name       string `yaml:"name"`
	URIs       List   `yaml:"uris"`
	Suites     List   `yaml:"suites"`
	Components List   `yaml:"components"`
	// KeyURL is where the signing key is fetched from, each run; Key is
	// the key itself (armored); KeySrc is a file under the playbook's
	// files/ directory holding it, read when the playbook loads. One of
	// them is required.
	KeyURL string `yaml:"key_url"`
	Key    string `yaml:"key"`
	KeySrc string `yaml:"key_src"`
	// KeySHA256 is the hex digest the key fetched from KeyURL must have.
	KeySHA256 string `yaml:"key_sha256"`
	// KeyringDir is where the key goes; unset is /etc/apt/keyrings.
	KeyringDir string `yaml:"keyring_dir"`
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func newAptRepo(env module.Env, a aptRepoArgs) (host.Resource, error) {
	if env.Apt == nil {
		return nil, errors.New("apt_repo: the run has no Apt")
	}
	if err := required("name", a.Name); err != nil {
		return nil, err
	}
	if !repoName.MatchString(a.Name) {
		return nil, errors.New(`name "` + a.Name + `": letters, digits, _ . - only`)
	}
	switch {
	case len(a.URIs) == 0:
		return nil, errors.New("uris is required")
	case len(a.Suites) == 0:
		return nil, errors.New("suites is required")
	}
	switch n := btoi(a.KeyURL != "") + btoi(a.Key != "") + btoi(a.KeySrc != ""); {
	case n == 0:
		return nil, errors.New("key_url, key or key_src is required")
	case n > 1:
		return nil, errors.New("key_url, key and key_src: give one")
	case a.KeySHA256 != "" && a.KeyURL == "":
		return nil, errors.New("key_sha256 is for key_url")
	case a.KeySHA256 != "" && !sha256Hex.MatchString(a.KeySHA256):
		return nil, fmt.Errorf("key_sha256 %q: want 64 hex digits", a.KeySHA256)
	}
	if a.KeyringDir != "" {
		if err := absPath("keyring_dir", a.KeyringDir); err != nil {
			return nil, err
		}
	}
	r := host.Repo{Name: a.Name, URIs: a.URIs, Suites: a.Suites, Components: a.Components,
		KeyURL: a.KeyURL, KeySHA256: a.KeySHA256, KeyringDir: a.KeyringDir}
	switch {
	case a.Key != "":
		r.Key = []byte(a.Key)
	case a.KeySrc != "":
		b, err := readFiles(env, "key_src", a.KeySrc)
		if err != nil {
			return nil, err
		}
		if _, err := r.KeyPath(b); err != nil {
			return nil, fmt.Errorf("key_src %q: %w", a.KeySrc, err)
		}
		r.Key = b
	}
	return env.Apt.Repo(r), nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

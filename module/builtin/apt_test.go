package builtin_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
)

// fakeDpkg answers dpkg-query from installed, and apt-get install and
// remove by changing it.
func fakeDpkg(h *hosttest.Host, installed ...string) *[]string {
	pkgs := slices.Clone(installed)
	h.Exec.Respond = func(line string) host.Output {
		switch {
		case strings.HasPrefix(line, "dpkg-query"):
			var b strings.Builder
			for _, p := range pkgs {
				b.WriteString(p + "\tii \t1.0\n")
			}
			return hosttest.Out(b.String())
		case strings.HasPrefix(line, "apt-get") && strings.Contains(line, " install "):
			pkgs = append(pkgs, strings.Fields(strings.SplitN(line, " install ", 2)[1])...)
		case strings.HasPrefix(line, "apt-get") && strings.Contains(line, " remove "):
			for _, p := range strings.Fields(strings.SplitN(line, " remove ", 2)[1]) {
				pkgs = slices.DeleteFunc(pkgs, func(s string) bool { return s == p })
			}
		}
		return host.Output{}
	}
	return &pkgs
}

func TestApt(t *testing.T) {
	h, env := newHost(t)
	pkgs := fakeDpkg(h, "curl")
	hosttest.Converge(t, build(t, env, "apt", map[string]any{"name": []any{"curl", "jq"}, "update_cache": true}), host.Changed)
	if !slices.Contains(*pkgs, "jq") {
		t.Fatal(*pkgs)
	}
	if n := len(h.Exec.Ran("apt-get")); n != 2 { // one update, one install
		t.Fatalf("ran %v", h.Exec.Calls())
	}
	h.Exec.Reset()
	hosttest.Converge(t, build(t, env, "apt", map[string]any{"name": "jq", "state": "purged"}), host.Changed)
	if got := h.Exec.Ran("apt-get"); len(got) != 1 || !strings.HasSuffix(got[0], "remove --purge jq") {
		t.Fatal(got)
	}
	// An update alone, every run, is an action: changed each time.
	if r := hosttest.ApplyOne(t, build(t, env, "apt", map[string]any{"update_cache": true})); r.Status != host.Changed || !r.Action {
		t.Fatalf("%s action=%v", r.Status, r.Action)
	}
}

func TestAptRefused(t *testing.T) {
	_, env := newHost(t)
	refuse(t, env, "apt", map[string]any{}, "name or update_cache")
	refuse(t, env, "apt", map[string]any{"name": "curl", "state": "latest"}, "want present, absent, purged")
	refuse(t, env, "apt", map[string]any{"name": "curl; rm -rf /"}, "not a package name")
	refuse(t, env, "apt", map[string]any{"name": "telnet:amd64", "state": "absent"}, "bare package name")
	refuse(t, env, "apt", map[string]any{"name": "telnet=1.0", "state": "purged"}, "bare package name")
	refuse(t, env, "apt", map[string]any{"name": "curl", "state": "absent", "update_cache": true}, "update_cache")
	refuse(t, env, "apt", map[string]any{"name": "curl", "cache_valid_time": 3600}, "duration")
}

const armoredKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQENBF...\n-----END PGP PUBLIC KEY BLOCK-----\n"

func TestAptRepo(t *testing.T) {
	h, env := newHost(t)
	hosttest.Converge(t, build(t, env, "apt_repo", map[string]any{"name": "docker", "uris": "https://download.docker.com/linux/ubuntu",
		"suites": "noble", "components": []any{"stable"}, "key": armoredKey}), host.Changed)
	want := "Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: noble\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n"
	if got := h.Read("/etc/apt/sources.list.d/docker.sources"); got != want {
		t.Fatalf("%q", got)
	}
	if h.Read("/etc/apt/keyrings/docker.asc") != armoredKey {
		t.Fatal("key")
	}
	refuse(t, env, "apt_repo", map[string]any{"name": "x", "uris": "u", "suites": "s"}, "key_url or key")
	refuse(t, env, "apt_repo", map[string]any{"name": "../x", "uris": "u", "suites": "s", "key": "k"}, "letters")
	refuse(t, env, "apt_repo", map[string]any{"name": "x", "suites": "s", "key": "k"}, "uris is required")
}

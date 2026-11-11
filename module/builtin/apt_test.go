package builtin_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
)

func TestApt(t *testing.T) {
	h, env := simHost(t)
	h.Packages["curl"] = true
	hosttest.Converge(t, build(t, env, "apt", map[string]any{"name": []any{"curl", "jq"}, "update_cache": true}), host.Changed)
	if !h.Packages["jq"] {
		t.Fatal(h.Packages)
	}
	if n := len(h.Exec.Ran("apt-get")); n != 2 { // one update, one install
		t.Fatalf("ran %v", h.Exec.Calls())
	}
	h.Exec.Reset()
	hosttest.Converge(t, build(t, env, "apt", map[string]any{"name": "jq", "state": "purged"}), host.Changed)
	if got := h.Exec.Ran("apt-get"); len(got) != 1 || !strings.HasSuffix(got[0], "remove --purge jq") || h.Packages["jq"] {
		t.Fatal(got, h.Packages)
	}
	// An update alone is an action, once a run.
	update := map[string]any{"update_cache": true}
	play := host.Play{Tasks: []host.Task{{Resource: build(t, env, "apt", update)}, {Resource: build(t, env, "apt", update)}}}
	got, err := hosttest.Run(hosttest.Apply, play)
	if err != nil || got[0].Status != host.Changed || !got[0].Action || got[1].Status != host.OK {
		t.Fatalf("%+v %v", got, err)
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
	refuse(t, env, "apt_repo", map[string]any{"name": "x", "uris": "u", "suites": "s"}, "key_url, key or key_src is required")
	refuse(t, env, "apt_repo", map[string]any{"name": "x", "uris": "u", "suites": "s", "key": "k", "key_url": "https://k"}, "give one")
	refuse(t, env, "apt_repo", map[string]any{"name": "x", "uris": "u", "suites": "s", "key": "k", "key_src": "k.asc"}, "give one")
	refuse(t, env, "apt_repo", map[string]any{"name": "../x", "uris": "u", "suites": "s", "key": "k"}, "letters")
	refuse(t, env, "apt_repo", map[string]any{"name": "x", "suites": "s", "key": "k"}, "uris is required")
}

// key_src is a key file under the playbook's files/ directory, read as
// copy reads src.
func TestAptRepoKeySrc(t *testing.T) {
	h, env := newHost(t)
	env.Files = fstest.MapFS{"keys/docker.asc": {Data: []byte(armoredKey)}, "notakey": {Data: []byte("<html>\n")}}
	args := func(src string) map[string]any {
		return map[string]any{"name": "docker", "uris": "https://download.docker.com/linux/ubuntu", "suites": "noble", "key_src": src}
	}
	hosttest.Converge(t, build(t, env, "apt_repo", args("keys/docker.asc")), host.Changed)
	if h.Read("/etc/apt/keyrings/docker.asc") != armoredKey {
		t.Fatal("key")
	}
	if got := h.Read("/etc/apt/sources.list.d/docker.sources"); !strings.Contains(got, "Signed-By: /etc/apt/keyrings/docker.asc\n") {
		t.Fatalf("%q", got)
	}
	refuse(t, env, "apt_repo", args("missing.asc"), "key_src: open missing.asc")
	refuse(t, env, "apt_repo", args("../etc/key.asc"), `key_src "../etc/key.asc": not a path inside files/`)
	refuse(t, env, "apt_repo", args("/etc/key.asc"), "not a path inside files/")
	refuse(t, env, "apt_repo", args("notakey"), `key_src "notakey": not an OpenPGP public key`)
	env.Files = nil
	refuse(t, env, "apt_repo", args("keys/docker.asc"), "no files/ directory")
}

func TestAptRepoKeySHA256(t *testing.T) {
	_, env := newHost(t)
	sum := sha256.Sum256([]byte(armoredKey))
	args := map[string]any{"name": "x", "uris": "u", "suites": "s", "key_url": "https://k", "key_sha256": strings.ToUpper(hex.EncodeToString(sum[:]))}
	build(t, env, "apt_repo", args)
	args["key_sha256"] = "abc"
	refuse(t, env, "apt_repo", args, `key_sha256 "abc": want 64 hex digits`)
	refuse(t, env, "apt_repo", map[string]any{"name": "x", "uris": "u", "suites": "s", "key": armoredKey, "key_sha256": hex.EncodeToString(sum[:])}, "key_sha256 is for key_url")
}

package host_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/host/hosttest"
)

// The helpers the package exports for its callers (the engine's setup)
// read and build what its resources do.

func TestTail(t *testing.T) {
	if got := host.Tail("a\n\nb\nc\n\n", 2); !slices.Equal(got, []string{"b", "c"}) {
		t.Fatalf("%q", got)
	}
	if got := host.Tail("a\n", 5); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("%q", got)
	}
}

func TestShowProps(t *testing.T) {
	kv := host.ShowProps("Id=engine.service\nActiveState=active\nSubState=auto-restart\nInvocationID=\n")
	if kv["Id"] != "engine.service" || kv["ActiveState"] != "active" || kv["SubState"] != "auto-restart" {
		t.Fatalf("%v", kv)
	}
	if v, ok := kv["InvocationID"]; !ok || v != "" {
		t.Fatalf("an empty property: %q %v", v, ok)
	}
}

func TestSameContent(t *testing.T) {
	dir := t.TempDir()
	a := hosttest.WriteFile(t, dir, "a", "same")
	b := hosttest.WriteFile(t, dir, "b", "same")
	c := hosttest.WriteFile(t, dir, "c", "diff")
	d := hosttest.WriteFile(t, dir, "d", "longer")
	for _, tc := range []struct {
		x, y string
		want bool
	}{{a, b, true}, {a, c, false}, {a, d, false}} {
		if got, err := host.SameContent(tc.x, tc.y); err != nil || got != tc.want {
			t.Fatalf("%s %s: %v %v", tc.x, tc.y, got, err)
		}
	}
	if _, err := host.SameContent(a, filepath.Join(dir, "none")); !os.IsNotExist(err) {
		t.Fatalf("a missing file: %v", err)
	}
}

func TestNoninteractiveEnv(t *testing.T) {
	env := host.NoninteractiveEnv()
	if !slices.Contains(env, "DEBIAN_FRONTEND=noninteractive") || !slices.Contains(env, "NEEDRESTART_MODE=a") {
		t.Fatalf("%q", env)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, "0123456789")
	}))
	defer srv.Close()
	b, err := host.Fetch(context.Background(), srv.Client(), srv.URL+"/index", 4)
	if err != nil || string(b) != "0123" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := host.Fetch(context.Background(), srv.Client(), srv.URL+"/missing", 4); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("a 404: %v", err)
	}
}

func TestWithBlockAndWithoutLine(t *testing.T) {
	const begin, end = "# BEGIN x", "# END x"
	got, err := host.WithBlock([]byte("a\n"), begin, end, "b", false)
	if err != nil || string(got) != "a\n# BEGIN x\nb\n# END x\n" {
		t.Fatalf("%q %v", got, err)
	}
	if back, err := host.WithBlock(got, begin, end, "", true); err != nil || string(back) != "a\n" {
		t.Fatalf("absent: %q %v", back, err)
	}
	if _, err := host.WithBlock([]byte("# BEGIN x\nb\n"), begin, end, "c", false); err == nil {
		t.Fatal("a begin marker with no end")
	}
	if got := host.WithoutLine([]byte("a\nb=1\nc\n"), "a", regexp.MustCompile(`^b=`)); string(got) != "c\n" {
		t.Fatalf("%q", got)
	}
}

func TestSwarmState(t *testing.T) {
	x := (&hosttest.Exec{}).On("docker info", hosttest.Out("active\n"))
	if st, err := host.SwarmState(context.Background(), x); err != nil || st != "active" {
		t.Fatalf("%q %v", st, err)
	}
	x = (&hosttest.Exec{}).On("docker info", hosttest.Exit(1))
	if _, err := host.SwarmState(context.Background(), x); err == nil {
		t.Fatal("a failed docker info")
	}
}

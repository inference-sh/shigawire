//go:build conformance

package conformance

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The distros package host supports, and their public images.
var distros = []string{"ubuntu-22.04", "ubuntu-24.04", "debian-12", "debian-13"}

func baseImage(distro string) string {
	name, version, _ := strings.Cut(distro, "-")
	return name + ":" + version
}

var only = flag.String("distros", "", "comma-separated distros to run (default all)")

// insideEnv tells the test binary it is the one copied into a container:
// only then does TestInside touch the host it runs on.
const insideEnv = "RES_CONFORMANCE_INSIDE"

// hostDockerfile is a distro's image as a stock cloud image has it: CA
// roots, curl and gpg, a login user. Its apt lists are removed, as on a
// fresh image before its first update.
const hostDockerfile = `ARG BASE
FROM ${BASE}
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl gnupg \
    && rm -rf /var/lib/apt/lists/* && useradd -m -s /bin/bash infsh
CMD ["sleep", "infinity"]
`

// systemdDockerfile is a host image with systemd to boot.
const systemdDockerfile = `ARG BASE
FROM ${BASE}
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends systemd systemd-sysv dbus \
    && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
`

// TestConformance runs TestInside in a container per distro: once in the
// distro's image as it is, once in a privileged one booted with systemd
// for the units. It prints a resource by distro table at the end.
func TestConformance(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no docker")
	}
	bin := buildInside(t)
	tab := &table{cells: map[string]map[string]string{}}
	t.Cleanup(func() { t.Log("\n" + tab.String()) })

	for _, d := range distros {
		if *only != "" && !slices.Contains(strings.Split(*only, ","), d) {
			continue
		}
		t.Run(d, func(t *testing.T) {
			t.Parallel()
			image := buildImage(t, "shiga-conformance:"+d, hostDockerfile, baseImage(d))
			t.Run("plain", func(t *testing.T) {
				t.Parallel()
				c := startContainer(t, image, false)
				tab.add(d, runInside(t, c, bin, "-test.run", "^TestInside$", "-test.skip", "^TestInside/systemd"))
			})
			t.Run("systemd", func(t *testing.T) {
				t.Parallel()
				c := startContainer(t, buildImage(t, "shiga-conformance-systemd:"+d, systemdDockerfile, image), true)
				waitBooted(t, c)
				tab.add(d, runInside(t, c, bin, "-test.run", "^TestInside/systemd"))
			})
		})
	}
}

// buildInside compiles this package's tests for a container: static, so
// one binary runs on every distro's libc.
func buildInside(t *testing.T) string {
	bin := filepath.Join(t.TempDir(), "conformance.test")
	cmd := exec.CommandContext(t.Context(), "go", "test", "-c", "-tags", "conformance", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the test binary: %v\n%s", err, out)
	}
	return bin
}

// buildImage is tag, built from dockerfile on base the first time; later
// runs use the one built.
func buildImage(t *testing.T, tag, dockerfile, base string) string {
	if _, err := docker(t.Context(), nil, "image", "inspect", tag); err == nil {
		return tag
	}
	if out, err := docker(t.Context(), strings.NewReader(dockerfile),
		"build", "-q", "-t", tag, "--build-arg", "BASE="+base, "-"); err != nil {
		t.Fatalf("build %s: %v\n%s", tag, err, out)
	}
	return tag
}

// startContainer runs image, removed when t ends. A systemd one is
// privileged with its own cgroup namespace, so systemd can manage its
// cgroups without seeing the host's.
func startContainer(t *testing.T, image string, systemd bool) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "shiga-conformance-" + hex.EncodeToString(b)
	args := []string{"run", "-d", "--name", name, "--label", "shiga-conformance=1"}
	if systemd {
		args = append(args, "--privileged", "--cgroupns=private", "-e", "container=docker",
			"--tmpfs", "/run", "--tmpfs", "/run/lock", image)
	} else {
		args = append(args, image, "sleep", "infinity")
	}
	if out, err := docker(t.Context(), nil, args...); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = docker(context.Background(), nil, "rm", "-f", name) })
	return name
}

// waitBooted waits for systemd to finish starting; degraded is booted
// (a container has units that cannot start). Until systemd has its bus
// up, systemctl fails at once instead of waiting.
func waitBooted(t *testing.T, c string) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	for {
		out, _ := docker(ctx, nil, "exec", c, "systemctl", "is-system-running", "--wait")
		switch s := strings.TrimSpace(out); {
		case s == "running" || s == "degraded":
			return
		case ctx.Err() != nil:
			t.Fatalf("systemd did not boot: %q", s)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// runInside copies bin into c, runs it as root, and returns the result of
// each TestInside case.
func runInside(t *testing.T, c, bin string, args ...string) map[string]string {
	if out, err := docker(t.Context(), nil, "cp", bin, c+":/conformance.test"); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, out)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	out, err := docker(ctx, nil, append([]string{"exec", "-u", "root", "-e", insideEnv + "=1", c,
		"/conformance.test", "-test.v", "-test.count=1"}, args...)...)
	t.Log("\n" + out)
	if err != nil {
		t.Errorf("TestInside: %v", err)
	}
	return caseResults(out)
}

var caseLine = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): TestInside/(\S+)`)

func caseResults(out string) map[string]string {
	got := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		if m := caseLine.FindStringSubmatch(sc.Text()); m != nil {
			got[m[2]] = strings.ToLower(m[1])
		}
	}
	return got
}

func docker(ctx context.Context, stdin *strings.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// table is each case's result per distro.
type table struct {
	mu    sync.Mutex
	cells map[string]map[string]string // case -> distro -> pass, fail, skip
}

func (tb *table) add(distro string, got map[string]string) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	for c, r := range got {
		if tb.cells[c] == nil {
			tb.cells[c] = map[string]string{}
		}
		// A case runs in both containers: skipped in one, run in the other.
		if prev := tb.cells[c][distro]; prev == "" || prev == "skip" {
			tb.cells[c][distro] = r
		}
	}
}

func (tb *table) String() string {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	var cases []string
	for c := range tb.cells {
		cases = append(cases, c)
	}
	slices.Sort(cases)
	var b strings.Builder
	fmt.Fprintf(&b, "%-34s", "resource")
	for _, d := range distros {
		fmt.Fprintf(&b, " %-13s", d)
	}
	b.WriteString("\n")
	for _, c := range cases {
		fmt.Fprintf(&b, "%-34s", c)
		for _, d := range distros {
			r := tb.cells[c][d]
			if r == "" {
				r = "-"
			}
			fmt.Fprintf(&b, " %-13s", r)
		}
		b.WriteString("\n")
	}
	return b.String()
}

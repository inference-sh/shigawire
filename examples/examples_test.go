// Package examples tests the example playbooks on a simulated host.
package examples

import (
	"os"
	"strings"
	"testing"

	"github.com/inference-sh/shigawire/host/hosttest"
	"github.com/inference-sh/shigawire/host/hosttest/sim"
	"github.com/inference-sh/shigawire/shigatest"
)

// webHost is a simulated host where nginx answers the health check.
func webHost(t *testing.T) *sim.Host {
	h := sim.New(t)
	h.Exec.On("curl ", hosttest.Out("200"))
	return h
}

func TestWeb(t *testing.T) {
	h := webHost(t)
	shigatest.RunOn(t, h.Host, "web/web.yml", map[string]string{"DOMAIN": "shop.example.org", "DEPLOY_KEY": "ssh-ed25519 AAAAC3Nz me@laptop"})

	if !h.Packages["nginx"] || !h.Packages["ufw"] {
		t.Fatal(h.Packages)
	}
	if h.Exists("/etc/nginx/sites-enabled/default") {
		t.Fatal("default site still enabled")
	}
	if target, err := os.Readlink(h.Path("/etc/nginx/sites-enabled/shop.example.org")); err != nil || target != "/etc/nginx/sites-available/shop.example.org" {
		t.Fatal(target, err)
	}
	site := h.Read("/etc/nginx/sites-available/shop.example.org")
	if !strings.Contains(site, "server_name shop.example.org;") || !strings.Contains(site, "root /var/www/shop.example.org;") {
		t.Fatal(site)
	}
	if !strings.Contains(h.Read("/etc/nginx/snippets/security-headers.conf"), "nosniff") {
		t.Fatal("no snippet")
	}
	if u := h.Units["nginx"]; !u.Enabled || !u.Active || u.Reloads != 1 {
		t.Fatalf("nginx %+v: want one reload, from the first apply", *u)
	}
	if u := h.Users["deploy"]; u == nil || u.Shell != "/bin/bash" || !h.Exists("/home/deploy") {
		t.Fatal(h.Users)
	}
	if !strings.Contains(h.Read("/home/deploy/.ssh/authorized_keys"), "me@laptop") {
		t.Fatal("no key")
	}
	rules := h.Read("/etc/ufw/user.rules")
	for _, want := range []string{"### tuple ### limit tcp 22 0.0.0.0/0 any 0.0.0.0/0 in comment=737368", "### tuple ### allow tcp 80,443 0.0.0.0/0 any 0.0.0.0/0 in comment=776562"} {
		if !strings.Contains(rules, want) {
			t.Fatalf("no %q in\n%s", want, rules)
		}
	}
	if !strings.Contains(h.Read("/etc/ufw/ufw.conf"), "ENABLED=yes") {
		t.Fatal("ufw not enabled")
	}
	// nginx -t ran before the reload.
	calls := strings.Join(h.Exec.Calls(), "\n")
	if i, j := strings.Index(calls, "nginx -t -q"), strings.Index(calls, "systemctl reload nginx"); i < 0 || j < i {
		t.Fatal(calls)
	}
}

// With no key, the key task is skipped and the rest still converges.
func TestWebNoKey(t *testing.T) {
	h := webHost(t)
	shigatest.RunOn(t, h.Host, "web/web.yml", nil)
	if h.Exists("/home/deploy/.ssh/authorized_keys") {
		t.Fatal("key written")
	}
	if !h.Exists("/etc/nginx/sites-enabled/example.com") {
		t.Fatal("no site")
	}
}

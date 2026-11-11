package host

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serve(t *testing.T, body *string) (url string, hits *int) {
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, *body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func sum(s string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(s))) }

func TestDownload_Checksum(t *testing.T) {
	body := "#!/bin/sh\n"
	url, hits := serve(t, &body)
	path := filepath.Join(t.TempDir(), "nvidia.run")
	d := &Download{URL: url + "/nvidia.run", Path: path, SHA256: sum(body), Mode: 0o755}
	converge(t, d, Changed)
	if *hits != 1 {
		t.Fatalf("fetched %d times: a matching file is in place", *hits)
	}
	if m := perm(t, path); m != 0o755 {
		t.Fatalf("mode %o", m)
	}

	// Content the sum does not match is never put in place.
	body = "tampered"
	os.WriteFile(path, []byte("old"), 0o755)
	_, err := runMode((*Runner).Run, Play{Tasks: []Task{{Resource: d}}})
	if err == nil || !strings.Contains(err.Error(), "want "+sum("#!/bin/sh\n")) {
		t.Fatalf("err %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("file %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("left %v", ents)
	}
}

func TestDownload_ForceAndExisting(t *testing.T) {
	body := "script v1"
	url, hits := serve(t, &body)
	path := filepath.Join(t.TempDir(), "get-docker.sh")

	// Without Force a file that exists is in place.
	d := &Download{URL: url, Path: path}
	converge(t, d, Changed)
	body = "script v2"
	if r := apply(t, d); r.Status != OK || *hits != 1 {
		t.Fatalf("%s, %d fetches", r.Status, *hits)
	}

	// Force fetches again: changed only for new content.
	f := &Download{URL: url, Path: path, Force: true}
	if r := apply(t, f); r.Status != Changed {
		t.Fatalf("new content: %s", r.Status)
	}
	if r := apply(t, f); r.Status != OK || r.Msg != "same content" {
		t.Fatalf("same content: %s %q", r.Status, r.Msg)
	}
	// The same content leaves the file there: same inode.
	before, _ := os.Stat(path)
	apply(t, f)
	if after, _ := os.Stat(path); !os.SameFile(before, after) {
		t.Fatal("the same content replaced the file")
	}
	// It is an action: Verify reports it in place.
	if got, err := runMode((*Runner).Verify, Play{Tasks: []Task{{Resource: f}}}); err != nil || got[0].Status != OK {
		t.Fatalf("verify: %v %v", got, err)
	}

	// A mode that is off is put right without fetching.
	n := *hits
	if r := apply(t, &Download{URL: url, Path: path, Mode: 0o755}); r.Status != Changed || *hits != n {
		t.Fatalf("mode: %s, %d fetches", r.Status, *hits-n)
	}
	if m := perm(t, path); m != 0o755 {
		t.Fatalf("mode %o", m)
	}
}

func TestDownload_HTTPError(t *testing.T) {
	body := ""
	url, _ := serve(t, &body)
	d := &Download{URL: url + "/missing", Path: filepath.Join(t.TempDir(), "x")}
	if _, err := runMode((*Runner).Run, Play{Tasks: []Task{{Resource: d}}}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err %v", err)
	}
}

// roundTrip serves body for any request, counting them.
type roundTrip struct {
	body string
	hits int
}

func (rt *roundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.hits++
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(rt.body)), Request: r}, nil
}

// Client, when set, is what fetches the URL.
func TestDownload_Client(t *testing.T) {
	rt := &roundTrip{body: "payload"}
	path := filepath.Join(t.TempDir(), "f")
	d := &Download{URL: "http://unreachable.invalid/f", Path: path, SHA256: sum("payload"), Client: &http.Client{Transport: rt}}
	converge(t, d, Changed)
	if b, _ := os.ReadFile(path); string(b) != "payload" || rt.hits != 1 {
		t.Fatalf("%q, %d fetches", b, rt.hits)
	}
}

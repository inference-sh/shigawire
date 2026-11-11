package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"
)

// Download is a file fetched from a URL. With SHA256 the download must
// match it, and a file that matches already is in place. Without, a file
// that exists is in place unless Force fetches it again (a script that
// moves on): an action, run every time (State.Action), changed only when
// the content is new; the same content leaves the file as it is. The file is
// written beside the destination and renamed into place once complete
// (and checked), so a failed download leaves the old file.
type Download struct {
	URL  string
	Path string
	// SHA256 is the hex digest the file must have ("" checks nothing).
	SHA256 string
	// Mode is the permission bits; 0 leaves an existing file's and makes a
	// new one 0644. The file is the writer's.
	Mode  fs.FileMode
	Force bool
	// Client fetches the URL; nil is one that gives up after 30 minutes.
	Client *http.Client
}

func (d *Download) ID() string { return "download " + d.URL + " to " + d.Path }

// fetched is what Download.Check found: the file there (nil: none), its
// sha256 ("" when not read), and whether only its mode is wrong.
type fetched struct {
	fi       fs.FileInfo
	sum      string
	modeOnly bool
}

func (d *Download) Check(context.Context) (State, error) {
	fi, err := os.Lstat(d.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return driftData(fetched{}, "download")
	case err != nil:
		return State{}, err
	case !fi.Mode().IsRegular():
		return driftData(fetched{fi: fi}, "%s, want a file", kind(fi))
	}
	sum := ""
	if d.SHA256 != "" || d.Force {
		if sum, err = fileSum(d.Path); err != nil {
			return State{}, err
		}
	}
	switch {
	case d.SHA256 != "" && !strings.EqualFold(sum, d.SHA256):
		return driftData(fetched{fi: fi, sum: sum}, "sha256 %s, want %s", sum, d.SHA256)
	case d.SHA256 == "" && d.Force:
		return State{Drift: "download again", Action: true, Data: fetched{fi: fi, sum: sum}}, nil
	}
	if m := (attrs{mode: d.Mode}).drift(fi); len(m) > 0 {
		return driftData(fetched{fi: fi, sum: sum, modeOnly: true}, "%s", strings.Join(m, "; "))
	}
	return inPlace()
}

func (d *Download) Apply(ctx context.Context, st State) (Result, error) {
	f := st.Data.(fetched)
	if f.modeOnly {
		if err := os.Chmod(d.Path, d.Mode); err != nil {
			return Result{}, err
		}
		return changed("%s", st.Drift)
	}
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	resp, err := get(ctx, client, d.URL)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body := &checked{r: resp.Body, h: sha256.New(), want: strings.ToLower(d.SHA256), same: f.sum}
	mode, _ := (attrs{mode: d.Mode}).forNew(f.fi, 0o644)
	err = writeAtomic(d.Path, body, mode, nil)
	if errors.Is(err, errSameContent) {
		if m := (attrs{mode: d.Mode}).drift(f.fi); len(m) > 0 {
			if err := os.Chmod(d.Path, d.Mode); err != nil {
				return Result{}, err
			}
			return changed("same content; %s", strings.Join(m, "; "))
		}
		return unchanged("same content")
	}
	if err != nil {
		return Result{}, fmt.Errorf("download %s: %w", d.URL, err)
	}
	return changed("downloaded, sha256 %s", body.sum)
}

// get is a GET of url that answered 200, its body to read and close.
func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// Fetch reads what url serves with client, up to limit bytes: an
// installer's index, a published sha256.
func Fetch(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := get(ctx, client, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	return b, nil
}

// checked hashes what it reads and, at the end, fails instead of ending
// when the hash is not want: the atomic write then never renames the file
// into place.
type checked struct {
	r    io.Reader
	h    hash.Hash
	want string
	sum  string
	// same is the sha256 of the file there: a download with it ends in
	// errSameContent, so the file is not replaced.
	same string
}

// errSameContent ends a download that has the content of the file there.
var errSameContent = errors.New("same content")

func (c *checked) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.h.Write(p[:n])
	if err == io.EOF {
		c.sum = hex.EncodeToString(c.h.Sum(nil))
		if c.want != "" && c.sum != c.want {
			return n, fmt.Errorf("sha256 %s, want %s", c.sum, c.want)
		}
		if c.same != "" && c.sum == c.same {
			return n, errSameContent
		}
	}
	return n, err
}

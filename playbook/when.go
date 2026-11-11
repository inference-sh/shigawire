package playbook

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
)

// cond is one when condition:
//
//	[not] SUBJECT [OP VALUE]
//	[not] exists PATH
//
// SUBJECT is ${VAR}, mode (plan, apply or verify), a fact (os.id,
// os.version_id, os.arch) or a
// registered result (NAME.rc, NAME.stdout, NAME.changed, NAME.failed,
// NAME.skipped, NAME.ok). OP is ==, != or contains. Without OP the
// subject is tested for truth.
type cond struct {
	raw    string
	not    bool
	exists string // the path, for an exists test
	subj   subject
	op     string
	value  string
}

// subject is what a condition reads.
type subject struct {
	constant *string // ${VAR}, known when the playbook loads
	fact     string  // "mode", "os.id", "os.version_id", "os.arch"
	reg      *record
	regName  string
	field    string // of reg
}

var facts = []string{"os.id", "os.version_id", "os.arch"}

var regFields = []string{"rc", "stdout", "changed", "failed", "skipped", "ok"}

// parseCond parses s. registered are the names registered by tasks before
// this one.
func parseCond(s string, vars map[string]string, registered map[string]*record) (*cond, error) {
	c := &cond{raw: strings.TrimSpace(s)}
	rest := c.raw
	if r, ok := cutWord(rest, "not"); ok {
		c.not, rest = true, r
	}
	if rest == "" {
		return nil, errors.New("empty condition")
	}
	if r, ok := cutWord(rest, "exists"); ok {
		p, err := expand(r, vars)
		if err != nil {
			return nil, err
		}
		p = unquote(p)
		if !path.IsAbs(p) {
			return nil, fmt.Errorf("exists %q: want an absolute path", p)
		}
		c.exists = p
		return c, nil
	}
	word, rest, _ := strings.Cut(rest, " ")
	rest = strings.TrimSpace(rest)
	for _, op := range []string{"==", "!="} {
		if strings.Contains(word, op) {
			return nil, fmt.Errorf("%q: put spaces around %s", word, op)
		}
	}
	switch {
	case strings.Contains(word, "${"):
		v, err := expand(word, vars)
		if err != nil {
			return nil, err
		}
		c.subj.constant = &v
	case word == "mode":
		c.subj.fact = word
	case strings.HasPrefix(word, "os."):
		if !slices.Contains(facts, word) {
			return nil, fmt.Errorf("unknown fact %q (facts: %s)", word, strings.Join(facts, ", "))
		}
		c.subj.fact = word
	default:
		name, field, ok := strings.Cut(word, ".")
		rec := registered[name]
		switch {
		case !ok || !validName(name):
			return nil, fmt.Errorf("%q: a condition reads ${VAR}, mode, a fact (%s) or NAME.FIELD of a registered task", word, strings.Join(facts, ", "))
		case rec == nil:
			return nil, fmt.Errorf("%q: no task before this one registers %q", word, name)
		case !slices.Contains(regFields, field):
			return nil, fmt.Errorf("%q: unknown field %q (fields: %s)", word, field, strings.Join(regFields, ", "))
		}
		c.subj.reg, c.subj.regName, c.subj.field = rec, name, field
	}
	if rest == "" {
		return c, nil
	}
	op, val, _ := strings.Cut(rest, " ")
	switch op {
	case "==", "!=", "contains":
	default:
		return nil, fmt.Errorf("%q: want ==, != or contains after %s", op, word)
	}
	val = strings.TrimSpace(val)
	if val == "" {
		return nil, fmt.Errorf("nothing after %s (write \"\" for the empty string)", op)
	}
	v, err := expand(val, vars)
	if err != nil {
		return nil, err
	}
	c.op, c.value = op, unquote(v)
	return c, nil
}

// cutWord is s after the word w and the spaces that follow it, when s
// starts with that word.
func cutWord(s, w string) (string, bool) {
	r, ok := strings.CutPrefix(s, w)
	if !ok || (r != "" && r[0] != ' ' && r[0] != '\t') {
		return s, false
	}
	return strings.TrimSpace(r), true
}

// unquote is s without one pair of matching quotes around it.
func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// truthy reports whether a value counts as true: anything but "", 0,
// false, no and off (in any case).
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

// eval evaluates c on the host env acts on.
func (c *cond) eval(ctx context.Context, env module.Env, f *factCache) (bool, error) {
	ok, err := c.test(ctx, env, f)
	if err != nil {
		return false, fmt.Errorf("when %s: %w", c.raw, err)
	}
	return ok != c.not, nil
}

func (c *cond) test(ctx context.Context, env module.Env, f *factCache) (bool, error) {
	if c.exists != "" {
		_, err := os.Lstat(env.Path(c.exists))
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, fs.ErrNotExist):
			return false, nil
		}
		return false, err
	}
	v, err := c.subj.read(ctx, f)
	if err != nil {
		return false, err
	}
	if c.subj.field == "stdout" && c.op != "contains" {
		v = strings.TrimSpace(v)
	}
	switch c.op {
	case "":
		return truthy(v), nil
	case "==":
		return v == c.value, nil
	case "!=":
		return v != c.value, nil
	}
	return strings.Contains(v, c.value), nil
}

func (s subject) read(ctx context.Context, f *factCache) (string, error) {
	switch {
	case s.constant != nil:
		return *s.constant, nil
	case s.fact != "":
		return f.get(ctx, s.fact)
	}
	r := s.reg
	if !r.ran {
		return "", fmt.Errorf("%s has not run", s.regName)
	}
	switch s.field {
	case "changed":
		return strconv.FormatBool(r.changed), nil
	case "failed":
		return strconv.FormatBool(r.failed), nil
	case "skipped":
		return strconv.FormatBool(r.skipped), nil
	case "ok":
		return strconv.FormatBool(!r.failed && !r.skipped), nil
	}
	if r.out == nil {
		return "", fmt.Errorf("%s ran no command", s.regName)
	}
	if s.field == "rc" {
		return strconv.Itoa(r.out.Code), nil
	}
	return r.out.Out, nil
}

// factCache reads the facts once a run, when a condition first asks.
// mode is the run's: plan, apply or verify.
type factCache struct {
	env     module.Env
	mode    string
	read    bool
	values  map[string]string
	osErr   error
	archErr error
}

func (f *factCache) reset(mode string) {
	f.mode, f.read, f.values, f.osErr, f.archErr = mode, false, nil, nil, nil
}

func (f *factCache) get(ctx context.Context, name string) (string, error) {
	if name == "mode" {
		return f.mode, nil
	}
	if !f.read {
		f.read, f.values = true, map[string]string{}
		f.osErr = f.readOSRelease()
		f.archErr = f.readArch(ctx)
	}
	if name == "os.arch" {
		return f.values[name], f.archErr
	}
	return f.values[name], f.osErr
}

// readOSRelease reads ID and VERSION_ID from /etc/os-release.
func (f *factCache) readOSRelease() error {
	kv, err := host.ReadKV(f.env.Path("/etc/os-release"))
	if err != nil {
		return fmt.Errorf("read /etc/os-release: %w", err)
	}
	f.values["os.id"], f.values["os.version_id"] = kv["ID"], kv["VERSION_ID"]
	return nil
}

// readArch reads the machine with uname -m, named as Go names it (amd64,
// arm64); a machine Go has no other name for keeps uname's.
func (f *factCache) readArch(ctx context.Context) error {
	if f.env.Exec == nil {
		return errors.New("os.arch: no Exec to run uname with")
	}
	o := f.env.Exec.Run(ctx, host.Cmd{Name: "uname", Args: []string{"-m"}})
	if o.Err != nil {
		return fmt.Errorf("os.arch: %w", o.Err)
	}
	m := strings.TrimSpace(o.Out)
	switch m {
	case "x86_64":
		m = "amd64"
	case "aarch64", "arm64":
		m = "arm64"
	case "armv7l", "armv6l":
		m = "arm"
	case "i386", "i686":
		m = "386"
	}
	f.values["os.arch"] = m
	return nil
}

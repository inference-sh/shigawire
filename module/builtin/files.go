package builtin

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
)

func registerFiles(r *module.Registry) {
	r.Register("file", module.Typed(newFile))
	r.Register("copy", module.Typed(newCopy))
	r.Register("dir", module.Typed(newDir))
	r.Register("absent", module.Typed(newAbsent))
	r.Register("symlink", module.Typed(newSymlink))
	register(r, module.Typed(newLine), "line", "lineinfile")
	register(r, module.Typed(newBlock), "block", "blockinfile")
}

// writeArgs are what file writes, beside its path.
type writeArgs struct {
	// Content is the file's content; Src names a file to copy instead.
	// A relative Src is under the playbook's files/ directory (Env.Files),
	// read when the task is loaded; an absolute one is a file on the host,
	// read when the task is checked (an earlier task may make it). One of
	// them is required.
	Content *string `yaml:"content"`
	Src     string  `yaml:"src"`
	// Mode is the permission bits ("0644"); unset leaves an existing
	// file's and makes a new one 0644.
	Mode fs.FileMode `yaml:"mode"`
	Owner
	// Parents: missing parent directories are made.
	Parents bool `yaml:"parents"`
	// Seed: an existing file's content is kept; only its mode and owner
	// are put right. With seed, content and src may be left out: a missing
	// file is then made empty.
	Seed bool `yaml:"seed"`
}

type fileArgs struct {
	Path      string `yaml:"path"`
	writeArgs `yaml:",inline"`
}

// copyArgs are file's, with Ansible copy's dest for the path.
type copyArgs struct {
	Dest      string `yaml:"dest"`
	writeArgs `yaml:",inline"`
}

func fileResource(env module.Env, key, path string, a writeArgs) (host.Resource, error) {
	if err := absPath(key, path); err != nil {
		return nil, err
	}
	switch {
	case a.Content != nil && a.Src != "":
		return nil, errors.New("content and src: give one")
	case a.Content == nil && a.Src == "" && !a.Seed:
		return nil, errors.New("content or src is required (or seed)")
	}
	var content []byte
	from := ""
	switch {
	case a.Content != nil:
		content = []byte(*a.Content)
	case a.Src == "": // seed alone
	case filepath.IsAbs(a.Src):
		from = env.Path(a.Src)
	default:
		b, err := readFiles(env, a.Src)
		if err != nil {
			return nil, err
		}
		content = b
	}
	return owned(env, a.Owner, "file "+path, func(o *host.Owner) host.Resource {
		return &host.File{Path: env.Path(path), Content: content, From: from, Mode: a.Mode, Owner: o, Parents: a.Parents, Seed: a.Seed}
	})
}

// readFiles reads a relative src argument from env.Files.
func readFiles(env module.Env, src string) ([]byte, error) {
	if env.Files == nil {
		return nil, fmt.Errorf("src %q: relative, and the playbook has no files/ directory", src)
	}
	name := filepath.ToSlash(filepath.Clean(src))
	if !fs.ValidPath(name) {
		return nil, fmt.Errorf("src %q: not a path inside files/", src)
	}
	b, err := fs.ReadFile(env.Files, name)
	if err != nil {
		return nil, fmt.Errorf("src: %w", err)
	}
	return b, nil
}

func newFile(env module.Env, a fileArgs) (host.Resource, error) {
	return fileResource(env, "path", a.Path, a.writeArgs)
}

func newCopy(env module.Env, a copyArgs) (host.Resource, error) {
	return fileResource(env, "dest", a.Dest, a.writeArgs)
}

type dirArgs struct {
	Path string      `yaml:"path"`
	Mode fs.FileMode `yaml:"mode"`
	Owner
}

func newDir(env module.Env, a dirArgs) (host.Resource, error) {
	if err := absPath("path", a.Path); err != nil {
		return nil, err
	}
	return owned(env, a.Owner, "dir "+a.Path, func(o *host.Owner) host.Resource {
		return &host.Dir{Path: env.Path(a.Path), Mode: a.Mode, Owner: o}
	})
}

type absentArgs struct {
	Path string `yaml:"path"`
	// Recursive: a directory goes with everything in it.
	Recursive bool `yaml:"recursive"`
}

func newAbsent(env module.Env, a absentArgs) (host.Resource, error) {
	if err := absPath("path", a.Path); err != nil {
		return nil, err
	}
	if a.Path == "/" {
		return nil, errors.New("path /: refusing to remove the root")
	}
	return &host.Absent{Path: env.Path(a.Path), Recursive: a.Recursive}, nil
}

type symlinkArgs struct {
	Path   string `yaml:"path"`
	Target string `yaml:"target"`
	// Force: a regular file at path is replaced by the link.
	Force bool `yaml:"force"`
	// Aside: a file or directory at path is moved here first.
	Aside string `yaml:"aside"`
	Owner
}

func newSymlink(env module.Env, a symlinkArgs) (host.Resource, error) {
	if err := absPath("path", a.Path); err != nil {
		return nil, err
	}
	if err := required("target", a.Target); err != nil {
		return nil, err
	}
	aside := ""
	if a.Aside != "" {
		if err := absPath("aside", a.Aside); err != nil {
			return nil, err
		}
		aside = env.Path(a.Aside)
	}
	return owned(env, a.Owner, "link "+a.Path+" -> "+a.Target, func(o *host.Owner) host.Resource {
		return &host.Symlink{Path: env.Path(a.Path), Target: a.Target, Force: a.Force, Aside: aside, Owner: o}
	})
}

type lineArgs struct {
	Path string `yaml:"path"`
	Line string `yaml:"line"`
	// Regexp matches the lines line replaces (or, with state absent,
	// removes).
	Regexp string `yaml:"regexp"`
	// State is present (the default) or absent.
	State string `yaml:"state"`
}

func newLine(env module.Env, a lineArgs) (host.Resource, error) {
	if err := absPath("path", a.Path); err != nil {
		return nil, err
	}
	state, err := choice("state", a.State, "present", "absent")
	if err != nil {
		return nil, err
	}
	var re *regexp.Regexp
	if a.Regexp != "" {
		var err error
		if re, err = regexp.Compile(a.Regexp); err != nil {
			return nil, fmt.Errorf("regexp: %w", err)
		}
	}
	// A block scalar (line: |) ends in a newline that is not part of
	// the line.
	a.Line = strings.TrimSuffix(a.Line, "\n")
	switch {
	case strings.Contains(a.Line, "\n"):
		return nil, errors.New("line: has a newline in it; use block for several lines")
	case state == "present" && a.Line == "":
		return nil, errors.New("line is required")
	case state == "absent" && a.Line == "" && re == nil:
		return nil, errors.New("line or regexp is required")
	}
	return &host.Line{Path: env.Path(a.Path), Line: a.Line, Replace: re, Absent: state == "absent"}, nil
}

type blockArgs struct {
	Path  string `yaml:"path"`
	Block string `yaml:"block"`
	// Marker is the marker lines' text, {mark} standing for BEGIN and
	// END; unset is "# {mark} MANAGED BLOCK".
	Marker string `yaml:"marker"`
	State  string `yaml:"state"`
}

func newBlock(env module.Env, a blockArgs) (host.Resource, error) {
	if err := absPath("path", a.Path); err != nil {
		return nil, err
	}
	state, err := choice("state", a.State, "present", "absent")
	if err != nil {
		return nil, err
	}
	if a.Marker != "" && !strings.Contains(a.Marker, "{mark}") {
		return nil, errors.New("marker: want {mark} in it")
	}
	return &host.Block{Path: env.Path(a.Path), Content: a.Block, Marker: a.Marker, Absent: state == "absent"}, nil
}

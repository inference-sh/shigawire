package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Line is a line in a text file (fstab, mdadm.conf): the first line equal
// to it or matching Replace becomes it, the other such lines go, and a
// file with none gets it appended. The rest of the file is kept as it is.
// The file is replaced atomically, with its mode and owner (a new one:
// 0644, the writer's); a link is followed, so the file it names is the one
// written, and a link to nothing is an error. With Absent, every line
// equal to Line or matching Replace goes, and a missing file is in place.
// A Line with a newline in it is an error (Block is for several lines).
type Line struct {
	Path string
	Line string
	// Replace matches the lines Line stands in for (an older entry for the
	// same mount point); nil: only lines equal to Line.
	Replace *regexp.Regexp
	Absent  bool
}

func (l *Line) ID() string {
	if l.Absent {
		what := l.Line
		if what == "" && l.Replace != nil {
			what = l.Replace.String()
		}
		return "no line in " + l.Path + ": " + what
	}
	return "line in " + l.Path + ": " + l.Line
}

// lineEdit is what Line.Check found: the file to write (links followed)
// and the content it gets.
type lineEdit struct {
	path string
	want []byte
}

func (l *Line) Check(context.Context) (State, error) {
	if strings.Contains(l.Line, "\n") {
		return State{}, fmt.Errorf("line %q has a newline in it: a line is one line (Block is for several)", l.Line)
	}
	path, cur, want, err := l.read()
	switch {
	case err != nil:
		return State{}, err
	case cur == nil && l.Absent:
		return inPlace()
	case l.Absent && !bytes.Equal(cur, want):
		return driftData(lineEdit{path, want}, "remove the line")
	case cur == nil:
		return driftData(lineEdit{path, want}, "create the file with the line")
	case !bytes.Equal(cur, want):
		return driftData(lineEdit{path, want}, "%s", lineDrift(cur, l.Line))
	}
	return inPlace()
}

func (l *Line) Apply(ctx context.Context, st State) (Result, error) {
	if err := writeEdit(ctx, st.Data.(lineEdit)); err != nil {
		return Result{}, err
	}
	return changed("%s", st.Drift)
}

// writeEdit writes e's content to its file, with the file's mode and
// owner (a new one: 0644, the writer's).
func writeEdit(ctx context.Context, e lineEdit) error {
	fi, err := os.Stat(e.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := saveBefore(ctx, e.path); err != nil {
		return err
	}
	mode, owner := attrs{}.forNew(fi, 0o644)
	return writeAtomic(e.path, bytes.NewReader(e.want), mode, owner)
}

// read is the path to write (links followed), the file's content (nil
// when there is no file) and the content it should have.
func (l *Line) read() (path string, cur, want []byte, err error) {
	path, cur, err = readText(l.Path)
	if err != nil {
		return "", nil, nil, err
	}
	if l.Absent {
		return path, cur, withoutLine(cur, l.Line, l.Replace), nil
	}
	return path, cur, WithLine(cur, l.Line, l.Replace), nil
}

// readText is the file to write for a text edit at p (a link followed)
// and its content: nil when there is no file, empty for an empty one.
func readText(p string) (string, []byte, error) {
	path := p
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if path, err = filepath.EvalSymlinks(p); err != nil {
			return "", nil, fmt.Errorf("%s is a link to nothing: %w", p, err)
		}
	}
	cur, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if cur == nil {
		cur = []byte{}
	}
	return path, cur, nil
}

// WithLine is content with line in it, as Line puts it there: the first
// line equal to line or matching replace becomes line, the other such
// lines are dropped, and content with none gets line appended.
func WithLine(content []byte, line string, replace *regexp.Regexp) []byte {
	var out []string
	done := false
	lines, trailing := splitLines(content)
	for _, s := range lines {
		if s == line || (replace != nil && replace.MatchString(s)) {
			if !done {
				out = append(out, line)
				done = true
			}
			continue
		}
		out = append(out, s)
	}
	if !done {
		out = append(out, line)
		trailing = true
	}
	return joinLines(out, trailing)
}

// withoutLine is content without the lines equal to line or matching
// replace ("" and nil match none).
func withoutLine(content []byte, line string, replace *regexp.Regexp) []byte {
	if len(content) == 0 {
		return content
	}
	lines, trailing := splitLines(content)
	var out []string
	for _, s := range lines {
		if (line != "" && s == line) || (replace != nil && replace.MatchString(s)) {
			continue
		}
		out = append(out, s)
	}
	return joinLines(out, trailing)
}

// splitLines is content's lines, and whether it ends in a newline; empty
// content has none.
func splitLines(content []byte) (lines []string, trailing bool) {
	text := string(content)
	if text == "" {
		return nil, false
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n"), strings.HasSuffix(text, "\n")
}

// joinLines is lines joined, ending in a newline when trailing; no lines
// is empty content.
func joinLines(lines []string, trailing bool) []byte {
	if len(lines) == 0 {
		return []byte{}
	}
	s := strings.Join(lines, "\n")
	if trailing {
		s += "\n"
	}
	return []byte(s)
}

func lineDrift(cur []byte, line string) string {
	for _, s := range strings.Split(string(cur), "\n") {
		if s == line {
			return "drop the lines it replaces"
		}
	}
	return "set the line"
}

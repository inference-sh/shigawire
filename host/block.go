package host

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
)

// DefaultMarker is Block's marker when it names none.
const DefaultMarker = "# {mark} MANAGED BLOCK"

// Block is lines in a text file between two marker lines, as Ansible's
// blockinfile puts them: what is between the first begin marker and the
// end marker after it becomes Content, and a file without the markers
// gets them, with Content, appended. Absent removes the markers and what
// is between them. The rest of the file is kept as it is. The file is
// replaced atomically, with its mode and owner (a new one: 0644, the
// writer's); a link is followed, as Line does.
type Block struct {
	Path string
	// Content is the lines between the markers; a trailing newline is
	// dropped.
	Content string
	// Marker is the marker lines' text, {mark} standing for BEGIN in the
	// first and END in the second; "" is DefaultMarker. Two Blocks in one
	// file need two markers.
	Marker string
	Absent bool
}

func (b *Block) ID() string {
	id := "block in " + b.Path
	if b.Marker != "" {
		id += ": " + b.Marker
	}
	if b.Absent {
		id += " absent"
	}
	return id
}

func (b *Block) markers() (begin, end string) {
	m := cmp.Or(b.Marker, DefaultMarker)
	return strings.ReplaceAll(m, "{mark}", "BEGIN"), strings.ReplaceAll(m, "{mark}", "END")
}

func (b *Block) Check(context.Context) (State, error) {
	path, cur, err := readText(b.Path)
	if err != nil {
		return State{}, err
	}
	if cur == nil && b.Absent {
		return inPlace()
	}
	begin, end := b.markers()
	want, err := withBlock(cur, begin, end, b.Content, b.Absent)
	switch {
	case err != nil:
		return State{}, fmt.Errorf("%s: %w", path, err)
	case cur == nil:
		return driftData(lineEdit{path, want}, "create the file with the block")
	case bytes.Equal(cur, want):
		return inPlace()
	case b.Absent:
		return driftData(lineEdit{path, want}, "remove the block")
	case !bytes.Contains(cur, []byte(begin)):
		return driftData(lineEdit{path, want}, "add the block")
	}
	return driftData(lineEdit{path, want}, "set the block")
}

func (b *Block) Apply(ctx context.Context, st State) (Result, error) {
	if err := writeEdit(ctx, st.Data.(lineEdit)); err != nil {
		return Result{}, err
	}
	return changed("%s", st.Drift)
}

// withBlock is content with block between the marker lines begin and end,
// as Block puts it there; absent removes the markers and what is between
// them, and with no block leaves content as it is. A begin marker with no
// end marker after it is an error.
func withBlock(content []byte, begin, end, block string, absent bool) ([]byte, error) {
	text := string(content)
	var lines []string
	if text != "" {
		lines = strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	}
	var mine []string
	if !absent {
		mine = append(mine, begin)
		if block = strings.TrimSuffix(block, "\n"); block != "" {
			mine = append(mine, strings.Split(block, "\n")...)
		}
		mine = append(mine, end)
	}
	b, e := -1, -1
	for i, l := range lines {
		if b < 0 && l == begin {
			b = i
		} else if b >= 0 && l == end {
			e = i
			break
		}
	}
	var out []string
	switch {
	case b >= 0 && e < 0:
		return nil, fmt.Errorf("%q has no %q after it", begin, end)
	case b >= 0:
		out = slices.Concat(lines[:b], mine, lines[e+1:])
	case absent:
		// No block to remove: the content as it is.
		return content, nil
	default:
		out = append(lines, mine...)
	}
	if len(out) == 0 {
		return []byte{}, nil
	}
	return []byte(strings.Join(out, "\n") + "\n"), nil
}

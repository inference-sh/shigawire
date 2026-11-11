//go:build unix && !linux

package host

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// eachEntry calls f with each name in the directory fd, its type unknown
// (DT_UNKNOWN: the caller stats it), reading in batches. fd stays open.
func eachEntry(fd int, _ []byte, f func(name string, typ uint8) error) error {
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	d := os.NewFile(uintptr(dup), "")
	defer d.Close()
	for {
		names, err := d.Readdirnames(4096)
		for _, n := range names {
			if err := f(n, unix.DT_UNKNOWN); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(names) == 0) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

package host

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// eachEntry calls f with each name in the directory fd and its type
// (d_type: unix.DT_DIR, ...; DT_UNKNOWN when the filesystem does not
// say), reading with getdents into buf. fd is fresh: read from its start.
func eachEntry(fd int, buf []byte, f func(name string, typ uint8) error) error {
	for {
		n, err := unix.ReadDirent(fd, buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return nil
		}
		// struct linux_dirent64: d_ino u64, d_off s64, d_reclen u16,
		// d_type u8, d_name (NUL-terminated).
		for b := buf[:n]; len(b) >= 19; {
			reclen := int(binary.NativeEndian.Uint16(b[16:18]))
			if reclen < 19 || reclen > len(b) {
				break
			}
			typ, name := b[18], b[19:reclen]
			for i, c := range name {
				if c == 0 {
					name = name[:i]
					break
				}
			}
			if s := string(name); s != "." && s != ".." {
				if err := f(s, typ); err != nil {
					return err
				}
			}
			b = b[reclen:]
		}
	}
}

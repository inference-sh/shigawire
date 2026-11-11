package host

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOSExec(t *testing.T) {
	ctx := context.Background()
	e := OSExec{}
	o := e.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "pwd; echo $RES_TEST; echo err >&2"}, Env: []string{"RES_TEST=set"}})
	if !o.OK() || o.Out != "/\nset\nerr\n" {
		t.Fatalf("%+v", o)
	}
	o = e.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "echo one; echo two; exit 3"}})
	if o.OK() || o.Code != 3 || o.Err.Error() != "sh: exit 3: one | two" {
		t.Fatalf("%+v", o)
	}
	// No output: the error ends at the exit, as the fakes' does.
	if o = e.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "exit 4"}}); o.Err.Error() != "sh: exit 4" {
		t.Fatalf("%q", o.Err)
	}
	o = e.Run(ctx, Cmd{Name: "sleep", Args: []string{"5"}, Timeout: 50 * time.Millisecond})
	if o.Code != -1 || !strings.Contains(o.Err.Error(), "timed out after 50ms") {
		t.Fatalf("%+v", o)
	}
	if o = e.Run(ctx, Cmd{Name: "no-such-binary-res"}); o.Code != -1 || o.Err == nil {
		t.Fatalf("%+v", o)
	}
}

// A timeout ends what the command started too, and a command that leaves
// something running with its output open returns all the same.
func TestOSExec_ProcessGroup(t *testing.T) {
	ctx := context.Background()
	defer func(old time.Duration) { killGrace = old }(killGrace)
	killGrace = 200 * time.Millisecond
	e := OSExec{}
	start := time.Now()
	o := e.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & sleep 30; wait"}, Timeout: 100 * time.Millisecond})
	if o.Code != -1 || !strings.Contains(o.Err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v after %s", o, time.Since(start))
	}
	// A child that ignores SIGTERM is killed after the grace.
	start = time.Now()
	o = e.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "trap '' TERM; sleep 30 & wait"}, Timeout: 100 * time.Millisecond})
	if o.OK() || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v after %s", o, time.Since(start))
	}
	start = time.Now()
	o = e.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "echo started; sleep 30 &"}})
	if !o.OK() || o.Out != "started\n" || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v after %s", o, time.Since(start))
	}
}

// ctx ending is ctx's error, not the command's timeout.
func TestOSExec_ParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	o := OSExec{}.Run(ctx, Cmd{Name: "sleep", Args: []string{"5"}, Timeout: time.Hour})
	if !errors.Is(o.Err, context.DeadlineExceeded) || strings.Contains(o.Err.Error(), "timed out after") {
		t.Fatalf("%+v", o)
	}
}

// Path finds the command and is its PATH, whatever this process's.
func TestOSExec_Path(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "res-tool"), []byte("#!/bin/sh\necho \"$PATH\"\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "not-exec"), []byte("x"), 0o644)
	t.Setenv("PATH", "/nonexistent")
	e := OSExec{Path: dir + ":/usr/bin:/bin"}
	if o := e.Run(context.Background(), Cmd{Name: "res-tool"}); !o.OK() || o.Out != dir+":/usr/bin:/bin\n" {
		t.Fatalf("%+v", o)
	}
	if o := e.Run(context.Background(), Cmd{Name: "not-exec"}); !errors.Is(o.Err, fs.ErrNotExist) {
		t.Fatalf("%+v", o)
	}
}

// A Dir that is not there is said so (ErrNoDir), not as the binary
// missing: exec reports a failed chdir as fork/exec of the binary.
func TestOSExec_NoDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x")
	o := OSExec{}.Run(context.Background(), Cmd{Name: "true", Dir: dir})
	if o.Code != -1 || !errors.Is(o.Err, ErrNoDir) || o.Err.Error() != "run true in "+dir+": the directory does not exist" {
		t.Fatalf("%+v", o)
	}
}

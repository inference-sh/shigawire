package host

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
)

// Command is a command that puts something in place a resource here does
// not model (an installer, mdadm --create, update-initramfs). Without a
// guard it is an action: it runs every time and reports changed. A guard
// says when it is in place already, and its drift says what the guard
// found: "no /usr/bin/docker" (Creates), "/opt/x exists" (Removes), or
// the last line Unless printed ("swarm inactive: init"), else its exit
// code ("unless: exit 1").
type Command struct {
	Exec Exec
	Cmd  Cmd
	// Creates: in place when this path exists (what the command makes).
	Creates string
	// Removes: in place when this path does not exist.
	Removes string
	// Unless: in place when this command exits 0.
	Unless *Cmd
	// Watch: the command changed something only when one of these files'
	// content did (it rewrites a config with the same settings, or not).
	Watch []string
	// NoChange: the command only reads or verifies; it reports ok.
	NoChange bool
	// Apt, when set, is the Apt whose read of the installed packages the
	// command may change (an installer that runs apt-get): it reads them
	// again after the command ran.
	Apt *Apt
}

func (c *Command) ID() string { return "command " + c.Cmd.String() }

func (c *Command) Check(ctx context.Context) (State, error) {
	if c.Creates != "" {
		if ok, err := exists(c.Creates); err != nil || ok {
			return State{InPlace: ok}, err
		}
	}
	if c.Removes != "" {
		if ok, err := exists(c.Removes); err != nil || !ok {
			return State{InPlace: err == nil}, err
		}
	}
	if c.Unless != nil {
		o := c.Exec.Run(Aside(ctx), *c.Unless)
		if o.OK() {
			return inPlace()
		}
		if said := LastLines(o.Out, 1); said != "" {
			return drift("%s", said)
		}
		return drift("unless: exit %d", o.Code)
	}
	switch {
	case c.Creates != "":
		return drift("no %s", c.Creates)
	case c.Removes != "":
		return drift("%s exists", c.Removes)
	}
	return action("run %s", c.Cmd.Name)
}

func (c *Command) Apply(ctx context.Context, _ State) (Result, error) {
	if c.Apt != nil {
		defer c.Apt.Invalidate()
	}
	before := sums(c.Watch)
	o, err := run(ctx, c.Exec, c.Cmd)
	if err != nil {
		return Result{}, err
	}
	msg := LastLines(o.Out, 1)
	if c.NoChange {
		return unchanged(msg)
	}
	if len(c.Watch) > 0 {
		after := sums(c.Watch)
		var diff []string
		for i, p := range c.Watch {
			if before[i] != after[i] {
				diff = append(diff, p)
			}
		}
		if len(diff) == 0 {
			return unchanged("no change to " + strings.Join(c.Watch, ", "))
		}
		return changed("changed %s", strings.Join(diff, ", "))
	}
	return changed("%s", msg)
}

// sums are the files' hashes, "" for one that cannot be read.
func sums(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i], _ = fileSum(p)
	}
	return out
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

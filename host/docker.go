package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

func docker(args ...string) Cmd {
	return Cmd{Name: "docker", Args: args, Timeout: 5 * time.Minute}
}

// swarmState is the node's swarm state as docker info has it: inactive,
// pending, active, error or locked.
func swarmState(ctx context.Context, x Exec) (string, error) {
	o, err := run(ctx, x, docker("info", "--format", "{{.Swarm.LocalNodeState}}"))
	if err != nil {
		return "", fmt.Errorf("docker info: %w", err)
	}
	return strings.TrimSpace(o.Out), nil
}

// Swarm is the Docker node in a swarm: one it inits (a manager of a new
// swarm), or one it joins with JoinToken at RemoteAddrs. An active node is
// in place, whichever swarm it is in; an inactive one inits or joins. A
// node pending, in error or locked is a Refusal: what to do then is the
// operator's call, and no task before this one changes it.
type Swarm struct {
	Exec Exec
	// Join: join a swarm; else init one.
	Join bool
	// AdvertiseAddr and ListenAddr go to docker swarm init or join; ""
	// leaves them to docker.
	AdvertiseAddr string
	ListenAddr    string
	// RemoteAddrs are managers of the swarm to join (host:port).
	RemoteAddrs []string
	// JoinToken is the swarm's worker or manager token. It is not in the
	// resource's ID or messages.
	JoinToken string
}

func (s *Swarm) ID() string {
	if s.Join {
		return "docker swarm joined at " + strings.Join(s.RemoteAddrs, ",")
	}
	return "docker swarm"
}

// Validate reports a join without a token or a manager's address, or an
// init with either.
func (s *Swarm) Validate() error {
	switch {
	case s.Join && (s.JoinToken == "" || len(s.RemoteAddrs) == 0):
		return errors.New("docker swarm join needs a join token and a manager's address")
	case !s.Join && (s.JoinToken != "" || len(s.RemoteAddrs) > 0):
		return errors.New("a join token and managers' addresses are for docker swarm join")
	}
	return nil
}

func (s *Swarm) Check(ctx context.Context) (State, error) {
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	st, err := swarmState(ctx, s.Exec)
	if err != nil {
		return State{}, err
	}
	switch st {
	case "active":
		return inPlace()
	case "inactive":
		if s.Join {
			return drift("swarm inactive: join")
		}
		return drift("swarm inactive: init")
	case "pending":
		return State{}, Refusal(errors.New("the node is pending: it is joining a swarm whose managers it has not reached; docker swarm leave to give up"))
	case "error", "locked":
		return State{}, Refusal(fmt.Errorf("the node's swarm state is %s: docker swarm unlock, or leave it, by hand", st))
	}
	return State{}, Refusal(fmt.Errorf("the node's swarm state is %q", st))
}

func (s *Swarm) Apply(ctx context.Context, st State) (Result, error) {
	var args []string
	if s.Join {
		args = []string{"swarm", "join", "--token", s.JoinToken}
	} else {
		args = []string{"swarm", "init"}
	}
	if s.AdvertiseAddr != "" {
		args = append(args, "--advertise-addr", s.AdvertiseAddr)
	}
	if s.ListenAddr != "" {
		args = append(args, "--listen-addr", s.ListenAddr)
	}
	if s.Join {
		args = append(args, s.RemoteAddrs...)
	}
	if o := s.Exec.Run(ctx, docker(args...)); o.Err != nil {
		// Not o.Err: its message may hold the command line, and the token.
		return Result{}, fmt.Errorf("docker swarm %s: exit %d: %s", args[1], o.Code, LastLines(o.Out, 2))
	}
	got, err := swarmState(ctx, s.Exec)
	if err != nil {
		return Result{}, err
	}
	if got != "active" {
		return Result{}, fmt.Errorf("docker swarm %s ran, and the node is %s", args[1], got)
	}
	return changed("%s", st.Drift)
}

// Compose is a Docker Compose project up (every service has a container
// running, or exited 0, made from the service's current config), or down
// (Absent: no containers). Check compares the config hash compose puts on
// each container (the com.docker.compose.config-hash label) with docker
// compose config --hash; Apply runs docker compose up -d, or down.
type Compose struct {
	Exec Exec
	// Dir is the project directory on the host; the commands run in it.
	Dir string
	// Files are the compose files (-f); none: compose's default in Dir.
	Files []string
	// Project is the project name (-p); "": compose's default.
	Project string
	Absent  bool
}

func (c *Compose) ID() string {
	id := "compose " + c.Dir
	if c.Project != "" {
		id = "compose " + c.Project
	}
	if c.Absent {
		id += " down"
	}
	return id
}

func (c *Compose) cmd(timeout time.Duration, args ...string) Cmd {
	base := []string{"compose"}
	for _, f := range c.Files {
		base = append(base, "-f", f)
	}
	if c.Project != "" {
		base = append(base, "-p", c.Project)
	}
	return Cmd{Name: "docker", Args: append(base, args...), Dir: c.Dir, Timeout: timeout}
}

// composeContainer is one line of docker compose ps --format json.
type composeContainer struct {
	Service  string
	State    string
	ExitCode int
	Labels   string
}

func (k composeContainer) label(name string) string {
	for _, kv := range strings.Split(k.Labels, ",") {
		if n, v, ok := strings.Cut(kv, "="); ok && n == name {
			return v
		}
	}
	return ""
}

// parseComposePS reads docker compose ps --format json: one object a line
// (compose 2.21 and later), or one array.
func parseComposePS(out string) ([]composeContainer, error) {
	out = strings.TrimSpace(out)
	var cs []composeContainer
	if strings.HasPrefix(out, "[") {
		err := json.Unmarshal([]byte(out), &cs)
		return cs, err
	}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		var k composeContainer
		if err := json.Unmarshal([]byte(l), &k); err != nil {
			return nil, fmt.Errorf("docker compose ps: %w", err)
		}
		cs = append(cs, k)
	}
	return cs, nil
}

func (c *Compose) Check(ctx context.Context) (State, error) {
	o, err := run(ctx, c.Exec, c.cmd(time.Minute, "ps", "--all", "--format", "json"))
	if err != nil {
		return State{}, err
	}
	cs, err := parseComposePS(o.Out)
	if err != nil {
		return State{}, err
	}
	if c.Absent {
		if len(cs) == 0 {
			return inPlace()
		}
		return drift("%d containers", len(cs))
	}
	o, err = run(ctx, c.Exec, c.cmd(time.Minute, "config", "--hash", "*"))
	if err != nil {
		return State{}, err
	}
	var d []string // one line for a service's replicas
	for _, l := range strings.Split(strings.TrimSpace(o.Out), "\n") {
		svc, hash, ok := strings.Cut(strings.TrimSpace(l), " ")
		if !ok {
			continue
		}
		if !slices.ContainsFunc(cs, func(k composeContainer) bool { return k.Service == svc }) {
			d = appendNew(d, svc+" not created")
			continue
		}
		for _, k := range cs {
			switch {
			case k.Service != svc:
			case k.label("com.docker.compose.config-hash") != hash:
				d = appendNew(d, svc+" config changed")
			case k.State == "running", k.State == "exited" && k.ExitCode == 0:
			default:
				d = appendNew(d, svc+" "+k.State)
			}
		}
	}
	if len(d) == 0 {
		return inPlace()
	}
	return drift("%s", strings.Join(d, "; "))
}

func (c *Compose) Apply(ctx context.Context, st State) (Result, error) {
	args := []string{"up", "--detach", "--remove-orphans"}
	if c.Absent {
		args = []string{"down", "--remove-orphans"}
	}
	if _, err := run(ctx, c.Exec, c.cmd(30*time.Minute, args...)); err != nil {
		return Result{}, err
	}
	return changed("%s", st.Drift)
}

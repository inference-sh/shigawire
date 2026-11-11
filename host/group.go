package host

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// GroupMember is a user in a supplementary group (docker), added without
// leaving the others. The user's new sessions have the group; running
// ones do not.
type GroupMember struct {
	Exec  Exec
	User  string
	Group string
}

func (g *GroupMember) ID() string { return "user " + g.User + " in group " + g.Group }

// InGroup reports whether user is in group as the system's user database
// has it now (id, through NSS: /etc/group, LDAP), whatever a running
// session of theirs picked up.
func InGroup(ctx context.Context, x Exec, user, group string) (bool, error) {
	o, err := run(ctx, x, Cmd{Name: "id", Args: []string{"-nG", user}, Timeout: time.Minute})
	if err != nil {
		return false, fmt.Errorf("the groups of %s: %w", user, err)
	}
	return slices.Contains(strings.Fields(o.Out), group), nil
}

func (g *GroupMember) Check(ctx context.Context) (State, error) {
	if in, err := InGroup(ctx, g.Exec, g.User, g.Group); err != nil || in {
		return State{InPlace: in}, err
	}
	return drift("add %s to %s", g.User, g.Group)
}

func (g *GroupMember) Apply(ctx context.Context, _ State) (Result, error) {
	if _, err := run(ctx, g.Exec, Cmd{Name: "usermod", Args: []string{"-a", "-G", g.Group, g.User}, Timeout: time.Minute}); err != nil {
		return Result{}, err
	}
	return changed("added")
}

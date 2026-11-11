//go:build !unix

package host

import (
	"context"
	"errors"
)

// OwnTree: files have no numeric owner here.
type OwnTree struct {
	Path      string
	Owner     Owner
	Hardlinks bool
	Marker    string
}

var errNoOwners = errors.New("files have no numeric owner on this platform")

func (t *OwnTree) ID() string                                   { return "tree " + t.Path }
func (t *OwnTree) Check(context.Context) (State, error)         { return State{}, errNoOwners }
func (t *OwnTree) Apply(context.Context, State) (Result, error) { return Result{}, errNoOwners }

// ProtectedHardlinks: no such setting here.
func ProtectedHardlinks(string) bool { return false }

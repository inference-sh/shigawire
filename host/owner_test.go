package host

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"testing"
)

func TestUserOwner(t *testing.T) {
	o, err := UserOwner(&user.User{Username: "ok", Uid: "1000", Gid: "1001"})
	if err != nil || o != (Owner{UID: 1000, GID: 1001}) {
		t.Fatalf("%v %v", o, err)
	}
	// A Windows SID is no numeric owner.
	if _, err := UserOwner(&user.User{Username: "ok", Uid: "S-1-5-21", Gid: "1001"}); err == nil {
		t.Fatal("a SID is a uid")
	}
	if _, err := UserOwner(&user.User{Username: "ok", Uid: "1000", Gid: "x"}); err == nil {
		t.Fatal("x is a gid")
	}
}

// An owner with GID -1 leaves the group as it is: only the uid is
// compared.
func TestAttrsAnyGroup(t *testing.T) {
	fi, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	own, _ := FileOwner(fi)
	if d := (attrs{owner: &Owner{UID: own.UID, GID: -1}}).drift(fi); len(d) != 0 {
		t.Fatal(d)
	}
	if d := (attrs{owner: &Owner{UID: own.UID + 1, GID: -1}}).drift(fi); len(d) != 1 || d[0] != fmt.Sprintf("owner %d, want %d", own.UID, own.UID+1) {
		t.Fatal(d)
	}
}

// LookupOwner takes a name or a number; a number with no entry is that
// id, and an owner number with no group leaves the group (GID -1).
func TestLookupOwner(t *testing.T) {
	ex := &fakeExec{respond: func(line string) Output {
		switch line {
		case "getent passwd app", "getent passwd 1000":
			return Output{Out: "app:x:1000:1001::/home/app:/bin/sh\n"}
		case "getent group docker":
			return Output{Out: "docker:x:999:\n"}
		}
		return Output{Code: 2}
	}}
	for _, c := range []struct {
		owner, group string
		want         Owner
		err          string
	}{
		{owner: "app", want: Owner{1000, 1001}},
		{owner: "1000", want: Owner{1000, 1001}},
		{owner: "app", group: "docker", want: Owner{1000, 999}},
		{owner: "65532", want: Owner{65532, -1}},
		{owner: "65532", group: "65533", want: Owner{65532, 65533}},
		{owner: "0", group: "docker", want: Owner{0, 999}},
		{owner: "ghost", err: "there is no user ghost"},
		{owner: "app", group: "ghosts", err: "there is no group ghosts"},
		{owner: "-1", err: "there is no user -1"},
		{owner: "4294967295", err: "there is no user 4294967295"},
		{owner: "2147483647", want: Owner{2147483647, -1}},
		{owner: "2147483648", err: "there is no user 2147483648"},
		{owner: "1000", group: "2147483648", err: "there is no group 2147483648"},
		{group: "docker", err: "group needs owner"},
	} {
		got, err := LookupOwner(context.Background(), ex, c.owner, c.group)
		switch {
		case c.err != "" && (err == nil || err.Error() != c.err):
			t.Errorf("%s:%s: %v %v, want %s", c.owner, c.group, got, err, c.err)
		case c.err == "" && (err != nil || *got != c.want):
			t.Errorf("%s:%s: %v %v, want %v", c.owner, c.group, got, err, c.want)
		}
	}
	if got, err := LookupOwner(context.Background(), ex, "", ""); got != nil || err != nil {
		t.Fatal(got, err)
	}
}

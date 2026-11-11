package host

import (
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

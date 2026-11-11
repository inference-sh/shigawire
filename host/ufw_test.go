package host

import (
	"fmt"
	"testing"
)

func TestUFWTuples(t *testing.T) {
	for _, c := range []struct {
		r      UFWRule
		v4, v6 string
	}{
		{UFWRule{Action: "allow", Proto: "tcp", Port: "22"}, "allow tcp 22 0.0.0.0/0 any 0.0.0.0/0 in", "allow tcp 22 ::/0 any ::/0 in"},
		{UFWRule{Action: "limit", Port: "22", From: "10.1.2.3/32"}, "limit any 22 0.0.0.0/0 any 10.1.2.3 in", ""},
		{UFWRule{Action: "deny", Direction: "out", To: "fd00::/8", Proto: "udp", Port: "53,853"}, "", "deny udp 53,853 fd00::/8 any ::/0 out"},
		{UFWRule{Action: "allow", Interface: "wg0", Comment: "vpn"}, "allow any any 0.0.0.0/0 any 0.0.0.0/0 in_wg0 comment=76706e", "allow any any ::/0 any ::/0 in_wg0 comment=76706e"},
	} {
		v4, v6, err := c.r.Tuples(true)
		if err != nil || v4 != c.v4 || v6 != c.v6 {
			t.Errorf("%+v: %q %q %v", c.r, v4, v6, err)
		}
	}
	// IPv6 off: a rule for any address is in user.rules only.
	if _, v6, _ := (UFWRule{Action: "allow", Port: "80"}).Tuples(false); v6 != "" {
		t.Fatal(v6)
	}
}

// Two rules that differ only in action or comment are one rule to ufw:
// the second would update the first, every run. So is an Absent rule
// that deletes a listed one. An Absent rule with another action deletes
// what was there before the listed rule replaces it: a move from limit
// to allow.
func TestUFWOneRuleTwice(t *testing.T) {
	u := &UFW{Rules: []UFWRule{
		{Action: "allow", Proto: "tcp", Port: "80"},
		{Action: "limit", Proto: "tcp", Port: "22"},
		{Action: "allow", Proto: "tcp", Port: "22", Comment: "ssh"},
	}}
	if err := u.Validate(); err == nil || err.Error() != "ufw rules 2 and 3 are one rule to ufw (they differ only in action or comment)" {
		t.Fatal(err)
	}
	u.Rules[2].Proto = "udp"
	if err := u.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		rules []UFWRule
		err   string
	}{
		{[]UFWRule{
			{Action: "limit", Proto: "tcp", Port: "22", Absent: true},
			{Action: "allow", Proto: "tcp", Port: "22"},
		}, ""},
		{[]UFWRule{
			{Action: "allow", Proto: "tcp", Port: "22", Comment: "old", Absent: true},
			{Action: "allow", Proto: "tcp", Port: "22", Comment: "ssh"},
		}, ""},
		{[]UFWRule{
			{Action: "allow", Proto: "tcp", Port: "22", Absent: true},
			{Action: "deny", Proto: "tcp", Port: "22", Absent: true},
		}, ""},
		{[]UFWRule{
			{Action: "allow", Proto: "tcp", Port: "22", Comment: "ssh"},
			{Action: "allow", Proto: "tcp", Port: "22", Absent: true},
		}, "ufw rule 2 deletes rule 1"},
		{[]UFWRule{
			{Action: "allow", Proto: "tcp", Port: "22", Comment: "ssh", Absent: true},
			{Action: "allow", Proto: "tcp", Port: "22", Comment: "ssh"},
		}, "ufw rule 1 deletes rule 2"},
	} {
		err := (&UFW{Rules: c.rules}).Validate()
		if got := fmt.Sprint(err); (c.err == "" && err != nil) || (c.err != "" && got != c.err) {
			t.Errorf("%+v: %v, want %q", c.rules, err, c.err)
		}
	}
}

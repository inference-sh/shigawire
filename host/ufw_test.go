package host

import "testing"

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
// the second would update the first, every run.
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
}

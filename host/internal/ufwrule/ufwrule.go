// Package ufwrule is how ufw stores its rules and policies, shared by
// host.UFW, which reads them, and the simulated host, which writes them.
package ufwrule

import (
	"encoding/hex"
	"strconv"
	"strings"
)

// Tuple is a "### tuple ###" line (the prefix cut) in three: the action,
// what tells the rule from others to ufw (all of it but the action and
// the comment), and the comment ("" or comment=HEX).
type Tuple struct{ Action, Key, Comment string }

// Parse splits the tuple line t.
func Parse(t string) Tuple {
	f := strings.Fields(t)
	if len(f) == 0 {
		return Tuple{}
	}
	var c string
	if last := f[len(f)-1]; strings.HasPrefix(last, "comment=") {
		c, f = last, f[:len(f)-1]
	}
	return Tuple{Action: f[0], Key: strings.Join(f[1:], " "), Comment: c}
}

// CommentText is the comment as written: none, or quoted.
func (t Tuple) CommentText() string {
	h, ok := strings.CutPrefix(t.Comment, "comment=")
	if !ok {
		return "none"
	}
	if b, err := hex.DecodeString(h); err == nil {
		return strconv.Quote(string(b))
	}
	return h
}

// Deletes reports whether ufw delete of the rule t deletes got: the same
// rule with t's action, and t's comment when t has one.
func (t Tuple) Deletes(got Tuple) bool {
	return got.Key == t.Key && got.Action == t.Action && (t.Comment == "" || got.Comment == t.Comment)
}

// DefaultKey is the /etc/default/ufw key of a direction's default policy
// (incoming, outgoing, routed); "" for another direction.
func DefaultKey(direction string) string {
	return map[string]string{
		"incoming": "DEFAULT_INPUT_POLICY",
		"outgoing": "DEFAULT_OUTPUT_POLICY",
		"routed":   "DEFAULT_FORWARD_POLICY",
	}[direction]
}

// PolicyValue is how /etc/default/ufw writes a policy (allow, deny,
// reject); "" for another.
func PolicyValue(policy string) string {
	return map[string]string{"allow": "ACCEPT", "deny": "DROP", "reject": "REJECT"}[policy]
}

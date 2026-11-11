package builtin

import (
	"errors"

	"github.com/inference-sh/shigawire/host"
	"github.com/inference-sh/shigawire/module"
)

func registerUFW(r *module.Registry) {
	r.Register("ufw", module.Typed(newUFW))
}

type ufwRuleArgs struct {
	// Rule is allow, deny, reject or limit.
	Rule string `yaml:"rule"`
	// Direction is in (the default) or out.
	Direction string `yaml:"direction"`
	Interface string `yaml:"interface"`
	// Proto is tcp, udp or any (the default).
	Proto string `yaml:"proto"`
	// From and To are an address or a network; unset is any.
	From string `yaml:"from"`
	To   string `yaml:"to"`
	// Port is the destination port: a number, a range a:b, or a list.
	Port    string `yaml:"port"`
	Comment string `yaml:"comment"`
	// State is present (the default) or absent.
	State string `yaml:"state"`
}

type ufwArgs struct {
	// Default is the policy by direction: incoming, outgoing, routed to
	// allow, deny or reject.
	Default map[string]string `yaml:"default"`
	// Enabled: ufw is enabled now and at boot. Unset or false leaves it.
	Enabled bool          `yaml:"enabled"`
	Rules   []ufwRuleArgs `yaml:"rules"`
}

func newUFW(env module.Env, a ufwArgs) (host.Resource, error) {
	if len(a.Default) == 0 && !a.Enabled && len(a.Rules) == 0 {
		return nil, errors.New("default, enabled or rules is required")
	}
	u := &host.UFW{Exec: env.Exec, Defaults: a.Default, Enabled: a.Enabled, Root: env.Root}
	for _, r := range a.Rules {
		state := r.State
		if state == "" {
			state = "present"
		}
		if err := oneOf("state", state, "present", "absent"); err != nil {
			return nil, err
		}
		u.Rules = append(u.Rules, host.UFWRule{Action: r.Rule, Direction: r.Direction, Interface: r.Interface,
			Proto: r.Proto, From: r.From, To: r.To, Port: r.Port, Comment: r.Comment, Absent: state == "absent"})
	}
	if err := u.Validate(); err != nil {
		return nil, err
	}
	return u, nil
}

package host

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// UFW is the ufw firewall's default policies, its rules, and whether it
// is enabled. Check reads what ufw has from its own files: the policies
// from /etc/default/ufw, enabled from /etc/ufw/ufw.conf, the rules from
// the "### tuple ###" lines of /etc/ufw/user.rules and user6.rules. Apply
// runs ufw for what differs: the policies, then each rule in order, then
// enable last, so the rules that let the operator in are there before it.
// After it ran, Check reads the files again, and what still differs (a
// rule ufw stored in another form) is an error.
//
// Rules are compared as ufw stores them, so a rule names ports by number
// (not by service or app name), and addresses as ufw writes them: a
// network masked ("10.1.2.0/24"), a single address without its prefix
// length.
type UFW struct {
	Exec Exec
	// Defaults are the policies by direction (incoming, outgoing,
	// routed): allow, deny or reject. A direction left out is left as it
	// is.
	Defaults map[string]string
	// Enabled: ufw is enabled, now and at boot. False leaves it as it is.
	Enabled bool
	Rules   []UFWRule
	// Root is where the host's files are ("" is /).
	Root string
}

// UFWRule is one ufw rule, as ufw allow and its kin take it.
type UFWRule struct {
	// Action is allow, deny, reject or limit.
	Action string
	// Direction is in ("" is in) or out.
	Direction string
	// Interface, when set, is the interface the rule is on.
	Interface string
	// Proto is tcp or udp; "" or any is both.
	Proto string
	// From and To are an address or a network; "" or any is any.
	From, To string
	// Port is the destination port: "22", a range "6000:6007", a list
	// "80,443" (a range or a list needs a Proto); "" is any.
	Port    string
	Comment string
	// Absent: the rule is deleted when ufw has it.
	Absent bool
}

var ufwPolicy = map[string]string{"allow": "ACCEPT", "deny": "DROP", "reject": "REJECT"}

var ufwDirection = map[string]string{
	"incoming": "DEFAULT_INPUT_POLICY",
	"outgoing": "DEFAULT_OUTPUT_POLICY",
	"routed":   "DEFAULT_FORWARD_POLICY",
}

var (
	ufwPort  = regexp.MustCompile(`^[0-9]+(:[0-9]+)?(,[0-9]+(:[0-9]+)?)*$`)
	ufwIface = regexp.MustCompile(`^[A-Za-z0-9_.+-]+$`)
)

func (u *UFW) ID() string { return "ufw" }

func (u *UFW) path(p string) string { return filepath.Join(cmp.Or(u.Root, "/"), p) }

// Validate reports the first default or rule ufw would not take.
func (u *UFW) Validate() error {
	for d, p := range u.Defaults {
		if ufwDirection[d] == "" {
			return fmt.Errorf("ufw default %q: want incoming, outgoing or routed", d)
		}
		if ufwPolicy[p] == "" {
			return fmt.Errorf("ufw default %s %q: want allow, deny or reject", d, p)
		}
	}
	for i, r := range u.Rules {
		if _, _, err := r.Tuples(true); err != nil {
			return fmt.Errorf("ufw rule %d: %w", i+1, err)
		}
	}
	return nil
}

// ufwAddr is an address or network as ufw writes it, and whether it is
// IPv6; any is "".
func ufwAddr(s string) (addr string, v6 bool, err error) {
	if s == "" || s == "any" {
		return "", false, nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String(), a.Is6(), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return "", false, fmt.Errorf("%q is not an address or a network", s)
	}
	p = p.Masked()
	if p.IsSingleIP() {
		return p.Addr().String(), p.Addr().Is6(), nil
	}
	return p.String(), p.Addr().Is6(), nil
}

// Tuples are the rule's "### tuple ###" lines as ufw writes them, without
// that prefix, for user.rules (v4) and user6.rules (v6): a rule with an
// address has one family, one with none both (v6 only when ufw has IPv6
// on).
func (r UFWRule) Tuples(ipv6 bool) (v4, v6 string, err error) {
	switch r.Action {
	case "allow", "deny", "reject", "limit":
	default:
		return "", "", fmt.Errorf("action %q: want allow, deny, reject or limit", r.Action)
	}
	dir := cmp.Or(r.Direction, "in")
	if dir != "in" && dir != "out" {
		return "", "", fmt.Errorf("direction %q: want in or out", r.Direction)
	}
	if r.Interface != "" {
		if !ufwIface.MatchString(r.Interface) {
			return "", "", fmt.Errorf("interface %q", r.Interface)
		}
		dir += "_" + r.Interface
	}
	proto := cmp.Or(r.Proto, "any")
	if proto != "any" && proto != "tcp" && proto != "udp" {
		return "", "", fmt.Errorf("proto %q: want tcp, udp or any", r.Proto)
	}
	port := cmp.Or(r.Port, "any")
	if r.Port != "" && !ufwPort.MatchString(r.Port) {
		return "", "", fmt.Errorf("port %q: want a number, a range a:b or a list of them", r.Port)
	}
	if strings.ContainsAny(r.Port, ":,") && proto == "any" {
		return "", "", fmt.Errorf("port %q: a range or a list needs proto tcp or udp", r.Port)
	}
	from, fromV6, err := ufwAddr(r.From)
	if err != nil {
		return "", "", fmt.Errorf("from: %w", err)
	}
	to, toV6, err := ufwAddr(r.To)
	if err != nil {
		return "", "", fmt.Errorf("to: %w", err)
	}
	if from != "" && to != "" && fromV6 != toV6 {
		return "", "", errors.New("from and to are of different IP versions")
	}
	if strings.ContainsAny(r.Comment, "\n'") {
		return "", "", errors.New("a comment with a newline or a quote")
	}
	tuple := func(anyAddr string) string {
		t := fmt.Sprintf("%s %s %s %s any %s %s", r.Action, proto, port, cmp.Or(to, anyAddr), cmp.Or(from, anyAddr), dir)
		if r.Comment != "" {
			t += " comment=" + hex.EncodeToString([]byte(r.Comment))
		}
		return t
	}
	switch {
	case from != "" && fromV6, to != "" && toV6:
		return "", tuple("::/0"), nil
	case from != "" || to != "":
		return tuple("0.0.0.0/0"), "", nil
	case ipv6:
		return tuple("0.0.0.0/0"), tuple("::/0"), nil
	}
	return tuple("0.0.0.0/0"), "", nil
}

// args are the rule as ufw's command line takes it, its addresses as ufw
// writes them.
func (r UFWRule) args() []string {
	from, _, _ := ufwAddr(r.From)
	to, _, _ := ufwAddr(r.To)
	a := []string{r.Action, cmp.Or(r.Direction, "in")}
	if r.Interface != "" {
		a = append(a, "on", r.Interface)
	}
	if r.Proto != "" && r.Proto != "any" {
		a = append(a, "proto", r.Proto)
	}
	a = append(a, "from", cmp.Or(from, "any"), "to", cmp.Or(to, "any"))
	if r.Port != "" {
		a = append(a, "port", r.Port)
	}
	if r.Comment != "" {
		a = append(a, "comment", r.Comment)
	}
	return a
}

// readKV reads a shell-style KEY=value file (quotes dropped).
func readKV(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return kv, sc.Err()
}

// readTuples is the "### tuple ###" lines of a ufw rules file, the prefix
// cut; a missing file has none.
func readTuples(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(l), "### tuple ###"); ok {
			out = append(out, strings.Join(strings.Fields(t), " "))
		}
	}
	return out, nil
}

func ufwCmd(args ...string) Cmd {
	return Cmd{Name: "ufw", Args: args, Timeout: time.Minute}
}

// ufwTodo is what UFW.Check found to do: ufw's commands, and what each
// does.
type ufwTodo struct {
	cmds  []Cmd
	drift []string
}

func (u *UFW) todo() (*ufwTodo, error) {
	if err := u.Validate(); err != nil {
		return nil, err
	}
	defaults, err := readKV(u.path("/etc/default/ufw"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("there is no /etc/default/ufw: ufw is not installed")
	}
	if err != nil {
		return nil, err
	}
	t := &ufwTodo{}
	dirs := make([]string, 0, len(u.Defaults))
	for d := range u.Defaults {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	for _, d := range dirs {
		p := u.Defaults[d]
		if got := defaults[ufwDirection[d]]; got != ufwPolicy[p] {
			t.cmds = append(t.cmds, ufwCmd("default", p, d))
			t.drift = append(t.drift, fmt.Sprintf("default %s %s (has %s)", d, p, cmp.Or(got, "none")))
		}
	}
	ipv6 := defaults["IPV6"] == "yes"
	v4, err := readTuples(u.path("/etc/ufw/user.rules"))
	if err != nil {
		return nil, err
	}
	v6, err := readTuples(u.path("/etc/ufw/user6.rules"))
	if err != nil {
		return nil, err
	}
	for _, r := range u.Rules {
		t4, t6, _ := r.Tuples(ipv6)
		has := (t4 == "" || slices.Contains(v4, t4)) && (t6 == "" || slices.Contains(v6, t6))
		some := (t4 != "" && slices.Contains(v4, t4)) || (t6 != "" && slices.Contains(v6, t6))
		desc := strings.Join(r.args(), " ")
		switch {
		case r.Absent && some:
			t.cmds = append(t.cmds, ufwCmd(append([]string{"delete"}, r.args()...)...))
			t.drift = append(t.drift, "delete "+desc)
		case !r.Absent && !has:
			t.cmds = append(t.cmds, ufwCmd(r.args()...))
			t.drift = append(t.drift, desc)
		}
	}
	if u.Enabled {
		conf, err := readKV(u.path("/etc/ufw/ufw.conf"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if conf["ENABLED"] != "yes" {
			t.cmds = append(t.cmds, ufwCmd("--force", "enable"))
			t.drift = append(t.drift, "enable")
		}
	}
	return t, nil
}

// Check's Data is the ufw commands to run.
func (u *UFW) Check(context.Context) (State, error) {
	t, err := u.todo()
	switch {
	case err != nil:
		return State{}, err
	case len(t.cmds) == 0:
		return inPlace()
	}
	return driftData(t.cmds, "%s", strings.Join(t.drift, "; "))
}

func (u *UFW) Apply(ctx context.Context, st State) (Result, error) {
	for _, c := range st.Data.([]Cmd) {
		if _, err := run(ctx, u.Exec, c); err != nil {
			return Result{}, err
		}
	}
	t, err := u.todo()
	if err != nil {
		return Result{}, err
	}
	if len(t.drift) > 0 {
		return Result{}, fmt.Errorf("ufw ran, and its files still differ: %s", strings.Join(t.drift, "; "))
	}
	return changed("%s", st.Drift)
}

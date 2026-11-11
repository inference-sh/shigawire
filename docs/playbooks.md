# Playbooks

A playbook is a YAML file that describes the state of a host. `shiga plan`
reads it and reports what would change, `shiga apply` makes those changes,
and `shiga verify` reports drift.

```yaml
name: web
vars:
  SSH_PORT: 22
  ADMIN: deploy
steps:
  - name: ssh
    tags: [security]
    tasks:
      - name: sshd port
        file:
          path: /etc/ssh/sshd_config.d/10-port.conf
          content: "Port ${SSH_PORT}\n"
        notify: reload ssh
    handlers:
      - name: validate sshd
        command: {cmd: [sshd, -t]}
      - name: reload ssh
        needs: validate sshd
        command: {cmd: [systemctl, reload, ssh]}

  - name: docker
    when: os.id == ubuntu
    tasks:
      - name: is docker installed
        assert: {cmd: [docker, version], warn: true}
        register: docker
      - name: compose file
        file: {path: /srv/app/compose.yml, src: compose.yml}
        when: docker.rc == 0
```

The modules are those of the registry the playbook is loaded with. The
`shiga` binary loads with the standard ones, listed in
[modules.md](modules.md); `shiga modules` lists them with their
arguments. A program that embeds shigawire may load with other modules
beside them ([modules.md](modules.md#registries)).

## Structure

### Playbook

| key     | value |
|---------|-------|
| `name`  | The playbook's name. Optional. |
| `vars`  | Default values of variables: a mapping of names to single values. |
| `steps` | The steps, in order. Required. |

Any other key is an error.

### Step

| key        | value |
|------------|-------|
| `name`     | Required, unique in the playbook. `--only` selects steps by name. |
| `tags`     | A tag or a list of tags. `--only` selects steps by tag too. |
| `manual`   | `true`: the step runs only when `--only` names it or one of its tags; a run without `--only` leaves it out. |
| `when`     | A condition, or a list of conditions, that applies to every task of the step. |
| `tasks`    | The tasks, in order. |
| `handlers` | The handlers the step defines. |

The handlers notified during a step run when the step ends, before the
next step starts, each once however many tasks notified it. They run in
the order the playbook defines them; a handler's `needs` that were also
notified run before it.

### Task

A task is a mapping with exactly one module key, whose value is the
module's arguments (a mapping, or nothing), and these keys:

| key             | value |
|-----------------|-------|
| `name`          | The task's line in the report. Default: the resource's ID (`file /etc/motd`). |
| `notify`        | A handler name or a list of them, run when the task changed something. Handlers are named across the whole playbook. |
| `when`          | A condition, or a list of conditions that must all hold. |
| `register`      | A name under which later conditions read what the task did. |
| `needs`         | A registered name or a list of them: earlier tasks this one depends on. When one of them failed in this run, this one is not checked: `plan` and `verify` report it `unknown` (`not checked: it needs swarm, which failed`), not a drift read from a host that task left as it was, and `apply` fails it. |
| `ignore_errors` | `true`: a failure is reported `ignored` and the play goes on, in `plan` and `verify` too. |

A key that is none of these is taken as the module; a second one is an
error. A module argument the module does not take is an error that names
it (`unknown key "contnet"`).

### Handler

A handler is a mapping with one module key and:

| key     | value |
|---------|-------|
| `name`  | Required, unique in the playbook. Tasks notify it by name. |
| `needs` | A handler name or a list of them. When one of them failed in this run, this one does not run and is reported failed. |
| `when`  | Conditions on variables and facts. A handler cannot read registered results. |

A step's `when` does not apply to its handlers: a handler only runs when a
task that ran notified it.

## Variables

`${NAME}` is replaced by the variable's value when the playbook loads. A
name is letters, digits and `_`, and does not start with a digit. `$${`
is a literal `${`. A variable with no value is an error that names the
file, line and column:

```
site.yml:14:20: undefined variable ${DOMAIN}
```

There are no templates, filters or loops.

### Sources

From lowest to highest precedence (a later source's value wins):

1. the playbook's `vars`;
2. the environment: `SHIGA_VAR_NAME=value` sets `NAME`. Only variables
   with that prefix are read, so `USER`, `HOME` or `PATH` never set a
   playbook variable;
3. each `--vars FILE`, in the order given. A vars file is a YAML mapping
   of names to single values, like `vars`. A file whose name ends in
   `.env` is read as `NAME=VALUE` lines instead: blank lines and `#`
   comments skipped, an `export ` prefix dropped, one pair of quotes
   around a value removed, nothing expanded;
4. each `--var NAME=VALUE`.

Values in `vars` and vars files are taken as written: a `${...}` in them
is not expanded.

### Where variables are expanded

- every value in a module's arguments (not the keys);
- a task's `name`;
- `when` conditions.

Step names, tags, handler names, `notify`, `needs` and `register` are
names and are not expanded. The tasks of a step that `--only` leaves out
are not loaded, so their variables need no value.

### Types after expansion

An unquoted value is typed after expansion, as YAML would type the text
it now holds. With `PORT=22`, `port: ${PORT}` is the number 22 and
`enabled: ${ON}` with `ON=true` is a boolean. A quoted value stays a
string: `content: "${PORT}"` is the string `"22"`.

In YAML's flow style (`{...}`, `[...]`), `{` and `}` end a value, so a
value that holds `${...}` must be quoted there:

```yaml
- file: {path: /etc/motd, content: "${GREETING}"}   # quoted: works
- file:
    path: /etc/motd
    content: ${GREETING}                             # block style: works
```

## Conditions

`when` takes one condition, or a list of conditions that must all hold.
Conditions on facts and registered results are evaluated when the play
reaches the task. A task whose condition is false is reported `skipped`
and nothing is done.

```
condition := ["not"] subject [op value]
           | ["not"] "exists" PATH
op        := "==" | "!=" | "contains"
```

With no `op`, the subject is tested for truth: it is false when it is
empty, `0`, `false`, `no` or `off` (in any case), and true otherwise.

`value` is the rest of the line, with variables expanded. One pair of
matching quotes around it is removed: `== ""` is the empty string.

A subject is one of:

| subject | value |
|---------|-------|
| `${NAME}` | The variable's value. |
| `mode` | The run's mode: `plan`, `apply` or `verify`. A step of health checks with `when: mode == verify` runs only in `verify`. |
| `os.id` | `ID` from `/etc/os-release` (`ubuntu`, `debian`). |
| `os.version_id` | `VERSION_ID` from `/etc/os-release` (`24.04`). |
| `os.arch` | `uname -m`, with Go's names: `amd64`, `arm64`, `arm`, `386`; other machines keep uname's name. |
| `NAME.rc` | The exit code of the last command the registered task ran. |
| `NAME.stdout` | The output (stdout and stderr together) of that command. `==` and `!=` compare it with surrounding whitespace removed; `contains` searches all of it. |
| `NAME.changed` | `true` when the task changed something (in plan and verify: when it would). |
| `NAME.failed` | `true` when its check or apply failed. |
| `NAME.skipped` | `true` when its own `when` was false. |
| `NAME.ok` | `true` when it neither failed nor was skipped. |

`exists PATH` is true when the path exists on the host (a link is not
followed). `PATH` must be absolute.

Examples:

```yaml
when: ${INSTALL_DOCKER}
when: not ${MINIMAL}
when: [os.id == ubuntu, os.version_id == "24.04"]
when: os.arch != arm64
when: not exists /etc/letsencrypt/live/${DOMAIN}
when: ufw.stdout contains Status: active
when: docker.rc == 0
when: mode == verify
```

A task or handler with a condition that reads only variables and is
false is skipped without its module being made: its arguments are not
checked, so a task for an optional file can name a variable that is
empty when the task does not apply (`src: ${CERT_FILE}` with
`when: ${CERT_FILE}`).

A condition that cannot be evaluated fails its task: `/etc/os-release`
missing, a registered task that has not run (its step was left out by
`--only`), or a registered task that ran no command, for `rc` and
`stdout`. In `plan` and `verify` that task is reported `unknown`.

### Registered results

`register: NAME` keeps what the task did in this run. Only tasks after it
can read it; a name is registered once in a playbook.

`rc` and `stdout` are those of the last command the task's resource ran
through the module's command runner, whether in its check or its apply.
A command's `unless` guard and the lookup of an `owner` or `group` do not
count. A task that ran no command (a guard said it was in place already)
has none.

`plan` and `verify` run no apply, so a task that only does its work in
apply (a command with no guard) has run no command there, and a later
`NAME.rc` is unknown. A module whose check runs the command (a read-only
query such as `assert`) gives `rc` and `stdout` in every mode.

## Files

A directory named `files` next to the playbook is the playbook's files.
A module argument that names one with a relative path
(`src: compose.yml`) reads it from there, when the playbook loads.

## Errors

Every load error names the file and line, and the column where YAML gives
one:

```
site.yml:12:9: unknown module "fiel" (shiga modules lists them)
site.yml:12:9: file: unknown key "contnet"
site.yml:20:15: when: "docker.rc": no task before this one registers "docker"
site.yml:7:5: step "ssh": a step of that name is at line 3
```

## Running

```
shiga plan PLAYBOOK [--var NAME=VALUE]... [--vars FILE]... [--only STEP,...] [--quiet] [--detailed-exitcode]
shiga apply PLAYBOOK ...
shiga verify PLAYBOOK ...
shiga check PLAYBOOK [--var NAME=VALUE]... [--vars FILE]... [--allow-undefined]
shiga test PLAYBOOK [--var NAME=VALUE]... [--vars FILE]... [--only STEP,...]
shiga modules
shiga version
```

- `plan` checks every task and changes nothing. It reports what `apply`
  would change, and which handlers would run.
- `apply` checks each task and changes those not in place. It stops at
  the first failure; the handlers notified before it still run.
- `verify` checks every task, runs no handler and no action (a command
  with no guard), and reports drift. A check that ran and found the host
  wrong (an `assert`, `probe` with no `fix` or `fail` task that does not
  pass, a locked swarm) is `failed`, and verify goes on to the next task.
- `check` validates the playbook and touches no host ([Checking a
  playbook](#checking-a-playbook)).
- `test` runs the playbook on a simulated host ([Testing a
  playbook](#testing-a-playbook)).

Flags can come before or after the playbook. `--only` takes step names
or tags, comma-separated or repeated; a name that matches no step is an
error.

One line is printed per task and handler, then a summary:

```
ok       ssh: sshd port
changed  ssh: authorized keys: create
skipped  docker: compose file: when docker.rc == 0
changed  handler reload ssh
apply: 1 ok, 2 changed, 1 skipped
```

| status | meaning |
|--------|---------|
| `ok` | In place; nothing done. |
| `changed` | Changed (apply), would change (plan), or drifted (verify). |
| `failed` | The check or apply failed. In `verify`, a check that ran and found the host wrong (an `assert` that does not hold). |
| `ignored` | Failed, with `ignore_errors: true`; the play went on. In plan and verify, a check that failed in such a task: it neither blocks a plan nor fails verify. |
| `warn` | The module reported a warning; the play went on. |
| `unknown` | In plan or verify, the check failed, so whether it is in place is not known: it could not read what it reads (a file's owner with no such user yet, ufw not installed). In plan, also an `assert` that does not hold, which a change planned before it may make pass. |
| `skipped` | Its `when` was false. |

`--quiet` leaves out the `ok` and `skipped` lines.

Exit codes:

| code | meaning |
|------|---------|
| 0 | Success. `verify`: no drift. |
| 1 | A failure: a load error, a failed task or handler, in `verify` a `failed` or `unknown`, or in `plan` an `unknown` no earlier change can explain (below). |
| 2 | `verify`: drift. `plan` and `apply` with `--detailed-exitcode`: something changed or would change. |

An `unknown` in a plan is a check that failed. `plan` exits 0 when it
could plan every task, or when each `unknown` comes after a change planned
before it, which it may depend on (an owner a `user` task would create):
`apply` makes that change first, and the plan cannot tell whether the
check would pass then. The summary counts them:

```
plan: 8 changed, 2 unknown
2 unknown: checks that may depend on changes planned before them, which apply makes first
```

`plan` exits 1 for an `unknown` that `apply` fails at whatever runs before
it: a refusal (a `fail` task whose `when` holds, a `docker_swarm` that is
locked, pending or in error), or a check that failed with no change planned
before it, so `apply` reads the same host there.

## Checking a playbook

`shiga check` loads the playbook as `apply` would and reports every
problem it finds, one line each, where loading stops at the first. It
runs no command, reads no file of the host and checks no task against
the host. Each task's module builds its resource with a command runner
that runs nothing and a root that is a new empty directory.

It reports:

- YAML errors, and errors in the playbook's structure (an unknown key in
  a step, a step without a name). These stop the check: what follows
  cannot be read.
- unknown modules, also in a task whose `when` is false when the
  playbook loads;
- module argument errors: an unknown key, a wrong type, a relative path,
  a missing required argument, a bad regexp;
- undefined variables, in arguments, names and `when`;
- `when` conditions that do not parse, or read a name no earlier task
  registers;
- `notify` or a handler's `needs` naming no handler, a task's `needs`
  naming no earlier `register`;
- a relative `src`, or an `apt_repo` `key_src`, missing from `files/`.

Every step is checked, the manual ones too; `--only` is not taken.

```
$ shiga check site.yml
site.yml:12:9: unknown module "fiel" (shiga modules lists them)
site.yml:16:35: undefined variable ${DOMAIN}
site.yml:24:17: notify "reload sshd": no handler of that name
check: 3 problems
$ shiga check site.yml --var DOMAIN=example.com --var ...
ok: 4 steps, 13 tasks
```

Exit code 1 when there is a problem, 0 otherwise.

Undefined variables are problems by default, as they are for `apply`.
Give the values with `--var` and `--vars`, or with `SHIGA_VAR_` in the
environment. `--allow-undefined` is for a playbook whose variables come
only at apply time: an undefined variable is then not reported, and the
arguments, `when` or name that use one are left unchecked.

What a module reads from the host when the task runs is not checked:
that an `owner` or `group` exists (`file`, `copy`, `dir`, `symlink`), an
absolute `src`, that an `apt` package or repository exists, that a
command works, a URL answers or an `exists` path is there. A task whose
`when` is false when the playbook loads (it reads only variables) has its
module looked up but its arguments left unchecked, as `apply` leaves
them.

## Testing a playbook

`shiga test` runs the playbook on a simulated Debian host: it applies it,
applies it again and wants nothing changed but actions (a command with
no guard) and no handler run, then runs `verify` and wants no drift. On
failure it prints the failed tasks, what changed on the second apply, or
the drift, and exits 1.

```
$ shiga test site.yml --var DOMAIN=example.com
simulated host: packages, units, accounts, sysctl, ufw and docker swarm are modelled; any other command succeeds with no output
apply: 1 ok, 12 changed
apply again: 13 ok
verify: 13 ok
ok: the playbook converges on the simulated host
```

It is a simulation, not a host. Packages, systemd units, accounts,
sysctl values, ufw's files and the docker swarm state are modelled (any
package name installs); every other command succeeds with no output. A
task that reads a command's output (an `assert`, a `probe`, a `query`)
sees an empty one, so a health check that wants `200` from `curl` is
`failed` in `verify`. The host's files are a temporary directory,
removed when the test ends. `shiga test` uses package `simrun`, as
`shigatest` does.

Package `shigatest` runs a playbook on a simulated Debian host (package
`host/hosttest/sim`): a temporary directory as its root, and a command
runner that keeps packages, systemd units, accounts, sysctl values, ufw's
files and the docker swarm state, so what one run does the next one
sees. Any other command succeeds with no output unless the test gives it
an answer (`h.Exec.On`). `examples/examples_test.go` tests
`examples/web/web.yml` this way.

```go
import (
	"github.com/inference-sh/shigawire/shigatest"
)

func TestSite(t *testing.T) {
	h := shigatest.Run(t, "site.yml", map[string]string{"DOMAIN": "example.com"})
	if !strings.Contains(h.Read("/etc/nginx/sites-enabled/site"), "example.com") {
		t.Fatal("no server name")
	}
}
```

`Run` applies the playbook, applies it again and fails if the second run
changed anything but an action or ran a handler, then runs `verify` and
fails on drift. `RunOn` takes a `hosttest.Host` the test set up first
(files under its root, answers for commands with `h.Exec.On`). Modules
The playbook may name the standard modules. A playbook that names other
modules is tested with `RunOptions` and a registry that has them:

```go
reg := builtin.Registry()
internal.Register(reg)
shigatest.RunOptions(t, sim.New(t).Host, "site.yml", playbook.Options{Registry: reg})
```

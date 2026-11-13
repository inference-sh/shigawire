# shigawire

> *Shigawire: the metallic filament of Dune, extruded from a desert vine, thin as thread and strong enough to bind anything.*
>
> *"A process cannot be understood by stopping it. Understanding must move with the flow of the process, must join it and flow with it."*
> — The First Law of Mentat

**shigawire puts Linux hosts in a described state.** You write a YAML playbook: these packages, these files, this firewall, these services. The `shiga` command makes the host match it, and only changes what is different.

```
$ shiga plan examples/web/web.yml --var DOMAIN=example.com
changed  user: deploy user: create
unknown  user: ~/.ssh: there is no user deploy
skipped  user: authorized key: when ${DEPLOY_KEY}
changed  firewall: ufw installed: 2 of 2 to put in place
unknown  firewall: ssh and web only: there is no /etc/default/ufw: ufw is not installed
changed  nginx: nginx installed: 2 of 2 to put in place
ok       nginx: no default site
unknown  nginx: site root: there is no user deploy
changed  nginx: security headers snippet: create
changed  nginx: site config: create
changed  nginx: site enabled: create
changed  nginx: nginx running: enable and start
changed  handler reload nginx: nginx -t -q
skipped  health: nginx answers on port 80: when mode == verify
plan: 1 ok, 8 changed, 3 unknown, 2 skipped
3 unknown: checks that may depend on changes planned before them, which apply makes first

$ sudo shiga apply examples/web/web.yml --var DOMAIN=example.com --quiet
...
apply: 1 ok, 11 changed, 2 skipped

$ sudo shiga apply examples/web/web.yml --var DOMAIN=example.com --quiet
apply: 11 ok, 2 skipped
```

`unknown` in a plan is a check that failed. Here each one depends on an earlier change: the `deploy` user does not exist until apply creates it. plan cannot tell such a check from one that will fail in apply too, so it exits 0 and counts them. It exits 1 for an unknown no earlier change can explain: a refusal (a `fail` task, a locked docker swarm), or a check that failed with no change planned before it. (This output is from shiga's simulated host, `shiga test`; a real host prints the same.)

## Why

- **One static binary.** No Python, no interpreter, no agent on the host. Copy it over and run it.
- **Plan before you apply.** `shiga plan` reads the host and lists every change `apply` would make, and changes nothing.
- **Runs converge.** Every resource reads the host first and acts only when it is not in place. A second `apply` changes nothing, and the test tooling proves it.
- **Drift detection built in.** `shiga verify` checks the host against the playbook, runs no actions, and exits non-zero on drift. Put it in cron.
- **Playbooks you can test without a server.** `shiga check` validates a playbook without reading the host; `shiga test` runs it twice on a simulated Debian host.
- **Safe config changes.** A handler can validate a config (`sshd -t`, `nginx -t`, `visudo -c`) before reloading the service, and put the old file back when validation fails.
- **YAML for what, Go for how.** Playbooks only declare state. The logic lives in typed, tested Go modules.

## Install

Download a release for your platform from [Releases](https://github.com/inference-sh/shigawire/releases) (linux and macOS, amd64 and arm64), or:

```
go install github.com/inference-sh/shigawire/cmd/shiga@latest
```

## A playbook

```yaml
name: web
vars:
  DOMAIN: example.com

steps:
  - name: firewall
    tasks:
      - apt: {name: ufw}
      - ufw:
          default: {incoming: deny, outgoing: allow}
          rules:
            - {rule: limit, port: 22, proto: tcp}
            - {rule: allow, port: "80,443", proto: tcp}

  - name: nginx
    tasks:
      - apt: {name: nginx}
      - file:
          path: /etc/nginx/sites-enabled/${DOMAIN}
          src: site.conf
        notify: reload nginx
      - systemd: {name: nginx, enabled: true, state: started}
    handlers:
      - name: reload nginx
        validate_reload:
          validate: [nginx, -t]
          service: nginx
          restore: ["/etc/nginx/sites-enabled/${DOMAIN}"]
```

A playbook is a list of **steps**. A step holds **tasks**, and each task names one **module** with its arguments. Tasks **notify** **handlers**, which run once at the end of the step if something changed. Files named by `src` (and `apt_repo`'s `key_src`) live in `files/` next to the playbook.

- **Variables:** `${NAME}`, from the playbook's `vars`, `--vars FILE` (YAML, or `NAME=VALUE` lines in a `.env` file), `SHIGA_VAR_NAME` in the environment, and `--var NAME=VALUE`, in that order. An undefined variable is an error with its file and line.
- **Conditions:** `when: os.id == ubuntu`, `when: ${ENABLE_TLS}`, `when: docker.rc != 0` on a result saved with `register: docker`.
- **Dependencies:** `needs: swarm` on a task that reads what the task registered as `swarm` puts in place. When that one failed, this one is not checked: plan and verify report it unknown with the reason, and apply fails it.
- **Selecting steps:** `--only firewall,nginx` by name or tag. A step marked `manual` runs only when selected.
- **Includes:** `include: [../common/base.yml]` puts another playbook's steps before this one's, its `vars` under this one's. Paths are relative to the including file; a file reached twice loads once.

There are no templates and no loops, on purpose. A full example is in [examples/web](examples/web). The format reference is [docs/playbooks.md](docs/playbooks.md).

## Commands

| command | |
|---|---|
| `shiga plan PLAYBOOK` | what `apply` would change; changes nothing |
| `shiga apply PLAYBOOK` | put the host in the playbook's state |
| `shiga verify PLAYBOOK` | report drift; runs no actions; exit 2 on drift, 1 on a failed check (an `assert` that does not hold) |
| `shiga check PLAYBOOK` | validate modules, arguments, variables, `when`, `notify` and files; reads nothing of the host |
| `shiga test PLAYBOOK` | apply, apply again, verify, on a simulated Debian host |
| `shiga modules` | every module and its arguments |

Exit codes: 0 ok, 1 failure (in `verify`: a failed or unknown check; in `plan`: an unknown no earlier change can explain), 2 drift (`verify`) or changes (`--detailed-exitcode`).

## Modules

| | |
|---|---|
| files | `file` / `copy`, `dir`, `absent`, `symlink`, `line` / `lineinfile`, `block` / `blockinfile` |
| packages | `apt`, `apt_repo`, `download` / `get_url` (sha256-checked) |
| services | `systemd` / `service`, handlers `restart`, `reload`, `daemon_reload`, `validate_reload` |
| system | `user`, `group`, `sysctl`, `ufw` |
| containers | `compose` / `docker_compose`, `docker_swarm` |
| commands and checks | `command` / `shell` (with `creates` / `unless`), `query`, `probe`, `assert`, `fail` |

The `ufw` module adds and updates the rules it lists and leaves other rules alone. ufw keeps one rule per port, protocol, addresses, interface and direction, whatever its action, so a listed `allow 22/tcp` where ufw has `limit 22/tcp` is drift reported as `limit -> allow`, and apply updates that rule in place.

Names follow Ansible's where they mean the same thing. Every module refuses unknown arguments by name. Reference: [docs/modules.md](docs/modules.md).

## Compared with Ansible

| | shigawire | Ansible |
|---|---|---|
| On the host | one static binary | Python |
| Runs from | the host itself | a control machine over SSH |
| Many hosts at once | not yet (planned: push over SSH) | yes, with inventories |
| Dry run | `plan`, from every resource's own check | `--check`, per module support |
| Drift report | `verify`, exit code 2 | `--check --diff`, by convention |
| Playbook tests | built in: `check` and `test` | Molecule, separately |
| Templates and loops | none: variables and conditions only | Jinja2 |
| Modules | about 25 for Debian and Ubuntu servers | thousands, many platforms |

Ansible is broader and much more mature. shigawire does less, and aims to do it with stronger guarantees: one binary, a plan that matches what apply does, runs that converge, and playbooks that are tested.

## Use it as a library

The `host` package is the resource library: `host.File`, `host.Line`, `host.Block`, `host.Apt`, `host.Service`, `host.User`, `host.UFW` and more. Each has a `Check` that reads the host and an `Apply` that changes only what `Check` found. A `host.Runner` runs them in apply, plan or verify mode with handlers.

A program can embed shigawire and add its own modules:

```go
reg := builtin.Registry()        // the standard modules
mymodules.Register(reg)          // yours: module.Registry.Register(name, ctor)
pb, err := playbook.Load("setup.yml", playbook.Options{Registry: reg})
```

Registries are values, not globals, so one program can run its own playbooks with private modules and untrusted playbooks with the built-ins only.

In Go tests, `shigatest.Run(t, "site.yml", vars)` applies a playbook twice on the simulated host, verifies it, and returns the host to inspect.

## Layout

| | |
|---|---|
| `cmd/shiga` | the command |
| `host` | resources, the Runner, plan and verify |
| `host/hosttest`, `host/hosttest/sim` | a fake command runner, and a simulated Debian host |
| `host/conformance` | resource tests in real Ubuntu and Debian containers |
| `module`, `module/builtin` | the module registry, and the standard modules |
| `playbook` | the YAML loader, variables and conditions |
| `shigatest` | testing playbooks from Go |

## Status

Early. The resource library has set up production hosts for a while; the playbook layer and the CLI are new. Hosts: Debian 12/13 and Ubuntu 22.04/24.04. Run `shiga plan` before the first `apply` on any host you care about.

Planned: push to many hosts over SSH with an inventory, running hosts in parallel; more modules.

## Building

```
make test     # vet and tests
make dist     # static binaries for linux/amd64 and linux/arm64
```

Releases: run the Release workflow on `dev` with a bump (patch, minor or major), from the Actions tab or `gh workflow run release.yml -f bump=minor`. It tests, checks the API, tags the next version, builds the binaries and publishes them with checksums. Pushing a `v*` tag by hand releases that tag the same way.

API compatibility: the module's public API (every package but `internal` ones) is compared with the latest `v*` release by [apidiff](https://pkg.go.dev/golang.org/x/exp/cmd/apidiff), pinned in `.github/scripts/apicompat.sh`; `make apicompat` runs it locally. Removing or changing an exported name, field or signature is an incompatible change, and it needs a major release, or a minor one while the version is v0 (v0.1.x to v0.2.0). Patch releases and, from v1, minor releases add API only.

- CI fails on an incompatible change unless `.github/api-next` names the next release and that release is big enough for the change. A version already released in that file is stale and ignored.
- The Release workflow runs the same check with the version it is about to tag, and stops before tagging when the bump is too small.

Callers outside this repo (the engine) import the packages: a name with no caller here may still have one.

## License

Apache-2.0. See [LICENSE](LICENSE).

# Modules

A task names a module and gives it arguments. Each module turns its
arguments into one resource from package `host`: it reads the host
first, and changes it only when it is not in place, so a second run
changes nothing. These are the standard modules, in package `modules`
(`builtin.Registry()` is a registry of them).

## Registries

A playbook can name only the modules in the registry it is loaded with
(`playbook.Options.Registry`, a `*module.Registry`). There is no global
registry. The `shiga` binary and `shigatest` use `builtin.Registry()`.

A program that embeds shigawire builds its own. An engine with internal
modules loads its own playbooks with the built-ins plus its private
modules, and untrusted playbooks with the built-ins only, so those cannot
name the private ones:

```go
trusted := builtin.Registry()
internal.Register(trusted) // the engine's own modules

own, err := playbook.Load("engine.yml", playbook.Options{Registry: trusted, Env: env})
theirs, err := playbook.Load(userPath, playbook.Options{Registry: builtin.Registry(), Env: env})
```

A package of modules exposes `func Register(r *module.Registry)`, which
calls `r.Register(name, ctor)` for each module (`module.Typed` makes a
ctor from a function of an argument struct). `Register` panics when a
name is registered twice.

Rules for every module:

- Arguments are strict. A key the module does not take is an error
  (`unknown key "contnet"`), and so is a value of the wrong type.
- Paths are the host's and must be absolute.
- A mode is octal: `"0644"`, `0644` or `0o644`. A mode written as a
  decimal number (`644`, or a variable holding `640` in an unquoted
  `mode: ${MODE}`) is an error, since it would not be the mode meant.
- A duration is a string: `"30s"`, `"1h"`.
- A command (`cmd`, `unless`, `validate`, `then`) is a string, run by
  `/bin/sh -c`, or a list, run as it is with the first item the program.
- `owner` and `group` take a name or a number, read from the host's user
  database (`getent`) when the task runs. An owner with no group gives
  the owner's primary group. A group with no owner is refused.
- A list argument (`name`, `groups`, `uris`, ...) also takes a single
  string.
- `state` defaults to `present` where a module has one.

Several modules are actions, with no state to read: they run every time
they are applied, and Verify does not run them. These are a `command`
with no guard, `apt` with `update_cache` and no `name` (unless the lists
are newer than `cache_valid_time`), and the handler modules `restart`,
`reload`, `daemon_reload` and `validate_reload`.

## file, copy

A regular file with its content, mode and owner. A file that differs is
replaced atomically: written beside it and renamed over it. A file that
differs only in mode or owner is written again too.

`copy` takes the same arguments with `dest` in place of `path`.

| key | type | |
|---|---|---|
| `path` (`dest` for copy) | path | required |
| `content` | string | the file's content; `""` is an empty file |
| `src` | path | a file to copy instead of `content`: a relative path is under the playbook's `files/` directory and read when the playbook loads; an absolute path is a file on the host, read when the task is checked |
| `mode` | mode | unset: an existing file's, or 0644 for a new one |
| `owner`, `group` | name or id | unset: an existing file's, or the writer's |
| `parents` | bool | make missing parent directories (0755) |
| `seed` | bool | keep an existing file's content; set only its mode and owner |

One of `content` and `src` is required, unless `seed` is set: then a
missing file is made empty, and an existing one keeps its content.

```yaml
- file:
    path: /etc/motd
    content: "Managed by shigawire.\n"
    mode: "0644"
- copy:
    src: agent
    dest: /usr/local/bin/agent
    mode: "0755"
```

## dir

A directory with its mode and owner. A missing one is made with its
missing parents.

| key | type | |
|---|---|---|
| `path` | path | required |
| `mode` | mode | unset: an existing directory's, or 0755 for a new one |
| `owner`, `group` | name or id | |

```yaml
- dir: { path: /srv/app/data, mode: "0750", owner: app }
```

## absent

A path that does not exist. A file, a link or an empty directory is
removed. A directory with something in it is an error unless
`recursive`. `/` is refused.

| key | type | |
|---|---|---|
| `path` | path | required |
| `recursive` | bool | remove a directory with everything in it |

```yaml
- absent: { path: /etc/nginx/sites-enabled/default }
```

## symlink

A link at `path` to `target`. A link pointing elsewhere is replaced
atomically. A real file or directory at `path` is an error unless `force`
(a regular file is replaced) or `aside` (it is moved there first).

| key | type | |
|---|---|---|
| `path` | path | required |
| `target` | string | required; written into the link as given |
| `force` | bool | replace a regular file at `path` |
| `aside` | path | move what is at `path` here first |
| `owner`, `group` | name or id | the link's own owner |

```yaml
- symlink: { path: /etc/resolv.conf, target: /run/systemd/resolve/resolv.conf, force: true }
```

## line, lineinfile

A line in a text file. The first line equal to `line` or matching
`regexp` becomes `line`; the other such lines are removed; a file with
none gets the line appended. A missing file is created (0644). With
`state: absent`, every line equal to `line` or matching `regexp` is
removed, and a missing file is in place. A link is followed.

| key | type | |
|---|---|---|
| `path` | path | required |
| `line` | string | required for present |
| `regexp` | regexp (RE2) | the lines `line` replaces, or removes |
| `state` | `present`, `absent` | |

With `state: absent`, `line` or `regexp` is required. `line` is one
line: one trailing newline (from `line: |`) is dropped, and a newline
inside it is an error; `block` is for several lines.

```yaml
- lineinfile:
    path: /etc/ssh/sshd_config
    regexp: '^#?PasswordAuthentication'
    line: PasswordAuthentication no
```

## block, blockinfile

Lines between two marker lines. What is between the begin marker and the
end marker after it becomes `block`; a file without the markers gets
them, with the block, appended. `state: absent` removes the markers and
what is between them. A begin marker with no end marker after it is an
error.

| key | type | |
|---|---|---|
| `path` | path | required |
| `block` | string | the lines; a trailing newline is dropped |
| `marker` | string | must hold `{mark}` (BEGIN, END); default `# {mark} MANAGED BLOCK` |
| `state` | `present`, `absent` | |

Two blocks in one file need two markers.

```yaml
- blockinfile:
    path: /etc/hosts
    marker: "# {mark} cluster"
    block: |
      10.0.0.2 db
      10.0.0.3 cache
```

## apt

Debian packages installed or removed, through `apt-get`. Every apt task
in a run shares one read of what dpkg has installed.

| key | type | |
|---|---|---|
| `name` | list | packages: `name`, `name=version`, `name:arch`; `absent` and `purged` take a bare `name` |
| `state` | `present`, `absent`, `purged` | `purged` removes config files too |
| `update_cache` | bool | update the package lists |
| `cache_valid_time` | duration | with `update_cache`: lists newer than this are not updated |

With `name` and `update_cache`, the lists are updated only when a
package is missing, right before the install. With `update_cache` alone,
the update is an action, run every time unless `cache_valid_time` says
the lists are new enough; a source added or changed in the run makes the
next update due whatever their age. `update_cache` with `absent` or
`purged` is refused. A virtual package name never matches what dpkg has
installed: name the package that provides it.

```yaml
- apt: { name: [curl, jq, ufw], update_cache: true }
- apt: { update_cache: true, cache_valid_time: 1h }
- apt: { name: snapd, state: purged }
```

## apt_repo

An apt source in deb822 form and its signing key. The key goes in
`keyring_dir/<name>.asc` (`.gpg` for a binary key), the source in
`/etc/apt/sources.list.d/<name>.sources` with `Signed-By` naming the key.
A key from `key_url` is fetched every run, so a rotated key is picked
up. A changed key or source makes the next apt update due.

| key | type | |
|---|---|---|
| `name` | string | required; letters, digits, `_ . -` |
| `uris` | list | required |
| `suites` | list | required |
| `components` | list | |
| `key_url` | URL | where the key is fetched from |
| `key` | string | the key itself, armored |
| `keyring_dir` | path | default `/etc/apt/keyrings` |

One of `key_url` and `key` is required.

```yaml
- apt_repo:
    name: docker
    uris: https://download.docker.com/linux/ubuntu
    suites: noble
    components: stable
    key_url: https://download.docker.com/linux/ubuntu/gpg
- apt: { name: docker-ce, update_cache: true }
```

## systemd, service

A systemd unit: enabled at boot or not, running or not, and its unit
file. What is left unset is left as it is.

| key | type | |
|---|---|---|
| `name` | string | required: the unit (`nginx`, `app.service`, `backup.timer`) |
| `enabled` | bool | true enables, false disables |
| `state` | `started`, `stopped` | |
| `unit` | string | the unit file's content |
| `unit_path` | path | default `/etc/systemd/system/<name>` (`.service` added to a name with no suffix) |

One of `enabled`, `state` and `unit` is required. When the unit file
changed, it is written and `systemctl daemon-reload` runs before the unit
is enabled or started. A running unit is not restarted when its file
changed: notify a `restart` handler. `restarted` and `reloaded` are not
states; they are the `restart` and `reload` modules.

```yaml
- systemd:
    name: agent
    enabled: true
    state: started
    unit: |
      [Unit]
      After=network-online.target
      [Service]
      ExecStart=/usr/local/bin/agent
      Restart=always
      [Install]
      WantedBy=multi-user.target
  notify: restart agent
```

## user

A user in the system's user database, made with `useradd` and put right
with `usermod`, or removed with `userdel` (its home is left). A field left
unset is left to `useradd` for a new user and as it is for an existing
one.

| key | type | |
|---|---|---|
| `name` | string | required |
| `uid` | int | |
| `group` | string | the primary group, by name or id |
| `groups` | list | supplementary groups the user is added to; it is never removed from others |
| `home` | path | an existing user's home is changed without moving its files |
| `shell` | string | |
| `comment` | string | |
| `system` | bool | a new user gets a system uid |
| `create_home` | bool | default true: a new user's home is made |
| `no_password` | bool | no password and not locked: the shadow password is `*`, as `adduser --disabled-password` leaves it |
| `state` | `present`, `absent` | |

`useradd` leaves a new user's shadow password `!`, a locked account. sshd
refuses a locked account even a key login when `UsePAM` is `no`. With
`no_password`, a new user is made with `*`, and an existing one whose
password is `!` (no hash) gets `*`; one locked with a hash in it is an
error.

```yaml
- user: { name: deploy, shell: /bin/bash, groups: [docker, adm] }
```

## group

A group, made with `groupadd`, its gid set with `groupmod`, or removed
with `groupdel`.

| key | type | |
|---|---|---|
| `name` | string | required |
| `gid` | int | unset: groupadd picks one, and an existing group's is kept |
| `system` | bool | a new group gets a system gid |
| `state` | `present`, `absent` | |

```yaml
- group: { name: app, gid: 2000, system: true }
```

## sysctl

A kernel parameter, set in a sysctl.d drop-in and in the running
kernel. The drop-in gets `key = value` in place of any line that sets
the key. When the line changed, or the kernel has another value,
`sysctl -p <file>` loads the drop-in (every key in it), and the kernel's
value is read again: one that still differs is an error. Values compare
with their whitespace collapsed (the kernel prints some tab-separated).

| key | type | |
|---|---|---|
| `name` | string | required: the key (`net.ipv4.ip_forward`) |
| `value` | string | required; quote numbers |
| `file` | path | default `/etc/sysctl.d/99-shigawire.conf` |

```yaml
- sysctl: { name: net.ipv4.ip_forward, value: "1" }
- sysctl: { name: vm.swappiness, value: "10", file: /etc/sysctl.d/90-memory.conf }
```

## download, get_url

A file fetched over HTTP(S). With `sha256`, a file that has it is in
place and the download must match it; without, a file that exists is in
place unless `force`. The file is written beside `dest` and renamed into
place once complete and checked, so a failed or mismatched download
leaves the old file.

| key | type | |
|---|---|---|
| `url` | URL | required; http or https |
| `dest` | path | required |
| `sha256` | string | 64 hex digits |
| `mode` | mode | unset: an existing file's, or 0644 |
| `force` | bool | without sha256: fetch again each run, an action (verify reports it in place); changed only when the content is new, which alone replaces the file |

```yaml
- get_url:
    url: https://github.com/example/tool/releases/download/v1.2.0/tool-linux-amd64
    dest: /usr/local/bin/tool
    sha256: 9f2c...e1
    mode: "0755"
```

## command, shell

A command for what no other module models (an installer, a one-time
init). Without a guard it is an action: it runs every time and reports
changed. A guard says when it has run already. `shell` is another name
for `command`; both run a string through `/bin/sh -c`.

| key | type | |
|---|---|---|
| `cmd` | command | required |
| `creates` | path | in place when this exists |
| `removes` | path | in place when this does not exist |
| `unless` | command | in place when this exits 0; runs with the same `chdir` and `env` |
| `chdir` | path | the directory it runs in; default `/` |
| `env` | map | variables added to the environment |
| `timeout` | duration | kill it after this long |
| `watch` | list of paths | changed only when one of these files' content changed |
| `changes_packages` | bool | it installs or removes packages: later apt tasks read dpkg again |

```yaml
- command:
    cmd: curl -fsSL https://get.docker.com | sh
    creates: /usr/bin/docker
    changes_packages: true
    timeout: 30m
- command:
    cmd: [mdadm, --create, /dev/md0, --level=1, --raid-devices=2, /dev/sdb, /dev/sdc]
    creates: /dev/md0
```

## compose, docker_compose

A Docker Compose project up or down. Up is in place when every service
in `docker compose config --hash '*'` has a container whose
`com.docker.compose.config-hash` label matches, running or exited 0;
otherwise `docker compose up --detach --remove-orphans` runs. Down is in
place when the project has no containers; otherwise
`docker compose down --remove-orphans` runs. The commands run in
`project_src`.

| key | type | |
|---|---|---|
| `project_src` | path | required: the project directory |
| `files` | list | compose files (`-f`), relative to `project_src` |
| `project_name` | string | `-p` |
| `state` | `present`, `absent` | |

A changed image under the same tag is not a changed config: pull it
with a `command` first.

```yaml
- compose: { project_src: /srv/app, files: compose.yaml, project_name: app }
```

## docker_swarm

The Docker node in a swarm. It reads `docker info`'s
`Swarm.LocalNodeState`: `active` is in place, whichever swarm the node is
in; `inactive` runs `docker swarm init` or `docker swarm join`; `pending`,
`error` and `locked` are errors left to the operator. After init or join
the state is read again and must be `active`. The join token is not in
messages.

| key | type | |
|---|---|---|
| `state` | `init`, `join` | default `init` |
| `advertise_addr` | string | `--advertise-addr` |
| `listen_addr` | string | `--listen-addr` |
| `remote_addrs` | list | join: managers' `host:port` |
| `join_token` | string | join: the worker or manager token |

```yaml
- docker_swarm: { advertise_addr: 100.64.0.10 }
- docker_swarm:
    state: join
    remote_addrs: 100.64.0.10:2377
    join_token: SWMTKN-1-...
```

## ufw

The ufw firewall's default policies, its rules, and whether it is
enabled. It reads what ufw has from ufw's own files: the policies and
`IPV6` from `/etc/default/ufw`, `ENABLED` from `/etc/ufw/ufw.conf`, the
rules from the `### tuple ###` lines of `/etc/ufw/user.rules` and
`/etc/ufw/user6.rules`. It runs `ufw` for what differs: the policies,
then each rule in order, then `ufw --force enable`, so the rules that let
the operator in are there before the firewall is on. Then it reads the
files again, and a rule ufw stored in another form than expected is an
error, not a change on every run. Missing rules are appended in the order
given; rules ufw has that the task does not list are left.

| key | type | |
|---|---|---|
| `default` | map | `incoming`, `outgoing`, `routed` to `allow`, `deny`, `reject` |
| `enabled` | bool | enable now and at boot; false leaves it |
| `rules` | list | rules, below |

A rule:

| key | type | |
|---|---|---|
| `rule` | `allow`, `deny`, `reject`, `limit` | required |
| `direction` | `in`, `out` | default `in` |
| `interface` | string | the interface the rule is on |
| `proto` | `tcp`, `udp`, `any` | default `any` |
| `from`, `to` | address or network | default any |
| `port` | string | destination port: `"22"`, `"6000:6007"`, `"80,443"`; a range or list needs `proto` |
| `comment` | string | no quote or newline |
| `state` | `present`, `absent` | absent deletes the rule when ufw has it |

Ports are numbers: service and app names (`ssh`, `OpenSSH`) are refused,
because ufw stores them in another form. A rule with an address is for
that address's IP version; one without is for IPv4, and IPv6 too when
`IPV6=yes`. A network is masked (`172.16.5.0/12` is `172.16.0.0/12`) and
a single address loses its prefix length, as ufw writes them. ufw must be
installed; when it is not, Check fails (Plan reports unknown).

```yaml
- ufw:
    default: { incoming: deny, outgoing: allow }
    enabled: true
    rules:
      - { rule: allow, port: "22", proto: tcp }
      - { rule: allow, port: "80,443", proto: tcp }
      - { rule: allow, from: 100.64.0.0/10, port: "5432", comment: postgres via tailscale }
      - { rule: deny, port: "5432", proto: tcp }
```

## assert

A health check. It runs `cmd` and tests its output; it never changes
anything. The command must exit 0. With `regex`, the output must match.
With bounds, the number is the regex's first group (or the whole output,
trimmed, when there is no regex or no group): below `min` or above `max`
fails, below `warn_min` or above `warn_max` warns. With `warn: true`, a
failure warns instead.

A bound's finding names the number and the bound; the rest of the line
the regex matched, outside the match, is added to it. A check script that
prints a level and what it found (`2 ufw inactive`), asserted with
`regex: '^([0-2]) '`, `warn_max: 0` and `max: 1`, passes on 0, warns on 1
and fails on 2 with `2, above 1: ufw inactive`.

In a run it reports ok, warn or failed (a failure stops the play like any
other task). In Plan and Verify a failure is reported unknown and a
warning warn; Verify fails on unknown.

| key | type | |
|---|---|---|
| `name` | string | the check's line in the report |
| `cmd` | command | required |
| `regex` | regexp (RE2) | |
| `min`, `max` | number | fail bounds |
| `warn_min`, `warn_max` | number | warn bounds |
| `warn` | bool | a failure warns |
| `timeout` | duration | kill the command after this long; default 1m |

```yaml
- assert:
    name: disk use of /
    cmd: [df, --output=pcent, /]
    regex: '(\d+)%'
    warn_max: 80
    max: 90
- assert:
    name: nginx answers
    cmd: curl -fsS -o /dev/null -w '%{http_code}' http://127.0.0.1/
    regex: '^200$'
```

## query

A read-only command, run when the task is checked: in `plan` and
`verify` too. It is always `ok` and changes nothing. Its use is
`register`: later conditions read its `rc` and `stdout` in every mode. A
nonzero exit is not a failure; a command that could not run (missing
binary, timeout) is.

| key | type | |
|---|---|---|
| `name` | string | the resource's ID |
| `cmd` | command | required |
| `chdir` | path | default `/` |
| `env` | map | variables added to the environment |
| `timeout` | duration | default 1m |

```yaml
- name: other nginx acquisitions
  query: { cmd: "grep -rlq --include='*.yaml' /var/log/nginx /etc/crowdsec/acquis.d" }
  register: nginx_acquis
- file: { path: /etc/crowdsec/acquis.d/nginx.yaml, content: "..." }
  when: nginx_acquis.rc != 0
```

## probe

State a command reads, and the commands that put it right. `test` runs
when the task is checked: it is in place when it exits 0 and, with
`regex`, its output (surrounding whitespace removed) matches. When it is
not, `fix` runs, in order, and then `test` again: still failing is an
error, so a fix that does not work does not report `changed` on every
run. With no `fix`, a failing test fails the task (in `plan` and
`verify`: `unknown`).

| key | type | |
|---|---|---|
| `name` | string | the resource's ID |
| `test` | command | required |
| `regex` | regexp (RE2) | what the output must match |
| `drift` | string | what is reported when not in place; default the test and "fails" |
| `fix` | list of commands | |
| `env` | map | variables added to the environment of test and fix |
| `timeout` | duration | each fix command's; default 10m (the test's is 1m) |

```yaml
- probe:
    test: [findmnt, -no, OPTIONS, /tmp]
    regex: '(^|,)nr_inodes=8388608(,|$)'
    drift: remount /tmp
    fix: [[mount, -o, "remount,nr_inodes=8388608", /tmp]]
```

## fail

A task that always fails with `msg`, so the play stops there (in `plan`
and `verify`: `unknown`). With `when`, a refusal: a variable a later task
needs is empty, the host is not one the playbook is for.

| key | type | |
|---|---|---|
| `msg` | string | required |

```yaml
- fail: { msg: "set HOST_NAME: CrowdSec's alerts name the host" }
  when: [ "${SLACK_WEBHOOK_URL}", "not ${HOST_NAME}" ]
```

## restart

A handler: `systemctl restart <name>` (a stopped unit is started). An
action, run when a task notifies it.

| key | type | |
|---|---|---|
| `name` | string | required: the unit |

```yaml
handlers:
  - name: restart agent
    restart: { name: agent }
```

## reload

A handler: `systemctl reload <name>`. An action.

| key | type | |
|---|---|---|
| `name` | string | required: the unit |

```yaml
handlers:
  - name: reload nginx
    reload: { name: nginx }
```

## daemon_reload

A handler: `systemctl daemon-reload`, for unit files or fstab written by
other tasks. Takes no arguments. (The `systemd` module reloads by itself
when it writes a unit file.)

```yaml
handlers:
  - name: daemon-reload
    daemon_reload: {}
```

## validate_reload

A handler that validates a config before it reloads the service that
reads it. `validate` runs first; when it exits 0, `systemctl reload
<service>` runs (or the `then` commands, in order). When it fails,
nothing is reloaded, the handler fails, and each path in `restore` that a
`file`, `copy`, `line` or `block` task wrote in the same run is put back
as it was before the run (removed when the run created it). Handlers
that need this one do not run after it failed.

| key | type | |
|---|---|---|
| `validate` | command | required (`sshd -t`, `nginx -t`, `visudo -c`) |
| `service` | string | the unit to reload |
| `then` | list of commands | run instead of a reload |
| `restore` | list of paths | files to put back when validation fails |

`service` and `then` cannot both be given. With neither, the handler
only validates, and restores on failure.

```yaml
tasks:
  - lineinfile:
      path: /etc/ssh/sshd_config
      regexp: '^#?PasswordAuthentication'
      line: PasswordAuthentication no
    notify: reload sshd
handlers:
  - name: reload sshd
    validate_reload:
      validate: [sshd, -t]
      service: ssh
      restore: [/etc/ssh/sshd_config]
```

# host conformance

Package `host` on real distros: ubuntu-22.04, ubuntu-24.04, debian-12 and
debian-13, in containers of the distros' public images with what a stock
cloud image adds (CA roots, curl, gpg, a login user), built on first use
(`shiga-conformance:<distro>`).

```
go test -tags conformance -v ./host/conformance [-distros debian-12,ubuntu-24.04]
```

Each case runs one play four times: a dry run reports changed and leaves
the host as it was (the files under /etc, /usr/local, /srv, /var/lib and
dpkg's database unchanged), a run reports changed and puts the play's
effect in place, a second run reports ok and runs no handler, a dry run
reports ok. The cases cover apt (update, install, remove, purge, a
package left unpacked or half-configured, a local .deb, a deb822 repo with
an armored and a binary key, a source that changed in an earlier run whose
update failed), files, dirs, absent paths, links, lines, guarded commands,
a user and group, a download checked against its sha256, and a systemd
unit with its reload and restart handlers. Some cases check a refusal
instead: a download that fails its sha256, a link whose aside is taken, a
line through a link to nothing.

The test binary is built static and copied into each container, where it
runs as root. On the dev host it touches nothing. The images have no
systemd, so the systemd case runs in a second, privileged container of the
same image with systemd added (`shiga-conformance-systemd:<distro>`) and
booted as PID 1 in its own cgroup namespace. The apt cases need the
distro mirrors and nvidia.github.io.

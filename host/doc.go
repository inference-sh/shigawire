// Package host puts a host in a described state with idempotent resources:
// a file with its content, mode and owner; a line or a marked block in a
// file; a link; apt packages and repos; systemd units and their unit
// files; users and groups; a kernel parameter; ufw's policies and rules; a
// Docker swarm node and a Compose project; a download checked against its
// sha256; a command guarded by what it creates; a probe, a command's
// output checked and checked again after its fix; a query, a read-only
// command run in every mode for what it prints.
//
// Every resource reads the host first (Check) and changes it only when it
// is not in place yet (Apply, with what Check read), so running the same
// list twice changes nothing the second time. An action (a restart, a
// command with no guard) has no state to read: its Check sets
// State.Action, and its Apply runs every time.
//
// A Runner runs an ordered list of tasks in one of three ways:
//
//   - Run checks each task and applies those not in place. It reports
//     each as ok, changed, failed, ignored, warn or skipped, stops at the first
//     failure, and runs the handlers the changed tasks notified (a
//     daemon-reload after a unit changed) once each. A handler whose Needs
//     failed does not run.
//   - Plan checks each task and applies none: it reports what Run would
//     change and which handlers it would run, and unknown for a Check
//     that fails.
//   - Verify checks each task, runs no action and no handler, and fails
//     when a task drifted or its Check failed.
//
// Lazy, When and Assert fit what a play needs between resources: one
// made from what the host is when the play reaches it, one only while a
// condition holds, a condition it stops at (or warns on). Validated is a handler that validates a config before a
// reload, and puts back the files the run wrote when it does not pass.
// A Check or an Apply that returns a Warning is reported warn and the
// play goes on.
//
// Commands run through an Exec, so tests stand a fake in for the host:
// package hosttest has one, and a temporary root to point paths under.
// The package keeps no global state: each resource is a value that holds
// what it acts on. Beyond the standard library it uses only x/sys.
//
// The resources are for Linux: apt, dpkg and systemd, numeric owners, and
// commands in process groups. The package builds elsewhere, where an owner
// is never read and setting one fails.
package host

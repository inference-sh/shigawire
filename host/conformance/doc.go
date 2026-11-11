// Package conformance runs package host against real distros: each
// resource applied inside a container of the distro's public image
// (ubuntu:22.04, ubuntu:24.04, debian:12, debian:13) with what a cloud
// image has added, the systemd ones in a privileged container booted with
// systemd. Its tests build with the "conformance" tag and need Docker:
//
//	go test -tags conformance -v ./host/conformance
//
// The tests that touch a host run only inside those containers.
package conformance

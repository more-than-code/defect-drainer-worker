#!/bin/sh
# Linux sandbox layout test (remote-job-fixes criteria 2 and 5): builds the
# internal/worker test binary for linux, then runs TestSandboxLayoutBwrap in
# a throwaway Podman debian:bookworm container (git and bubblewrap, no
# network, no Go toolchain). bwrap needs a privileged container to create
# its namespaces. The test cannot run Claude's own sandbox (that needs the
# real CLI): it applies the generated settings file's rules with bwrap
# directly. See the test's comment.
#
# Usage: sh packaging/test/sandbox_test.sh
set -eu

CONTAINER=${CONTAINER:-podman}
IMAGE=localhost/dd-worker-install-test:bookworm

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

case "$(uname -m)" in
aarch64 | arm64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*)
	echo "sandbox_test: unsupported arch $(uname -m)" >&2
	exit 1
	;;
esac

(cd "$root" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$arch go test -c -o "$work/worker.test" ./internal/worker)
"$CONTAINER" build -q -t "$IMAGE" -f "$root/packaging/test/Containerfile" "$root/packaging/test" >/dev/null

"$CONTAINER" run --rm --privileged --network none \
	-e DD_SANDBOX_E2E=1 \
	-v "$work:/t:ro" \
	"$IMAGE" /t/worker.test -test.run '^TestSandboxLayoutBwrap$' -test.v -test.count=1

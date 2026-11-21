#!/bin/sh
# Linux sandbox layout test (remote-job-fixes criteria 2 and 5, worker-browser
# AC3 and AC6): builds the internal/worker test binary for linux, then runs
# TestSandboxLayoutBwrap and TestBrowserSandboxBwrap in a throwaway Podman
# debian:bookworm container (git, bubblewrap and socat, no network, no Go
# toolchain). bwrap needs a privileged container to create its namespaces.
# The tests cannot run Claude's own sandbox (that needs the real CLI): they
# apply the generated settings file's rules with bwrap directly. See the
# tests' comments.
#
# Then it builds an image with Debian's Postgres and runs
# TestDDStackPostgresBwrap (dd-stack boots Postgres and a sample service in
# bwrap, runs a command and leaves nothing running). It is a required gate:
# a failed image build fails the script.
#
# Then it builds an image with Debian's Chromium and runs
# TestDDShotChromium (dd-shot writes a PNG) and TestDDShotSignInChromium
# (job stack S10: dd-worker shot signs in over the DevTools protocol, with
# a linux dd-worker built for it). SKIP_BROWSER=1 skips that step;
# a failed image build is reported and skipped, never passed silently.
#
# Usage: sh packaging/test/sandbox_test.sh
set -eu

CONTAINER=${CONTAINER:-podman}
IMAGE=localhost/dd-worker-install-test:bookworm
BROWSER_IMAGE=localhost/dd-worker-browser-test:bookworm
STACK_IMAGE=localhost/dd-worker-stack-test:bookworm

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
(cd "$root" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -o "$work/dd-worker" ./cmd/dd-worker)
"$CONTAINER" build -q -t "$IMAGE" -f "$root/packaging/test/Containerfile" "$root/packaging/test" >/dev/null

"$CONTAINER" run --rm --privileged --network none \
	-e DD_SANDBOX_E2E=1 \
	-v "$work:/t:ro" \
	"$IMAGE" /t/worker.test -test.run '^(TestSandboxLayoutBwrap|TestBrowserSandboxBwrap)$' -test.v -test.count=1

"$CONTAINER" build -q -t "$STACK_IMAGE" -f "$root/packaging/test/Containerfile.stack" "$root/packaging/test" >/dev/null
"$CONTAINER" run --rm --privileged --network none \
	-e DD_STACK_E2E=1 \
	-v "$work:/t:ro" \
	"$STACK_IMAGE" /t/worker.test -test.run '^TestDDStackPostgresBwrap$' -test.v -test.count=1

if [ "${SKIP_BROWSER:-}" = 1 ]; then
	echo "sandbox_test: SKIP TestDDShotChromium (SKIP_BROWSER=1)"
	exit 0
fi
if ! "$CONTAINER" build -q -t "$BROWSER_IMAGE" -f "$root/packaging/test/Containerfile.browser" "$root/packaging/test" >/dev/null; then
	echo "sandbox_test: SKIP TestDDShotChromium (the Chromium image did not build)"
	exit 0
fi
"$CONTAINER" run --rm --privileged --network none \
	-e DD_BROWSER_E2E=1 -e DD_WORKER_BIN=/t/dd-worker \
	-v "$work:/t:ro" \
	"$BROWSER_IMAGE" /t/worker.test -test.run '^(TestDDShotChromium|TestDDShotSignInChromium)$' -test.v -test.count=1

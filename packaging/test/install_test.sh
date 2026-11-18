#!/bin/sh
# Container test of packaging/install.sh (docs/worker-w3.md §5 criterion 2).
#
# Builds two test releases of dd-worker for this machine's arch (linux), then
# runs packaging/test/in_container.sh in throwaway Podman debian:bookworm
# containers with no network: the releases are served as file:// URLs.
#   main:    layout, owners, modes, idempotent re-install, policy kept,
#            secrets, upgrade, rollback, automatic rollback, remove, purge
#   refuse:  a missing prerequisite and a wrong checksum install nothing
# systemd does not run in the container: upgrade/rollback use a fake
# systemctl/journalctl; the live service checks are done on a real host.
#
# Usage: sh packaging/test/install_test.sh   (or make worker-package-test)
set -eu

CONTAINER=${CONTAINER:-podman}
IMAGE=localhost/dd-worker-install-test:bookworm
V1=0.0.1-test
V2=0.0.2-test

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

case "$(uname -m)" in
aarch64 | arm64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*)
	echo "install_test: unsupported arch $(uname -m)" >&2
	exit 1
	;;
esac

for v in "$V1" "$V2"; do
	WORKER_VERSION=$v WORKER_PLATFORMS=linux_$arch sh "$root/packaging/dist.sh" "$work/release/v$v"
done

# The same V1 release with its tarball's sum changed (first hex digit flipped).
mkdir -p "$work/bad"
cp -R "$work/release/v$V1" "$work/bad/"
sums="$work/bad/v$V1/SHA256SUMS"
awk '/_linux_/ { c = substr($1, 1, 1); $1 = (c == "0" ? "1" : "0") substr($1, 2); print $1 "  " $2; next } { print }' \
	"$sums" >"$sums.new"
mv "$sums.new" "$sums"

"$CONTAINER" build -q -t "$IMAGE" -f "$root/packaging/test/Containerfile" "$root/packaging/test" >/dev/null

run() { # release-dir mode
	"$CONTAINER" run --rm --network=none \
		-v "$1:/release:ro" -v "$root/packaging/test:/test:ro" \
		-e V1="$V1" -e V2="$V2" "$IMAGE" sh /test/in_container.sh "$2"
}

status=0
run "$work/release" main || status=1
run "$work/bad" refuse || status=1
if [ "$status" = 0 ]; then
	echo "install_test: PASS"
else
	echo "install_test: FAIL" >&2
fi
exit "$status"

#!/bin/sh
# Builds the dd-worker release assets (docs/worker-w3.md §3.1) into OUT_DIR:
#   dd-worker_<V>_<os>_<arch>.tar.gz  one per platform
#   install.sh                         also attached on its own
#   SHA256SUMS                         over all of the above
#
# Usage: WORKER_VERSION=X.Y.Z[-pre] sh packaging/dist.sh [OUT_DIR]
# WORKER_PLATFORMS overrides the platform list (default: all three).
# WORKER_RELEASE_BASE (optional, https://…/releases/download) is written into
# the shipped install.sh as its default release base; the source keeps none.
set -eu

version=${WORKER_VERSION:-}
out=${1:-dist}
platforms=${WORKER_PLATFORMS:-linux_arm64 linux_amd64 darwin_arm64}

die() { printf 'dist.sh: %s\n' "$*" >&2; exit 1; }

case "$version" in
'' | *[!0-9A-Za-z.-]*) die "set WORKER_VERSION=X.Y.Z or X.Y.Z-pre (got '$version')" ;;
esac
printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' ||
	die "WORKER_VERSION must be X.Y.Z or X.Y.Z-pre (got '$version')"

root=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
# Remove only what this script writes; OUT_DIR may hold other things.
rm -f "$out"/dd-worker_*.tar.gz "$out/install.sh" "$out/SHA256SUMS"

stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

# The install.sh that ships: the source, with the release base stamped in.
installer="$stage/install.sh"
release_base=${WORKER_RELEASE_BASE:-}
if [ -n "$release_base" ]; then
	printf '%s\n' "$release_base" | grep -Eqx 'https://[A-Za-z0-9.-]+(/[A-Za-z0-9._-]+)+' ||
		die "WORKER_RELEASE_BASE must be a plain https:// URL (got '$release_base')"
	sed "s|^DEFAULT_RELEASE_BASE=''\$|DEFAULT_RELEASE_BASE='$release_base'|" \
		"$root/packaging/install.sh" >"$installer"
	grep -qx "DEFAULT_RELEASE_BASE='$release_base'" "$installer" ||
		die "could not stamp the release base into install.sh"
else
	cp "$root/packaging/install.sh" "$installer"
fi

# Archive entries are owned by root, not the build user. GNU tar (CI) and
# bsdtar (macOS) spell that differently; bsdtar must also skip AppleDouble files.
if tar --version 2>/dev/null | grep -q 'GNU tar'; then
	owner_flags='--owner=0 --group=0 --numeric-owner'
else
	owner_flags='--uid 0 --gid 0'
fi
export COPYFILE_DISABLE=1

files='dd-worker install.sh dd-worker.service policy.example.toml README-worker.md LICENSE NOTICE'

for p in $platforms; do
	os=${p%_*}
	arch=${p#*_}
	case "$p" in
	linux_arm64 | linux_amd64 | darwin_arm64) ;;
	*) die "unknown platform $p" ;;
	esac
	d="$stage/$p"
	mkdir -p "$d"
	(cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "$d/dd-worker" ./cmd/dd-worker)
	cp "$installer" "$root/packaging/dd-worker.service" \
		"$root/packaging/policy.example.toml" "$root/packaging/README-worker.md" \
		"$root/LICENSE" "$root/NOTICE" "$d/"
	chmod 0755 "$d/dd-worker" "$d/install.sh"
	chmod 0644 "$d/dd-worker.service" "$d/policy.example.toml" "$d/README-worker.md" "$d/LICENSE" \
		"$d/NOTICE"
	# shellcheck disable=SC2086 # owner_flags and files are intentional word lists
	tar -C "$d" $owner_flags -czf "$out/dd-worker_${version}_${p}.tar.gz" $files
	printf 'dist.sh: built dd-worker_%s_%s.tar.gz\n' "$version" "$p"
done

cp "$installer" "$out/install.sh"
chmod 0755 "$out/install.sh"

if command -v sha256sum >/dev/null 2>&1; then
	sum='sha256sum'
else
	sum='shasum -a 256'
fi
# shellcheck disable=SC2086 # sum is a command with its flags
(cd "$out" && $sum dd-worker_"$version"_*.tar.gz install.sh >SHA256SUMS)
printf 'dist.sh: wrote %s/SHA256SUMS\n' "$out"

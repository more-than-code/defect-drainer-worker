#!/bin/sh
# Fails if the tracked tree holds personal paths, private hosts or names that
# must not be published (docs/worker-repo-split.md criterion 7 in the control
# plane). Run from the repo root: sh scripts/tidy-check.sh
set -eu

pattern='/Users/|tutored|ttd-|10\.0\.|192\.168\.|supabase|\bpi[0-9]+\b|binartist'

# Every tracked file except this script, which has to spell the pattern out.
if git ls-files -z -- . ':!:scripts/tidy-check.sh' |
	xargs -0 grep -I -n -i -E "$pattern" --; then
	echo "tidy-check: the matches above must not be published" >&2
	exit 1
fi
echo "tidy-check: clean"

#!/bin/sh
# dd-shot: after-fix screenshots for a Defect Drainer worker browser job.
# Written into the job's handoff by the worker (worker-browser B3), with the
# four values below filled in.
#
# Usage, from the handoff, in ONE Bash call (a server does not outlive it):
#   sh dd-shot <DEF-id> <path> [<path>...]
#
# Runs the app's preview command in the worktree, waits up to 90 s for the
# preview URL, screenshots each path into fix-evidence/<DEF-id>/fix-NN.png
# (numbered after the files already there), stops the server and prints
# status lines only:
#   DD-SHOT OK <DEF-id> <file> <bytes>B <path>
#   DD-SHOT FAIL <DEF-id> <why>
# Exit 0 when every screenshot was written, 1 otherwise, 2 on bad arguments.
# The server's and the browser's output go to .dd-shot/ in the handoff.
# DD_PREVIEW_CMD and DD_PREVIEW_URL, when set, win over the values below.
# With a job stack the preview runs inside dd-stack, which boots the # dd-stack
# app's stack first, and dd-shot waits the app's preview timeout; a failed # dd-stack
# stack is reported as: DD-SHOT FAIL <DEF-id> stack <service> <step> log=<file> # dd-stack
# (then the log's last 20 lines). Lines ending in "# dd-stack" are written # dd-stack
# only for an app with a job stack. # dd-stack

dd_browser=''
dd_dir=''
dd_cmd=''
dd_url=''
dd_wait='' # dd-stack

usage() {
	echo "DD-SHOT FAIL usage: sh dd-shot <DEF-id> <path> [<path>...] (at most 8 paths, each starting with /)"
	exit 2
}

if [ $# -lt 2 ] || [ $# -gt 9 ]; then
	usage
fi
def=$1
shift
case $def in
'' | -* | *[!A-Za-z0-9_-]*) usage ;;
esac
[ ${#def} -le 128 ] || usage
for p in "$@"; do
	case $p in
	/*) ;;
	*) usage ;;
	esac
	if printf '%s' "$p" | grep -q '[^A-Za-z0-9/._~%?=&#-]'; then
		usage
	fi
done

cmd=${DD_PREVIEW_CMD:-$dd_cmd}
url=${DD_PREVIEW_URL:-$dd_url}
url=${url%/}
wait_s=${DD_SHOT_WAIT:-90}
wait_s=${DD_SHOT_WAIT:-$dd_wait} # dd-stack
shot_s=${DD_SHOT_TIMEOUT:-60}

# The job-stack variant redefines fail below. # dd-stack
# shellcheck disable=SC2317 # dd-stack
fail() {
	echo "DD-SHOT FAIL $def $*"
	exit 1
}
fail() { # dd-stack
	# dd-stack's deadline is dd-shot's: give it 3 s to report its failure. # dd-stack
	j=0 # dd-stack
	while [ ! -s .dd-stack/failed ] && [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && [ $j -lt 3 ]; do # dd-stack
		sleep 1 # dd-stack
		j=$((j + 1)) # dd-stack
	done # dd-stack
	if [ -s .dd-stack/failed ]; then # dd-stack
		read -r why <.dd-stack/failed # dd-stack
		echo "DD-SHOT FAIL $def stack $why" # dd-stack
		f=${why##*log=} # dd-stack
		[ -f "$f" ] && tail -n 20 "$f" # dd-stack
		exit 1 # dd-stack
	fi # dd-stack
	echo "DD-SHOT FAIL $def $*" # dd-stack
	exit 1 # dd-stack
} # dd-stack

if [ -z "$cmd" ] || [ -z "$url" ]; then
	fail "no-preview"
fi
command -v curl >/dev/null 2>&1 || fail "no-curl"
command -v timeout >/dev/null 2>&1 || fail "no-timeout"
[ -x "$dd_browser" ] || fail "no-browser"
[ -d "$dd_dir" ] || fail "no-worktree"

here=$(pwd)
out="fix-evidence/$def"
mkdir -p "$out" .dd-shot || fail "cannot-write $out"
# Chromium puts its profile-lock socket in $TMPDIR, and a unix socket path
# must stay under about 107 bytes: a long $TMPDIR gets a short one in /tmp.
tmp=${TMPDIR:-/tmp}
short=
if [ ${#tmp} -gt 60 ]; then
	short=$(mktemp -d /tmp/dd-shot.XXXXXX 2>/dev/null) && tmp=$short
fi

# The preview runs in its own process group so the whole tree stops.
pid=
stop() {
	[ -n "$pid" ] || return 0
	# "kill -SIG -PGID", not "kill -SIG -- -PGID": dash refuses the "--".
	kill -TERM "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null
	i=0
	while kill -0 "$pid" 2>/dev/null && [ $i -lt 5 ]; do
		sleep 1
		i=$((i + 1))
	done
	kill -KILL "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null
	wait "$pid" 2>/dev/null
	pid=
}
trap 'stop' EXIT
trap 'stop; exit 1' INT TERM

# The preview runs inside dd-stack, given by absolute path: it starts in # dd-stack
# the worktree. # dd-stack
[ -f dd-stack ] || fail "no-dd-stack" # dd-stack
rm -f .dd-stack/failed # dd-stack
DD_STACK="$here/dd-stack" DD_STACK_CMD=$cmd # dd-stack
export DD_STACK DD_STACK_CMD # dd-stack
cmd="exec sh \"\$DD_STACK\" run -- sh -c \"\$DD_STACK_CMD\"" # dd-stack
# setsid makes the preview a process group leader (set -m needs a tty, which
# the sandbox has none of); without it only the top process is stopped.
if command -v setsid >/dev/null 2>&1; then
	(cd "$dd_dir" && exec setsid sh -c "$cmd") >.dd-shot/server.log 2>&1 &
else
	(cd "$dd_dir" && exec sh -c "$cmd") >.dd-shot/server.log 2>&1 &
fi
pid=$!

up=0
i=0
while [ $i -lt "$wait_s" ]; do
	if curl --noproxy '*' -s -o /dev/null -m 2 "$url/"; then
		up=1
		break
	fi
	kill -0 "$pid" 2>/dev/null || fail "server-exited log=.dd-shot/server.log"
	sleep 1
	i=$((i + 1))
done
[ $up -eq 1 ] || fail "server-timeout ${wait_s}s log=.dd-shot/server.log"

n=0
for f in "$out"/fix-[0-9][0-9].png; do
	[ -e "$f" ] || continue
	k=${f##*/fix-}
	k=${k%.png}
	k=${k#0} # two digits: at most one leading zero
	[ "$k" -gt "$n" ] && n=$k
done

status=0
for p in "$@"; do
	n=$((n + 1))
	name=$(printf 'fix-%02d.png' "$n")
	prof="$tmp/dd-shot-prof-$$-$n"
	rm -rf "$prof"
	TMPDIR="$tmp" XDG_CONFIG_HOME="$tmp/dd-shot-xdg-$$" XDG_CACHE_HOME="$tmp/dd-shot-xdgc-$$" \
		timeout "$shot_s" "$dd_browser" --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage \
		--user-data-dir="$prof" --window-size=1280,800 --screenshot="$here/$out/$name" "$url$p" \
		>/dev/null 2>.dd-shot/browser.log
	rc=$?
	rm -rf "$prof"
	if [ -s "$out/$name" ]; then
		echo "DD-SHOT OK $def $out/$name $(wc -c <"$out/$name" | tr -d ' ')B $p"
	else
		rm -f "$out/$name"
		echo "DD-SHOT FAIL $def $p no-png rc=$rc log=.dd-shot/browser.log"
		status=1
	fi
done
rm -rf "$tmp/dd-shot-xdg-$$" "$tmp/dd-shot-xdgc-$$"
[ -z "$short" ] || rm -rf "$short"
stop
exit $status

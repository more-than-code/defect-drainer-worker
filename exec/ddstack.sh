#!/bin/sh
# dd-stack: the app's job stack for a Defect Drainer worker job.
# Written into the job's handoff by the worker (job stack S4), with
# the stack's definition filled in below.
#
# Usage, from the handoff, in ONE Bash call (the stack does not outlive it):
#   sh dd-stack run -- <cmd> [<arg>...]
#
# Boots a throwaway Postgres when a service needs it (initdb in $TMPDIR, a
# free loopback port), then each service in order: install, migrate, seeds,
# then start, waiting up to 120 s for its health URL. Then it runs <cmd> with
# the preview env, and stops everything, Postgres included, even on failure.
# The whole stack must be up within the app's preview timeout.
#
# It prints status lines only, never an env value:
#   DD-STACK UP postgres 127.0.0.1:<port>
#   DD-STACK OK <service> <step> <secs>s
#   DD-STACK UP <service> http://127.0.0.1:<port>
#   DD-STACK RUN
#   DD-STACK FAIL <service> <step> log=.dd-stack/<file>   (then its last 20 lines)
#   DD-STACK DOWN
# Exit: <cmd>'s status once the stack is up; 125 when the stack failed; 2 on
# bad arguments. Every step's output is kept in .dd-stack/ in the handoff.

# The functions below run from traps and from the definition dd_config
# brings in, which shellcheck cannot see.
# shellcheck disable=SC2317
dd_pg_bin=''
dd_timeout=90
dd_n=0
dd_ports=''
# The current service, set by svc_<n> below.
s_name=
s_dir=
s_port=
s_health=
s_start=
dd_config() { :; }
dd_config

usage() {
	echo "DD-STACK FAIL usage: sh dd-stack run -- <cmd> [<arg>...]"
	exit 2
}
if [ $# -lt 3 ] || [ "$1" != run ] || [ "$2" != -- ]; then
	usage
fi
shift 2

here=$(cd "$(dirname "$0")" && pwd) || exit 125
logs="$here/.dd-stack"
mkdir -p "$logs" || {
	echo "DD-STACK FAIL stack mkdir cannot-write .dd-stack"
	exit 125
}
rm -f "$logs"/*.log "$logs/failed"
start_s=$(date +%s)
deadline=$((start_s + dd_timeout))
tmp=${TMPDIR:-/tmp}
work=
pg_up=
dd_pg_url=
started=
cpid=

# The services run in their own process groups (setsid) so each whole tree
# stops; without setsid only the top process is signalled.
has_setsid=
command -v setsid >/dev/null 2>&1 && has_setsid=1

# stop_all stops every started service (TERM to all groups, up to 3 s, then
# KILL) and Postgres, then removes the throwaway data. It must stay well
# under 5 s: dd-shot kills its preview's group 5 s after a TERM.
stop_all() {
	if [ -n "$cpid" ]; then
		kill -TERM "$cpid" 2>/dev/null
		wait "$cpid" 2>/dev/null
		cpid=
	fi
	for p in $started; do
		kill -TERM "-$p" 2>/dev/null || kill -TERM "$p" 2>/dev/null
	done
	if [ -n "$pg_up" ]; then
		"$dd_pg_bin/pg_ctl" -D "$work/pg" -m fast -w -t 3 stop >/dev/null 2>&1 ||
			"$dd_pg_bin/pg_ctl" -D "$work/pg" -m immediate -w -t 2 stop >/dev/null 2>&1
		pg_up=
	fi
	i=0
	while [ "$i" -lt 6 ]; do
		alive=
		for p in $started; do
			kill -0 "$p" 2>/dev/null && alive=1
		done
		[ -n "$alive" ] || break
		sleep 0.5 2>/dev/null || sleep 1
		i=$((i + 1))
	done
	for p in $started; do
		kill -KILL "-$p" 2>/dev/null || kill -KILL "$p" 2>/dev/null
		wait "$p" 2>/dev/null
	done
	started=
	[ -z "$work" ] || rm -rf "$work"
	work=
}
down() {
	# A TERM now (dd-shot stopping its preview) must not cut the cleanup.
	trap '' INT TERM
	stop_all
	echo "DD-STACK DOWN"
}
trap 'down' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# fail <service> <step> <log>: the failure line, the log's last 20 lines,
# and .dd-stack/failed for dd-shot.
fail() {
	echo "$1 $2 log=.dd-stack/$3" >"$logs/failed"
	echo "DD-STACK FAIL $1 $2 log=.dd-stack/$3"
	[ -f "$logs/$3" ] && tail -n 20 "$logs/$3"
	exit 125
}

left() { echo $((deadline - $(date +%s))); }

# spawn <log> <cmd>: runs <cmd> in the current service's worktree with its
# env, in the background, as a process group leader; sets $spid.
spawn() {
	if [ -n "$has_setsid" ]; then
		(cd "$s_dir" && "env_$si" && exec setsid sh -c "$2") </dev/null >"$logs/$1" 2>&1 &
	else
		(cd "$s_dir" && "env_$si" && exec sh -c "$2") </dev/null >"$logs/$1" 2>&1 &
	fi
	spid=$!
}

# dd_step <step> <cmd>: one setup step of the current service, bounded by
# the stack's deadline.
dd_step() {
	f="$s_name-$1.log"
	t0=$(date +%s)
	spawn "$f" "$2"
	while kill -0 "$spid" 2>/dev/null; do
		if [ "$(left)" -le 0 ]; then
			kill -TERM "-$spid" 2>/dev/null || kill -TERM "$spid" 2>/dev/null
			sleep 1
			kill -KILL "-$spid" 2>/dev/null || kill -KILL "$spid" 2>/dev/null
			wait "$spid" 2>/dev/null
			fail "$s_name" "$1" "$f"
		fi
		sleep 0.2 2>/dev/null || sleep 1
	done
	wait "$spid" || fail "$s_name" "$1" "$f"
	echo "DD-STACK OK $s_name $1 $(($(date +%s) - t0))s"
}

# rand_port: a loopback port in 20000–49999 that no service uses.
rand_port() {
	while :; do
		n=$(od -An -N2 -tu2 /dev/urandom | tr -d ' ')
		port=$((20000 + n % 30000))
		case " $dd_ports " in
		*" $port "*) ;;
		*) return 0 ;;
		esac
	done
}

if [ -n "$dd_pg_bin" ]; then
	work=$(mktemp -d "$tmp/dd-stack.XXXXXX") || {
		echo "DD-STACK FAIL postgres initdb cannot-write \$TMPDIR"
		exit 125
	}
	"$dd_pg_bin/initdb" -D "$work/pg" -U postgres -A trust -E UTF8 --no-sync \
		</dev/null >"$logs/postgres-initdb.log" 2>&1 || fail postgres initdb postgres-initdb.log
	tries=0
	while :; do
		rand_port
		# No unix socket (TCP on loopback only); dynamic shared memory in
		# the data dir, so nothing outside $TMPDIR is needed.
		if "$dd_pg_bin/pg_ctl" -D "$work/pg" -l "$logs/postgres.log" -w -t "$(left)" \
			-o "-p $port -c listen_addresses=127.0.0.1 -c unix_socket_directories='' -c dynamic_shared_memory_type=mmap -c fsync=off" \
			start </dev/null >"$logs/postgres-ctl.log" 2>&1; then
			break
		fi
		tries=$((tries + 1))
		if [ $tries -ge 3 ] || [ "$(left)" -le 0 ]; then
			fail postgres start postgres.log
		fi
	done
	pg_up=1
	# Read by the env functions of the definition.
	# shellcheck disable=SC2034
	dd_pg_url="postgresql://postgres@127.0.0.1:$port/postgres"
	echo "DD-STACK UP postgres 127.0.0.1:$port"
fi

si=1
while [ $si -le "$dd_n" ]; do
	"svc_$si"
	"steps_$si"
	spawn "$s_name-start.log" "$s_start"
	started="$spid $started"
	h0=$(date +%s)
	until curl --noproxy '*' -fs -o /dev/null -m 2 "http://127.0.0.1:$s_port$s_health"; do
		kill -0 "$spid" 2>/dev/null || fail "$s_name" start "$s_name-start.log"
		if [ $(($(date +%s) - h0)) -ge 120 ] || [ "$(left)" -le 0 ]; then
			fail "$s_name" health "$s_name-start.log"
		fi
		sleep 1
	done
	echo "DD-STACK UP $s_name http://127.0.0.1:$s_port"
	si=$((si + 1))
done

echo "DD-STACK RUN"
# In the background so a TERM is handled at once (sh defers traps while a
# foreground child runs); stop_all stops it too.
(env_preview && exec "$@") <&0 &
cpid=$!
wait "$cpid"
rc=$?
cpid=
exit $rc

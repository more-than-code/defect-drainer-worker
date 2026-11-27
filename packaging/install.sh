#!/bin/sh
# dd-worker host installer (docs/worker-w3.md §3.4). POSIX sh, run as root.
# Every step is idempotent and says what it does. Nothing is installed from
# the network except the dd-worker release tarball, after its SHA256 checks.
#
#   sh install.sh install X.Y.Z      download, verify, unpack, set up the host
#   sh install.sh secrets            ANTHROPIC_API_KEY from stdin -> secrets.env
#                                    (root 0600: systemd LoadCredential reads it)
#   sh install.sh enroll <origin>    join token from stdin -> dd-worker init
#   sh install.sh start|stop|status
#   sh install.sh upgrade X.Y.Z      install alongside, switch, restart; rolls
#                                    back if not connected within 60 s
#   sh install.sh rollback           switch to the previous version (its unit
#                                    and the secrets.env owner it needs)
#   sh install.sh remove [--purge]   --purge also deletes state, config, user
#
# install and upgrade also install the self-update units
# (dd-worker-update.path and .service, docs/adr/0002-self-update.md). They do
# nothing until policy.toml sets auto_update = "server".
#
# DD_WORKER_RELEASE_BASE overrides the release base URL (https or file only);
# DD_WORKER_CONNECT_TIMEOUT the upgrade connect check (seconds, default 60).
set -eu

# Where releases live: <base>/vX.Y.Z/{dd-worker_*.tar.gz,SHA256SUMS}.
# The one place to set it once the public worker repo is fixed, e.g.
# https://github.com/<owner>/<repo>/releases/download
DEFAULT_RELEASE_BASE=''
RELEASE_BASE=${DD_WORKER_RELEASE_BASE:-$DEFAULT_RELEASE_BASE}
CONNECT_TIMEOUT=${DD_WORKER_CONNECT_TIMEOUT:-60}

WORKER_USER=dd-worker
LIB=/usr/local/lib/dd-worker
BIN_LINK=/usr/local/bin/dd-worker
ETC=/etc/dd-worker
STATE=/var/lib/dd-worker
WORKER_HOME=/home/dd-worker
UNIT=/etc/systemd/system/dd-worker.service
# Self-update (docs/adr/0002-self-update.md): the root step's units and dir.
UPDATE_PATH_UNIT=/etc/systemd/system/dd-worker-update.path
UPDATE_SERVICE_UNIT=/etc/systemd/system/dd-worker-update.service
UPDATE_ROOT=/var/lib/dd-worker-update
SECRETS=$ETC/secrets.env
WORKER_PATH=/usr/local/bin:/usr/bin:/bin
CONNECTED_LINE='connected (protocol'

say() { printf 'install.sh: %s\n' "$*"; }
die() {
	printf 'install.sh: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat >&2 <<'EOF'
usage: sh install.sh install X.Y.Z
       sh install.sh secrets            (ANTHROPIC_API_KEY on stdin)
       sh install.sh enroll <https://server-origin>   (join token on stdin)
       sh install.sh start | stop | status
       sh install.sh upgrade X.Y.Z
       sh install.sh rollback
       sh install.sh remove [--purge]
EOF
	exit 2
}

need_root() {
	[ "$(id -u)" = 0 ] || die "run as root: sudo sh install.sh $*"
}

check_version() {
	case "$1" in
	'' | *[!0-9A-Za-z.-]*) die "version must be X.Y.Z or X.Y.Z-pre (got '$1')" ;;
	esac
	printf '%s\n' "$1" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' ||
		die "version must be X.Y.Z or X.Y.Z-pre (got '$1')"
}

detect_arch() {
	[ "$(uname -s)" = Linux ] || die "dd-worker hosts are Linux (this is $(uname -s))"
	case "$(uname -m)" in
	aarch64 | arm64) ARCH=arm64 ;;
	x86_64 | amd64) ARCH=amd64 ;;
	*) die "unsupported architecture $(uname -m) (arm64 and amd64 only)" ;;
	esac
}

# Prints the apt line for anything missing; installs nothing itself.
check_prereqs() {
	missing=''
	for tool in curl:curl tar:tar sha256sum:coreutils bwrap:bubblewrap socat:socat \
		runuser:util-linux setsid:util-linux useradd:passwd; do
		command -v "${tool%%:*}" >/dev/null 2>&1 || missing="$missing ${tool#*:}"
	done
	if command -v git >/dev/null 2>&1; then
		gv=$(git --version | sed -n 's/^git version \([0-9][0-9]*\)\.\([0-9][0-9]*\).*/\1 \2/p')
		major=${gv% *}
		minor=${gv#* }
		if [ -z "$gv" ] || [ "$major" -lt 2 ] || { [ "$major" -eq 2 ] && [ "$minor" -lt 32 ]; }; then
			say "git $(git --version | cut -d' ' -f3) is older than 2.32 (the worker needs GIT_CONFIG_GLOBAL)"
			missing="$missing git"
		fi
	else
		missing="$missing git"
	fi
	if [ -n "$missing" ]; then
		say "missing or too old:$missing"
		say "install them first:  apt-get install -y$missing"
		die "prerequisites missing; nothing installed"
	fi
}

systemd_up() {
	[ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1
}

service_active() {
	systemd_up && systemctl is-active --quiet dd-worker
}

# The version /usr/local/bin/dd-worker points at, or empty.
# POSIX sh has no local variables, so helpers use their own cv_/sc_ names
# and never overwrite a caller's old, prev or cur.
current_version() {
	cv_link=$(readlink "$BIN_LINK" 2>/dev/null) || return 0
	cv_v=${cv_link#"$LIB"/}
	cv_v=${cv_v%/dd-worker}
	[ "$cv_link" = "$LIB/$cv_v/dd-worker" ] && printf '%s\n' "$cv_v"
	return 0
}

previous_version() {
	[ -f "$LIB/previous" ] && head -n 1 "$LIB/previous"
	return 0
}

# Downloads the tarball and SHA256SUMS for $1, verifies the sum, and unpacks
# to $LIB/$1. Touches nothing on the host until the sum matches.
fetch_version() {
	v=$1
	if [ -x "$LIB/$v/dd-worker" ]; then
		say "$LIB/$v already unpacked"
		return 0
	fi
	case "$RELEASE_BASE" in
	https://* | file://*) ;;
	'') die "no release base URL: set DD_WORKER_RELEASE_BASE=https://github.com/<owner>/<repo>/releases/download" ;;
	*) die "DD_WORKER_RELEASE_BASE must be https:// or file://" ;;
	esac
	tarball="dd-worker_${v}_linux_${ARCH}.tar.gz"
	url="$RELEASE_BASE/v$v"
	tmp=$(mktemp -d)
	# shellcheck disable=SC2064 # expand tmp now
	trap "rm -rf '$tmp'" EXIT
	say "downloading $url/$tarball"
	curl -fsSL --proto '=https,file' --proto-redir '=https' -o "$tmp/SHA256SUMS" "$url/SHA256SUMS" ||
		die "could not download SHA256SUMS; nothing installed"
	curl -fsSL --proto '=https,file' --proto-redir '=https' -o "$tmp/$tarball" "$url/$tarball" ||
		die "could not download $tarball; nothing installed"
	want=$(awk -v f="$tarball" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
	case "$want" in
	'' | *[!0-9a-f]*) die "SHA256SUMS has no single entry for $tarball; nothing installed" ;;
	esac
	[ "${#want}" -eq 64 ] || die "SHA256SUMS entry for $tarball is malformed; nothing installed"
	got=$(sha256sum "$tmp/$tarball" | cut -d' ' -f1)
	[ "$got" = "$want" ] || die "checksum mismatch for $tarball (want $want, got $got); nothing installed"
	say "checksum OK ($got)"

	mkdir -p "$LIB"
	chown root:root "$LIB"
	chmod 0755 "$LIB"
	stage="$LIB/.staging-$v.$$"
	rm -rf "$stage"
	mkdir "$stage"
	tar --no-same-owner --no-same-permissions -xzf "$tmp/$tarball" -C "$stage"
	# Only plain files and dirs: a symlink entry would make the chmods below
	# follow it out of the staging dir.
	if [ -n "$(find "$stage" ! -type f ! -type d | head -n 1)" ]; then
		rm -rf "$stage"
		die "$tarball holds a link or special file; nothing installed"
	fi
	[ -f "$stage/dd-worker" ] || {
		rm -rf "$stage"
		die "$tarball holds no dd-worker binary"
	}
	chown -R root:root "$stage"
	find "$stage" -type d -exec chmod 0755 {} +
	find "$stage" -type f -exec chmod 0644 {} +
	chmod 0755 "$stage/dd-worker" "$stage/install.sh"
	mv "$stage" "$LIB/$v"
	say "unpacked to $LIB/$v"
	rm -rf "$tmp"
	trap - EXIT
}

ensure_dir() { # path owner:group mode
	mkdir -p "$1"
	chown "$2" "$1"
	chmod "$3" "$1"
}

# The user, dirs, unit and example policy (docs/worker-w3.md §3.2). Never
# touches an existing policy.toml.
setup_host() {
	src=$1
	if id -u "$WORKER_USER" >/dev/null 2>&1; then
		say "user $WORKER_USER exists"
	else
		useradd --system --user-group --home-dir "$WORKER_HOME" --no-create-home \
			--shell /usr/sbin/nologin "$WORKER_USER"
		say "created system user $WORKER_USER"
	fi
	ensure_dir "$ETC" root:root 0755
	ensure_dir "$STATE" "$WORKER_USER:$WORKER_USER" 0700
	# credential/ sits inside a dir the worker owns, so root never chowns or
	# chmods it (a symlink there would aim root at any path): the worker
	# creates it, and a link is refused.
	if [ -L "$STATE/credential" ]; then
		die "$STATE/credential is a symlink; remove it and run install again"
	fi
	runuser -u "$WORKER_USER" -- mkdir -p -m 0700 "$STATE/credential"
	runuser -u "$WORKER_USER" -- chmod 0700 "$STATE/credential"
	ensure_dir "$WORKER_HOME" "$WORKER_USER:$WORKER_USER" 0700
	say "dirs: $ETC (root 0755), $STATE and $STATE/credential, $WORKER_HOME ($WORKER_USER 0700)"
	install -o root -g root -m 0644 "$src/policy.example.toml" "$ETC/policy.example.toml"
	if [ -f "$ETC/policy.toml" ]; then
		say "kept the existing $ETC/policy.toml"
	else
		say "no $ETC/policy.toml yet: the worker declines every job until you write one:"
		say "  cp $ETC/policy.example.toml $ETC/policy.toml && \$EDITOR $ETC/policy.toml"
	fi
	install_unit "$src/dd-worker.service"
	own_secrets "$UNIT"
	install_update_units "$src"
}

# The self-update units from release dir $1 (releases before self-update
# have none: nothing changes then). Harmless while policy.toml has
# auto_update = "off": the root step refuses every request.
install_update_units() {
	[ -f "$1/dd-worker-update.service" ] && [ -f "$1/dd-worker-update.path" ] || return 0
	ensure_dir "$UPDATE_ROOT" root:root 0755
	install -o root -g root -m 0644 "$1/dd-worker-update.service" "$UPDATE_SERVICE_UNIT"
	install -o root -g root -m 0644 "$1/dd-worker-update.path" "$UPDATE_PATH_UNIT"
	say "installed $UPDATE_PATH_UNIT and $UPDATE_SERVICE_UNIT (self-update: off until policy.toml sets auto_update = \"server\")"
	if systemd_up; then
		systemctl daemon-reload
		systemctl enable --now dd-worker-update.path
	fi
}

# Installs unit file $1 as the dd-worker unit and reloads systemd.
install_unit() {
	mkdir -p "$(dirname "$UNIT")"
	install -o root -g root -m 0644 "$1" "$UNIT"
	say "installed $UNIT"
	if systemd_up; then
		systemctl daemon-reload
	fi
}

# The owner secrets.env needs under unit file $1: root when systemd reads it
# for the worker (LoadCredential, 0.2.0-rc8 on), else the worker user
# (0.2.0-rc7 and older read the file themselves).
secrets_owner() {
	if [ -f "$1" ] && grep -q '^LoadCredential=secrets\.env:' "$1"; then
		printf 'root\n'
	else
		printf '%s\n' "$WORKER_USER"
	fi
}

# Gives secrets.env the owner unit file $1 needs, mode 0600. Under
# LoadCredential the unit does not start without the file, so a missing one
# is created empty (no keys, as a missing file meant before).
own_secrets() {
	os_owner=$(secrets_owner "$1")
	if [ ! -e "$SECRETS" ] && [ "$os_owner" = root ]; then
		(umask 077 && : >"$SECRETS")
		say "created an empty $SECRETS (no keys yet: sh install.sh secrets adds one)"
	fi
	[ -f "$SECRETS" ] || return 0
	chown "$os_owner:$os_owner" "$SECRETS"
	chmod 0600 "$SECRETS"
	say "$SECRETS: $os_owner 0600"
}

# Switches back to installed version $1 (rollback, and a failed upgrade):
# secrets.env's owner first, for the unit $1 shipped (an rc7 worker cannot
# read a root-owned file), then that unit, then the binary. The caller
# restarts.
switch_back() {
	sb_unit="$LIB/$1/dd-worker.service"
	if [ -f "$sb_unit" ]; then
		own_secrets "$sb_unit"
		install_unit "$sb_unit"
	else
		say "$LIB/$1 has no dd-worker.service: kept the installed unit"
	fi
	set_current "$1"
}

# Points /usr/local/bin/dd-worker at $1 (an atomic rename), records the old
# target as previous, and deletes every other version.
set_current() {
	sc_v=$1
	sc_old=$(current_version)
	if [ -n "$sc_old" ] && [ "$sc_old" != "$sc_v" ]; then
		printf '%s\n' "$sc_old" >"$LIB/previous.tmp"
		chmod 0644 "$LIB/previous.tmp"
		mv -f "$LIB/previous.tmp" "$LIB/previous"
	fi
	mkdir -p "$(dirname "$BIN_LINK")"
	ln -s "$LIB/$sc_v/dd-worker" "$BIN_LINK.tmp.$$"
	mv -f "$BIN_LINK.tmp.$$" "$BIN_LINK"
	say "$BIN_LINK -> $LIB/$sc_v/dd-worker"
	sc_prev=$(previous_version)
	for sc_d in "$LIB"/*; do
		[ -d "$sc_d" ] || continue
		sc_name=${sc_d#"$LIB"/}
		[ "$sc_name" = "$sc_v" ] || [ "$sc_name" = "$sc_prev" ] && continue
		rm -rf "$sc_d"
		say "removed old version $sc_name"
	done
}

# setsid: the worker process gets no controlling terminal, so it cannot push
# keystrokes into root's (TIOCSTI). stdin is still passed through.
run_as_worker() {
	setsid -w runuser -u "$WORKER_USER" -- env -i HOME="$WORKER_HOME" PATH="$WORKER_PATH" LANG=C.UTF-8 \
		"$BIN_LINK" "$@"
}

# Waits for the unit to be active with a "connected" log line since $1 (epoch).
wait_connected() {
	since=$1
	i=0
	while [ "$i" -lt "$CONNECT_TIMEOUT" ]; do
		if systemctl is-active --quiet dd-worker &&
			journalctl -u dd-worker --since "@$since" -o cat --no-pager 2>/dev/null |
			grep -qF "$CONNECTED_LINE"; then
			return 0
		fi
		sleep 1
		i=$((i + 1))
	done
	return 1
}

cmd_install() {
	[ $# -eq 1 ] || usage
	need_root install
	check_version "$1"
	detect_arch
	check_prereqs
	fetch_version "$1"
	setup_host "$LIB/$1"
	set_current "$1"
	if service_active; then
		say "dd-worker is running the old binary: sh install.sh upgrade $1 restarts it with a connect check"
	fi
	say "done. Next: policy.toml (above), then sh install.sh secrets, enroll, start"
}

cmd_upgrade() {
	[ $# -eq 1 ] || usage
	need_root upgrade
	check_version "$1"
	old=$(current_version)
	[ -n "$old" ] || die "dd-worker is not installed; use: sh install.sh install $1"
	[ "$old" != "$1" ] || {
		say "already on $1"
		return 0
	}
	detect_arch
	check_prereqs
	fetch_version "$1"
	setup_host "$LIB/$1"
	set_current "$1"
	if ! service_active; then
		say "dd-worker is not running: switched to $1 without a restart or connect check"
		return 0
	fi
	since=$(date +%s)
	say "restarting dd-worker on $1; waiting up to ${CONNECT_TIMEOUT}s for it to connect"
	systemctl restart dd-worker
	if wait_connected "$since"; then
		say "upgraded $old -> $1 (connected)"
		return 0
	fi
	say "$1 did not connect within ${CONNECT_TIMEOUT}s: rolling back to $old"
	switch_back "$old"
	systemctl restart dd-worker
	die "upgrade to $1 failed; back on $old (sh install.sh status, journalctl -u dd-worker)"
}

cmd_rollback() {
	[ $# -eq 0 ] || usage
	need_root rollback
	prev=$(previous_version)
	if [ -z "$prev" ] || [ ! -x "$LIB/$prev/dd-worker" ]; then
		die "no previous version to roll back to"
	fi
	cur=$(current_version)
	switch_back "$prev"
	if service_active; then
		systemctl restart dd-worker
		say "restarted dd-worker on $prev"
	fi
	say "rolled back ${cur:-?} -> $prev"
}

cmd_secrets() {
	[ $# -eq 0 ] || usage
	need_root secrets
	if [ ! -d "$ETC" ] || ! id -u "$WORKER_USER" >/dev/null 2>&1; then
		die "run sh install.sh install X.Y.Z first"
	fi
	key=''
	if [ -t 0 ]; then
		printf 'ANTHROPIC_API_KEY (input hidden): ' >&2
		stty -echo
		trap 'stty echo' EXIT INT TERM
		IFS= read -r key || true
		stty echo
		trap - EXIT INT TERM
		printf '\n' >&2
	else
		IFS= read -r key || true
	fi
	case "$key" in
	'') die "no key on stdin; secrets.env unchanged" ;;
	*[!A-Za-z0-9_-]*) die "the key has unexpected characters; secrets.env unchanged" ;;
	esac
	umask 077
	tmp=$(mktemp "$ETC/.secrets.env.XXXXXX")
	# shellcheck disable=SC2064 # expand tmp now
	trap "rm -f '$tmp'" EXIT
	# printf is a shell builtin: the key never appears in a process's argv.
	printf 'ANTHROPIC_API_KEY=%s\n' "$key" >"$tmp"
	key=''
	owner=$(secrets_owner "$UNIT")
	chown "$owner:$owner" "$tmp"
	chmod 0600 "$tmp"
	mv -f "$tmp" "$SECRETS"
	trap - EXIT
	say "wrote $SECRETS ($owner 0600)"
	if service_active; then
		systemctl restart dd-worker
		say "restarted dd-worker"
	fi
}

cmd_enroll() {
	[ $# -eq 1 ] || usage
	need_root enroll
	origin=$1
	case "$origin" in
	https://*) ;;
	*) die "the server origin must be https://… (got '$origin')" ;;
	esac
	case "$origin" in
	*[!A-Za-z0-9.:/_-]*) die "the server origin has unexpected characters" ;;
	esac
	[ -x "$BIN_LINK" ] || die "dd-worker is not installed; run sh install.sh install X.Y.Z first"
	say "enrolling with $origin as $WORKER_USER"
	if [ -t 0 ]; then
		printf 'Join token (input hidden): ' >&2
		stty -echo
		trap 'stty echo' EXIT INT TERM
		IFS= read -r token || true
		stty echo
		trap - EXIT INT TERM
		printf '\n' >&2
		[ -n "$token" ] || die "no join token entered"
		printf '%s\n' "$token" | run_as_worker init --server "$origin" --join-token-stdin
		token=''
	else
		run_as_worker init --server "$origin" --join-token-stdin
	fi
	say "enrolled. Next: sh install.sh start"
}

cmd_start() {
	need_root start
	systemd_up || die "systemd is not running"
	if [ ! -e "$SECRETS" ] && [ "$(secrets_owner "$UNIT")" = root ]; then
		die "$SECRETS is missing and the unit needs it: sh install.sh secrets, or install again for an empty one"
	fi
	systemctl enable --now dd-worker
	say "dd-worker enabled and started"
	cmd_status
}

cmd_stop() {
	need_root stop
	systemd_up || die "systemd is not running"
	systemctl stop dd-worker
	say "dd-worker stopped"
}

cmd_status() {
	need_root status
	if systemd_up; then
		systemctl status --no-pager --lines=5 dd-worker || true
	fi
	[ -x "$BIN_LINK" ] || die "dd-worker is not installed"
	printf 'version:    %s\n' "$(current_version)"
	if [ ! -e "$SECRETS" ]; then
		printf 'secrets.env: missing\n'
	elif [ "$(secrets_owner "$UNIT")" = root ]; then
		printf 'secrets.env: %s (via LoadCredential)\n' "$(stat -c '%U 0%a' "$SECRETS")"
	else
		printf 'secrets.env: %s\n' "$(stat -c '%U 0%a' "$SECRETS")"
	fi
	run_as_worker status
}

cmd_remove() {
	purge=0
	case "${1:-}" in
	'') ;;
	--purge) purge=1 ;;
	*) usage ;;
	esac
	need_root remove
	if systemd_up; then
		systemctl disable --now dd-worker-update.path 2>/dev/null || true
		systemctl disable --now dd-worker 2>/dev/null || true
	fi
	rm -f "$UNIT" "$UPDATE_PATH_UNIT" "$UPDATE_SERVICE_UNIT"
	rm -rf "$UPDATE_ROOT"
	if systemd_up; then
		systemctl daemon-reload
	fi
	rm -f "$BIN_LINK"
	rm -rf "$LIB"
	say "stopped and removed the unit and binaries"
	if [ "$purge" = 1 ]; then
		rm -rf "$STATE" "$ETC" "$WORKER_HOME"
		if id -u "$WORKER_USER" >/dev/null 2>&1; then
			userdel "$WORKER_USER"
		fi
		if getent group "$WORKER_USER" >/dev/null 2>&1; then
			groupdel "$WORKER_USER"
		fi
		say "purged $STATE, $ETC, $WORKER_HOME and user $WORKER_USER"
	else
		say "kept $ETC and $STATE (remove --purge deletes them)"
	fi
	say "the worker is not revoked: revoke it in Settings -> Workers"
}

case "$CONNECT_TIMEOUT" in
'' | *[!0-9]*) die "DD_WORKER_CONNECT_TIMEOUT must be a number of seconds" ;;
esac

[ $# -ge 1 ] || usage
verb=$1
shift
case "$verb" in
install) cmd_install "$@" ;;
upgrade) cmd_upgrade "$@" ;;
rollback) cmd_rollback "$@" ;;
secrets) cmd_secrets "$@" ;;
enroll) cmd_enroll "$@" ;;
start) cmd_start "$@" ;;
stop) cmd_stop "$@" ;;
status) cmd_status "$@" ;;
remove) cmd_remove "$@" ;;
*) usage ;;
esac

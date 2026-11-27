#!/bin/sh
# Runs inside the throwaway test container (see install_test.sh). /release holds
# v$V1/ and v$V2/ release dirs; /test is this directory.
set -u

LIB=/usr/local/lib/dd-worker
LINK=/usr/local/bin/dd-worker
UNIT=/etc/systemd/system/dd-worker.service
INSTALL=/release/v$V1/install.sh
SECRETS=/etc/dd-worker/secrets.env
LC_LINE='LoadCredential=secrets.env:/etc/dd-worker/secrets.env'
export DD_WORKER_RELEASE_BASE=file:///release

failures=0
ok() { printf 'ok   - %s\n' "$*"; }
not_ok() {
	printf 'FAIL - %s\n' "$*"
	failures=$((failures + 1))
}
check() { # description, then a command
	d=$1
	shift
	if "$@" >/dev/null 2>&1; then ok "$d"; else not_ok "$d"; fi
}
attrs() { # path "owner:group mode"
	got=$(stat -c '%U:%G %a' "$1" 2>/dev/null || echo missing)
	if [ "$got" = "$2" ]; then ok "$1 is $2"; else not_ok "$1 is $2 (got $got)"; fi
}
links_to() {
	got=$(readlink "$LINK" 2>/dev/null || echo none)
	if [ "$got" = "$LIB/$1/dd-worker" ]; then ok "$LINK -> $1"; else not_ok "$LINK -> $1 (got $got)"; fi
}
absent() { [ ! -e "$1" ] && [ ! -L "$1" ]; }
worker_reads() { runuser -u dd-worker -- cat "$SECRETS"; }
# The last restart's view: "owner=<secrets.env owner> lc=<1 if the unit had
# LoadCredential>", logged by the fake systemctl at the moment of the restart.
last_restart_is() {
	got=$(grep '^at-restart ' /tmp/systemctl.log | tail -n 1)
	if [ "$got" = "at-restart $1" ]; then ok "at the restart: $1"; else not_ok "at the restart: $1 (got '$got')"; fi
}
# daemon-reload is logged after the unit's last change and before the restart.
reload_before_restart() {
	awk '/^daemon-reload$/ { r = NR } /^restart dd-worker$/ { s = NR } END { exit !(r && s && r < s) }' /tmp/systemctl.log
}
inst() { sh "$INSTALL" "$@" >/tmp/out 2>&1; }
out_has() { grep -qF -- "$1" /tmp/out; }

layout() { # version
	attrs $LIB "root:root 755"
	attrs "$LIB/$1" "root:root 755"
	attrs "$LIB/$1/dd-worker" "root:root 755"
	attrs /etc/dd-worker "root:root 755"
	attrs /etc/dd-worker/policy.example.toml "root:root 644"
	attrs /var/lib/dd-worker "dd-worker:dd-worker 700"
	attrs /var/lib/dd-worker/credential "dd-worker:dd-worker 700"
	attrs /home/dd-worker "dd-worker:dd-worker 700"
	attrs $UNIT "root:root 644"
	attrs /etc/systemd/system/dd-worker-update.path "root:root 644"
	attrs /etc/systemd/system/dd-worker-update.service "root:root 644"
	attrs /var/lib/dd-worker-update "root:root 755"
	links_to "$1"
}

fake_systemd() {
	mkdir -p /run/systemd/system /opt/fake/bin
	cat >/opt/fake/bin/systemctl <<'EOF'
#!/bin/sh
echo "$*" >>/tmp/systemctl.log
case "$*" in
"is-active --quiet dd-worker") [ -e /tmp/fake-active ] ;;
"restart dd-worker" | "enable --now dd-worker")
	echo "at-restart owner=$(stat -c %U /etc/dd-worker/secrets.env 2>/dev/null || echo missing) lc=$(grep -c '^LoadCredential=secrets.env:' /etc/systemd/system/dd-worker.service)" >>/tmp/systemctl.log
	touch /tmp/fake-active
	;;
"stop dd-worker" | "disable --now dd-worker") rm -f /tmp/fake-active ;;
*) exit 0 ;;
esac
EOF
	cat >/opt/fake/bin/journalctl <<'EOF'
#!/bin/sh
[ -e /tmp/fake-connects ] && echo "connected (protocol 1, heartbeat 20s)"
exit 0
EOF
	chmod 0755 /opt/fake/bin/systemctl /opt/fake/bin/journalctl
	PATH=/opt/fake/bin:$PATH
	export PATH
}

test_main() {
	echo "# clean install $V1 (no systemd)"
	if inst install "$V1"; then ok "install $V1"; else
		not_ok "install $V1"
		cat /tmp/out
	fi
	# shellcheck disable=SC2016 # expanded by the inner sh
	check "dd-worker is a system user with no login shell" sh -c \
		'[ "$(id -u dd-worker)" -lt 1000 ] && getent passwd dd-worker | grep -q ":/home/dd-worker:/usr/sbin/nologin$"'
	layout "$V1"
	check "the unit is the release's dd-worker.service" cmp "$LIB/$V1/dd-worker.service" $UNIT
	check "the unit reads secrets.env through LoadCredential" grep -qx "$LC_LINE" $UNIT
	check "the unit has no EnvironmentFile" sh -c "! grep -q '^EnvironmentFile' $UNIT"
	attrs $SECRETS "root:root 600"
	check "a missing secrets.env is created empty" sh -c "[ ! -s $SECRETS ]"
	check "install says it created an empty secrets.env" out_has "created an empty $SECRETS"
	check "dd-worker cannot read the root-owned secrets.env" sh -c "! runuser -u dd-worker -- cat $SECRETS"
	check "no policy.toml is created" absent /etc/dd-worker/policy.toml
	check "the update units are the release's" sh -c "cmp $LIB/$V1/dd-worker-update.path /etc/systemd/system/dd-worker-update.path && cmp $LIB/$V1/dd-worker-update.service /etc/systemd/system/dd-worker-update.service"
	check "the update service runs the installed binary as root" sh -c "grep -qx 'ExecStart=/usr/local/bin/dd-worker update-run' /etc/systemd/system/dd-worker-update.service && grep -qx 'User=root' /etc/systemd/system/dd-worker-update.service"
	check "update-run refuses a non-root caller" sh -c "! runuser -u dd-worker -- $LINK update-run"
	for f in LICENSE NOTICE README-worker.md install.sh policy.example.toml dd-worker-update.path dd-worker-update.service; do
		check "the tarball holds $f" test -f "$LIB/$V1/$f"
	done
	check "dd-worker version prints $V1" sh -c "$LINK version | grep -qx '$V1'"

	echo "# re-install keeps an existing policy.toml"
	cp /etc/dd-worker/policy.example.toml /etc/dd-worker/policy.toml
	echo "# local edit" >>/etc/dd-worker/policy.toml
	chmod 0644 /etc/dd-worker/policy.toml
	before=$(sha256sum /etc/dd-worker/policy.toml)
	check "re-install $V1 succeeds" inst install "$V1"
	check "re-install says it kept policy.toml" out_has "kept the existing /etc/dd-worker/policy.toml"
	check "policy.toml is unchanged" sh -c "[ \"\$(sha256sum /etc/dd-worker/policy.toml)\" = '$before' ]"
	layout "$V1"
	check "policy.example.toml is valid for the worker" sh -c \
		"runuser -u dd-worker -- $LINK status 2>&1 | grep -q '^policy: *valid, enabled=true, orgs=more-than-code, adapters=claude'"

	echo "# a credential symlink planted by the worker is refused, not followed"
	rmdir /var/lib/dd-worker/credential
	runuser -u dd-worker -- ln -s /usr/local/lib/dd-worker /var/lib/dd-worker/credential
	if inst install "$V1"; then not_ok "install refuses a credential symlink"; else ok "install refuses a credential symlink"; fi
	attrs $LIB "root:root 755"
	rm /var/lib/dd-worker/credential
	check "install after removing the link" inst install "$V1"
	attrs /var/lib/dd-worker/credential "dd-worker:dd-worker 700"

	echo "# secrets"
	key='sk-ant-api03-TEST_only-0123456789'
	if printf '%s\n' "$key" | inst secrets; then ok "secrets"; else
		not_ok "secrets"
		cat /tmp/out
	fi
	attrs $SECRETS "root:root 600"
	check "secrets says root 0600" out_has "wrote $SECRETS (root 0600)"
	check "dd-worker cannot read secrets.env" sh -c "! runuser -u dd-worker -- cat $SECRETS"
	check "secrets.env holds the key" sh -c "[ \"\$(cat /etc/dd-worker/secrets.env)\" = 'ANTHROPIC_API_KEY=$key' ]"
	check "the key is not in install.sh's output" sh -c "! grep -qF '$key' /tmp/out"
	check "a key with a space is refused" sh -c "! printf 'bad key\n' | sh $INSTALL secrets"
	check "an empty key is refused" sh -c "! printf '' | sh $INSTALL secrets"
	check "secrets.env is unchanged after the refusals" sh -c "[ \"\$(cat /etc/dd-worker/secrets.env)\" = 'ANTHROPIC_API_KEY=$key' ]"
	check "no temp secrets file is left" sh -c '! ls -A /etc/dd-worker | grep -q "^\.secrets"'
	check "re-install keeps the key" inst install "$V1"
	check "secrets.env still holds the key" sh -c "[ \"\$(cat $SECRETS)\" = 'ANTHROPIC_API_KEY=$key' ]"
	attrs $SECRETS "root:root 600"
	inst status
	check "status says root 0600 via LoadCredential" out_has "secrets.env: root 0600 (via LoadCredential)"

	echo "# enroll refuses a non-https origin"
	check "enroll http:// is refused" sh -c "! printf 'dd_wjn_x\n' | sh $INSTALL enroll http://example.com"

	echo "# upgrade (fake systemd, connects)"
	fake_systemd
	touch /tmp/fake-active /tmp/fake-connects
	if inst upgrade "$V2"; then ok "upgrade $V2"; else
		not_ok "upgrade $V2"
		cat /tmp/out
	fi
	links_to "$V2"
	check "it says upgraded $V1 -> $V2" out_has "upgraded $V1 -> $V2 (connected)"
	check "previous is $V1" sh -c "[ \"\$(cat $LIB/previous)\" = '$V1' ]"
	check "upgrade restarted the service" grep -qx "restart dd-worker" /tmp/systemctl.log
	check "upgrade enabled the update trigger" grep -qx "enable --now dd-worker-update.path" /tmp/systemctl.log
	check "upgrade reloaded systemd" grep -qx "daemon-reload" /tmp/systemctl.log
	check "daemon-reload came before the restart" reload_before_restart
	last_restart_is "owner=root lc=1"
	check "dd-worker version prints $V2" sh -c "$LINK version | grep -qx '$V2'"
	layout "$V2"

	echo "# rollback"
	check "rollback" inst rollback
	links_to "$V1"
	check "it says rolled back $V2 -> $V1" out_has "rolled back $V2 -> $V1"
	check "previous is $V2" sh -c "[ \"\$(cat $LIB/previous)\" = '$V2' ]"
	check "both versions are kept" test -x "$LIB/$V2/dd-worker"
	check "rollback restored $V1's unit" cmp "$LIB/$V1/dd-worker.service" $UNIT
	attrs $SECRETS "root:root 600"

	echo "# start refuses a missing secrets.env under LoadCredential"
	mv $SECRETS /tmp/secrets.keep
	if inst start; then not_ok "start without secrets.env fails"; else ok "start without secrets.env fails"; fi
	check "it names install.sh secrets" out_has "sh install.sh secrets"
	inst status
	check "status says secrets.env is missing" out_has "secrets.env: missing"
	mv /tmp/secrets.keep $SECRETS

	echo "# rollback to a LoadCredential version recreates a missing secrets.env"
	check "upgrade $V2 again" inst upgrade "$V2"
	mv $SECRETS /tmp/secrets.keep
	check "rollback with no secrets.env" inst rollback
	attrs $SECRETS "root:root 600"
	last_restart_is "owner=root lc=1"
	mv /tmp/secrets.keep $SECRETS

	echo "# upgrade that never connects rolls back"
	rm -f /tmp/fake-connects
	if DD_WORKER_CONNECT_TIMEOUT=2 inst upgrade "$V2"; then not_ok "a failed upgrade exits non-zero"; else ok "a failed upgrade exits non-zero"; fi
	check "it says it rolled back" out_has "rolling back to $V1"
	check "it says it is back on $V1" out_has "back on $V1"
	links_to "$V1"

	echo "# an unknown version installs nothing"
	check "install 9.9.9 fails" sh -c "! sh $INSTALL install 9.9.9"
	links_to "$V1"
	check "no 9.9.9 dir" absent $LIB/9.9.9
	check "a bad version string is refused" sh -c "! sh $INSTALL install '1.2.3;id'"

	echo "# remove"
	check "remove" inst remove
	check "remove disabled the unit" grep -qx "disable --now dd-worker" /tmp/systemctl.log
	check "remove disabled the update trigger" grep -qx "disable --now dd-worker-update.path" /tmp/systemctl.log
	for p in $UNIT $LINK $LIB /etc/systemd/system/dd-worker-update.path /etc/systemd/system/dd-worker-update.service /var/lib/dd-worker-update; do check "removed $p" absent "$p"; done
	for p in /etc/dd-worker/policy.toml /etc/dd-worker/secrets.env /var/lib/dd-worker /home/dd-worker; do
		check "kept $p" test -e "$p"
	done
	check "kept user dd-worker" id dd-worker

	echo "# remove --purge"
	check "remove --purge" inst remove --purge
	for p in /etc/dd-worker /var/lib/dd-worker /home/dd-worker; do check "purged $p" absent "$p"; done
	check "user dd-worker is gone" sh -c '! id dd-worker'
	check "group dd-worker is gone" sh -c '! getent group dd-worker'
}

# An older release (V0, unit without LoadCredential) upgraded to and rolled
# back from this one (V1), with this release's installer, as on a host.
test_compat() {
	OLD=/release/v$V0/install.sh
	key='sk-ant-api03-TEST_only-compat-0123'
	echo "# $V0 installed by its own installer"
	if sh "$OLD" install "$V0" >/tmp/out 2>&1; then ok "install $V0"; else
		not_ok "install $V0"
		cat /tmp/out
	fi
	check "$V0's unit has no LoadCredential" sh -c "! grep -q '^LoadCredential=' $UNIT"
	if printf '%s\n' "$key" | sh "$OLD" secrets >/tmp/out 2>&1; then ok "secrets on $V0"; else not_ok "secrets on $V0"; fi
	attrs $SECRETS "dd-worker:dd-worker 600"
	check "dd-worker reads secrets.env on $V0" worker_reads

	echo "# upgrade $V0 -> $V1 (fake systemd, connects)"
	fake_systemd
	touch /tmp/fake-active /tmp/fake-connects
	if inst upgrade "$V1"; then ok "upgrade $V1"; else
		not_ok "upgrade $V1"
		cat /tmp/out
	fi
	links_to "$V1"
	check "the unit is $V1's" cmp "$LIB/$V1/dd-worker.service" $UNIT
	attrs $SECRETS "root:root 600"
	check "the key is kept" sh -c "[ \"\$(cat $SECRETS)\" = 'ANTHROPIC_API_KEY=$key' ]"
	check "dd-worker cannot read secrets.env on $V1" sh -c "! runuser -u dd-worker -- cat $SECRETS"
	check "daemon-reload came before the restart" reload_before_restart
	last_restart_is "owner=root lc=1"
	check "it restarted once" sh -c "[ \"\$(grep -cx 'restart dd-worker' /tmp/systemctl.log)\" = 1 ]"

	echo "# rollback $V1 -> $V0"
	check "rollback" inst rollback
	links_to "$V0"
	check "the unit is $V0's again" cmp "$LIB/$V0/dd-worker.service" $UNIT
	attrs $SECRETS "dd-worker:dd-worker 600"
	check "dd-worker reads secrets.env on $V0 again" worker_reads
	last_restart_is "owner=dd-worker lc=0"

	echo "# upgrade $V0 -> $V1 that never connects goes back to $V0"
	rm -f /tmp/fake-connects
	if DD_WORKER_CONNECT_TIMEOUT=2 inst upgrade "$V1"; then not_ok "a failed upgrade exits non-zero"; else ok "a failed upgrade exits non-zero"; fi
	check "it says it is back on $V0" out_has "back on $V0"
	links_to "$V0"
	check "the unit is $V0's" cmp "$LIB/$V0/dd-worker.service" $UNIT
	attrs $SECRETS "dd-worker:dd-worker 600"
	last_restart_is "owner=dd-worker lc=0"
	check "secrets on $V0's unit writes dd-worker 0600" sh -c "printf '%s\n' '$key' | sh $INSTALL secrets"
	attrs $SECRETS "dd-worker:dd-worker 600"
}

nothing_installed() {
	check "$1: no user" sh -c '! id dd-worker'
	for p in /etc/dd-worker /var/lib/dd-worker /home/dd-worker $LIB $LINK $UNIT; do
		check "$1: no $p" absent "$p"
	done
}

test_refuse() {
	echo "# a missing prerequisite (socat) installs nothing"
	mv /usr/bin/socat /usr/bin/socat.off
	if inst install "$V1"; then not_ok "install without socat fails"; else ok "install without socat fails"; fi
	check "it prints the apt line" out_has "apt-get install -y socat"
	nothing_installed "no socat"
	mv /usr/bin/socat.off /usr/bin/socat

	echo "# git older than 2.32 installs nothing"
	mkdir -p /opt/oldgit
	printf '#!/bin/sh\necho "git version 2.31.1"\n' >/opt/oldgit/git
	chmod 0755 /opt/oldgit/git
	if PATH=/opt/oldgit:$PATH inst install "$V1"; then not_ok "install with git 2.31 fails"; else ok "install with git 2.31 fails"; fi
	check "it says git is older than 2.32" out_has "is older than 2.32"
	check "it prints the apt line for git" out_has "apt-get install -y git"
	nothing_installed "git 2.31"

	echo "# no release base URL installs nothing"
	if DD_WORKER_RELEASE_BASE='' inst install "$V1"; then not_ok "install with no base URL fails"; else ok "install with no base URL fails"; fi
	check "it names DD_WORKER_RELEASE_BASE" out_has "set DD_WORKER_RELEASE_BASE="
	nothing_installed "no base URL"

	echo "# a correctly summed tarball holding a symlink installs nothing"
	v=6.6.6
	mkdir -p "/tmp/evil/v$v" /tmp/evil-src
	cp "/release/v$V1/install.sh" /tmp/evil-src/
	ln -s /etc/shadow /tmp/evil-src/dd-worker
	tarball="dd-worker_${v}_linux_$(dpkg --print-architecture).tar.gz"
	tar -C /tmp/evil-src -czf "/tmp/evil/v$v/$tarball" dd-worker install.sh
	(cd "/tmp/evil/v$v" && sha256sum "$tarball" >SHA256SUMS)
	shadow_before=$(stat -c '%U:%G %a' /etc/shadow)
	if DD_WORKER_RELEASE_BASE=file:///tmp/evil inst install "$v"; then not_ok "install of a tarball with a symlink fails"; else ok "install of a tarball with a symlink fails"; fi
	check "it says it holds a link" out_has "holds a link or special file"
	check "/etc/shadow is untouched" sh -c "[ \"\$(stat -c '%U:%G %a' /etc/shadow)\" = '$shadow_before' ]"
	check "no $v dir or staging dir" sh -c "! ls -A $LIB 2>/dev/null | grep -q ."
	rm -rf $LIB

	echo "# a wrong checksum installs nothing"
	if inst install "$V1"; then not_ok "install with a wrong sum fails"; else ok "install with a wrong sum fails"; fi
	check "it says checksum mismatch" out_has "checksum mismatch"
	nothing_installed "wrong sum"
}

case "${1:-}" in
main) test_main ;;
compat) test_compat ;;
refuse) test_refuse ;;
*)
	echo "usage: in_container.sh main|compat|refuse" >&2
	exit 2
	;;
esac

if [ "$failures" -eq 0 ]; then
	echo "# $1: all passed"
else
	echo "# $1: $failures failed"
	exit 1
fi

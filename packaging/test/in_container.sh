#!/bin/sh
# Runs inside the throwaway test container (see install_test.sh). /release holds
# v$V1/ and v$V2/ release dirs; /test is this directory.
set -u

LIB=/usr/local/lib/dd-worker
LINK=/usr/local/bin/dd-worker
UNIT=/etc/systemd/system/dd-worker.service
INSTALL=/release/v$V1/install.sh
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
	links_to "$1"
}

fake_systemd() {
	mkdir -p /run/systemd/system /opt/fake/bin
	cat >/opt/fake/bin/systemctl <<'EOF'
#!/bin/sh
echo "$*" >>/tmp/systemctl.log
case "$*" in
"is-active --quiet dd-worker") [ -e /tmp/fake-active ] ;;
"restart dd-worker" | "enable --now dd-worker") touch /tmp/fake-active ;;
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
	check "no policy.toml is created" absent /etc/dd-worker/policy.toml
	for f in LICENSE NOTICE README-worker.md install.sh policy.example.toml; do
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
	attrs /etc/dd-worker/secrets.env "dd-worker:dd-worker 600"
	check "secrets.env holds the key" sh -c "[ \"\$(cat /etc/dd-worker/secrets.env)\" = 'ANTHROPIC_API_KEY=$key' ]"
	check "the key is not in install.sh's output" sh -c "! grep -qF '$key' /tmp/out"
	check "a key with a space is refused" sh -c "! printf 'bad key\n' | sh $INSTALL secrets"
	check "an empty key is refused" sh -c "! printf '' | sh $INSTALL secrets"
	check "secrets.env is unchanged after the refusals" sh -c "[ \"\$(cat /etc/dd-worker/secrets.env)\" = 'ANTHROPIC_API_KEY=$key' ]"
	check "no temp secrets file is left" sh -c '! ls -A /etc/dd-worker | grep -q "^\.secrets"'

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
	check "previous is $V1" sh -c "[ \"\$(cat $LIB/previous)\" = '$V1' ]"
	check "upgrade restarted the service" grep -qx "restart dd-worker" /tmp/systemctl.log
	check "upgrade reloaded systemd" grep -qx "daemon-reload" /tmp/systemctl.log
	check "dd-worker version prints $V2" sh -c "$LINK version | grep -qx '$V2'"
	layout "$V2"

	echo "# rollback"
	check "rollback" inst rollback
	links_to "$V1"
	check "previous is $V2" sh -c "[ \"\$(cat $LIB/previous)\" = '$V2' ]"
	check "both versions are kept" test -x "$LIB/$V2/dd-worker"

	echo "# upgrade that never connects rolls back"
	rm -f /tmp/fake-connects
	if DD_WORKER_CONNECT_TIMEOUT=2 inst upgrade "$V2"; then not_ok "a failed upgrade exits non-zero"; else ok "a failed upgrade exits non-zero"; fi
	check "it says it rolled back" out_has "rolling back to $V1"
	links_to "$V1"

	echo "# an unknown version installs nothing"
	check "install 9.9.9 fails" sh -c "! sh $INSTALL install 9.9.9"
	links_to "$V1"
	check "no 9.9.9 dir" absent $LIB/9.9.9
	check "a bad version string is refused" sh -c "! sh $INSTALL install '1.2.3;id'"

	echo "# remove"
	check "remove" inst remove
	check "remove disabled the unit" grep -qx "disable --now dd-worker" /tmp/systemctl.log
	for p in $UNIT $LINK $LIB; do check "removed $p" absent "$p"; done
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
refuse) test_refuse ;;
*)
	echo "usage: in_container.sh main|refuse" >&2
	exit 2
	;;
esac

if [ "$failures" -eq 0 ]; then
	echo "# $1: all passed"
else
	echo "# $1: $failures failed"
	exit 1
fi

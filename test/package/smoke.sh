#!/usr/bin/env bash
# Debian package install/upgrade/remove/purge smoke test for the PR-time
# Package workflow (issue #162).
#
# The .deb and its maintainer scripts previously had zero test coverage:
# no PR workflow built the package and nothing installed it, so a broken
# nfpm.yaml or a broken maintainer script would only surface at release
# time. This script is the executable specification that closes that gap.
# It runs as root inside a plain ubuntu container (no running systemd) and
# drives the full lifecycle against two distinct package versions:
#
#   $1  the lower-version .deb  (installed first)
#   $2  the higher-version .deb (installed over the first — an upgrade)
#
# It asserts the maintainer-script exit codes, the unit file's presence
# and single-sourced ExecStart / Wants= shape, the binary version swap on
# upgrade (proving dpkg -i actually replaced the running binary), conffile
# preservation across the upgrade, and a clean remove + purge.
#
# The systemctl calls in postinstall.sh / postremove.sh are guarded by
# [ -d /run/systemd/system ], which is absent in this container, so the
# lifecycle steps exercise their non-systemd path here. The active-restart
# path is covered by the drain-hitless E2E scenario (test/e2e/scenarios); a
# final section below fakes systemd present with a stubbed systemctl to
# assert the OVN_NETWORK_RESTART_ON_UPGRADE opt-in parses robustly.
set -euo pipefail

LOWER_DEB=${1:?usage: smoke.sh <lower-version.deb> <higher-version.deb>}
HIGHER_DEB=${2:?usage: smoke.sh <lower-version.deb> <higher-version.deb>}

PKG=ovn-network-agent
UNIT=/lib/systemd/system/ovn-network-agent.service
BIN=/usr/bin/ovn-network-agent
DEFAULTS=/etc/default/ovn-network-agent
SAMPLE=/etc/ovn-network-agent/config.yaml.sample

fail() { echo "FAIL: $*" >&2; exit 1; }

assert_contains() { # haystack needle description
	case "$1" in
	*"$2"*) : ;;
	*) fail "$3: expected to contain '$2', got: $1" ;;
	esac
}

assert_absent() { # haystack needle description
	case "$1" in
	*"$2"*) fail "$3: expected NOT to contain '$2'" ;;
	*) : ;;
	esac
}

LOWER_VERSION=$(dpkg-deb -f "$LOWER_DEB" Version)
HIGHER_VERSION=$(dpkg-deb -f "$HIGHER_DEB" Version)
[ "$LOWER_VERSION" != "$HIGHER_VERSION" ] ||
	fail "the two packages must have distinct versions (got '$LOWER_VERSION' twice)"

echo "== install $LOWER_VERSION =="
dpkg -i "$LOWER_DEB"
[ "$(dpkg-query -W -f='${Status}' "$PKG")" = "install ok installed" ] ||
	fail "package not 'install ok installed' after install"
test -x "$BIN" || fail "binary $BIN missing or not executable"
assert_contains "$("$BIN" --version)" "$LOWER_VERSION" "installed binary --version"
test -f "$UNIT" || fail "unit file $UNIT missing"
# Strip comment lines: the unit carries an explanatory block that mentions
# Requires= and Wants= in prose, so match effective directives only — else
# these assertions grep free text and pass (or fail) on comment edits.
unit=$(grep -v '^[[:space:]]*#' "$UNIT")
assert_contains "$unit" "ExecStart=/usr/bin/ovn-network-agent" "unit ExecStart"
assert_contains "$unit" "Wants=frr.service" "unit Wants=frr.service"
assert_absent "$unit" "Requires=frr.service" "unit must not hard-require frr"

# The unit-file Wants= check and the package-metadata check are two different
# contracts. dpkg -i ignores Depends/Recommends, so a revert to a hard
# 'depends: frr' in nfpm.yaml would still install cleanly and pass every
# assertion above — assert the control metadata directly to catch that.
assert_contains "$(dpkg-deb -f "$LOWER_DEB" Recommends)" "frr" "package must Recommend frr"
assert_absent "$(dpkg-deb -f "$LOWER_DEB" Depends)" "frr" "package must not hard-Depend on frr"
test -f "$DEFAULTS" || fail "conffile $DEFAULTS missing"
test -f "$SAMPLE" || fail "sample config $SAMPLE missing"

echo "== mark the conffile =="
MARKER="# smoke-test marker $$"
echo "$MARKER" >>"$DEFAULTS"

echo "== upgrade $LOWER_VERSION -> $HIGHER_VERSION =="
# Exercises the old prerm ('upgrade') then the new postinst ('configure').
dpkg -i "$HIGHER_DEB"
[ "$(dpkg-query -W -f='${Version}' "$PKG")" = "$HIGHER_VERSION" ] ||
	fail "dpkg version is not $HIGHER_VERSION after upgrade"
assert_contains "$("$BIN" --version)" "$HIGHER_VERSION" "upgraded binary --version"
grep -qF "$MARKER" "$DEFAULTS" ||
	fail "conffile marker lost across upgrade — $DEFAULTS is not preserved"
test -f "$UNIT" || fail "unit file $UNIT missing after upgrade"

echo "== remove =="
dpkg -r "$PKG"
[ ! -e "$BIN" ] || fail "binary $BIN still present after remove"
[ ! -e "$UNIT" ] || fail "unit file $UNIT still present after remove"
test -f "$DEFAULTS" || fail "conffile $DEFAULTS should survive remove (dropped only on purge)"
[ "$(dpkg-query -W -f='${Status}' "$PKG")" = "deinstall ok config-files" ] ||
	fail "package not 'deinstall ok config-files' after remove"

echo "== purge =="
dpkg --purge "$PKG"
[ ! -e "$DEFAULTS" ] || fail "conffile $DEFAULTS still present after purge"
if dpkg-query -W "$PKG" >/dev/null 2>&1; then
	fail "package still known to dpkg after purge"
fi

echo "== restart-on-upgrade opt-in parse =="
# The OVN_NETWORK_RESTART_ON_UPGRADE opt-in is parsed out of the
# EnvironmentFile by postinstall.sh. Operators hand-edit or template that
# file, so a trailing space or a CRLF line ending on the assignment must
# still count as the accepted value — otherwise the opt-in silently no-ops
# and the fleet keeps running the stale binary. The active-restart path is
# systemd-guarded, so drive the real postinstall.sh with systemd faked
# present and systemctl stubbed, and assert which values trigger a restart.
STUB=$(mktemp -d)
RESTART_LOG="$STUB/restart.log"
cat >"$STUB/systemctl" <<EOF
#!/bin/sh
# Report the unit active so the opt-in branch is reached, record the units
# passed to try-restart, and succeed for everything else (daemon-reload).
case "\$1" in
is-active) exit 0 ;;
try-restart) echo "\$2" >>"$RESTART_LOG" ;;
esac
exit 0
EOF
chmod +x "$STUB/systemctl"
mkdir -p /run/systemd/system # make [ -d /run/systemd/system ] pass

# %b turns the \r into a real carriage return, giving a genuine CRLF line.
for value in 'true' 'true ' 'true\r' '"true"' '1' 'yes'; do
	printf 'OVN_NETWORK_RESTART_ON_UPGRADE=%b\n' "$value" >"$DEFAULTS"
	: >"$RESTART_LOG"
	PATH="$STUB:$PATH" sh packaging/postinstall.sh configure
	grep -q ovn-network-agent.service "$RESTART_LOG" ||
		fail "opt-in value [$value] did not trigger a restart"
done

# A value that is not an accepted truthy token must NOT restart.
printf 'OVN_NETWORK_RESTART_ON_UPGRADE=false\n' >"$DEFAULTS"
: >"$RESTART_LOG"
PATH="$STUB:$PATH" sh packaging/postinstall.sh configure
[ -s "$RESTART_LOG" ] && fail "value [false] unexpectedly triggered a restart"
rm -rf "$STUB"
rmdir /run/systemd/system 2>/dev/null || true

echo "PASS"

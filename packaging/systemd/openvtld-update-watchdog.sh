#!/usr/bin/env bash
#
# OpenVTL update watchdog.
#
# Runs as openvtld.service ExecStartPre — BEFORE every start attempt, including
# Restart= retries after a crash. It is the backstop for the worst case: an
# update whose new binary is DOA or whose migration fails, with no `openvtld
# update` CLI still around to roll back (operator disconnected, or a reboot
# happened mid-update).
#
# Contract:
#   * No pending update  -> no-op (a normal boot).
#   * Pending update, new binary starts healthy -> the daemon clears the marker
#     itself; this stays a no-op after that.
#   * Pending update, new binary crash-loops -> after OVTL_ATTEMPT_LIMIT start
#     attempts, restore the previous binary + DB snapshot, clear the marker, and
#     exit 0 so systemd's ExecStart then launches the reverted (good) binary.
#
# It parses only the sourceable env twin (update-pending.env), never JSON, so it
# works even when both binaries are suspect. Wired with `ExecStartPre=-` so a bug
# here can never permanently block startup.
set -u

STATE=/var/lib/openvtld
MARKER="$STATE/update-pending.json"
ENVF="$STATE/update-pending.env"
ATT="$STATE/update-attempts"

# No pending update -> normal start.
[ -f "$MARKER" ] || exit 0
# The env twin is what we act on; without it, don't guess — let openvtld start.
[ -f "$ENVF" ] || exit 0

# shellcheck disable=SC1090
. "$ENVF" 2>/dev/null || exit 0
if [ -z "${OVTL_BINARY:-}" ] || [ -z "${OVTL_PREV_BINARY:-}" ] || [ -z "${OVTL_DB:-}" ]; then
  exit 0
fi
limit="${OVTL_ATTEMPT_LIMIT:-3}"

n=$(cat "$ATT" 2>/dev/null || echo 0)
case "$n" in ''|*[!0-9]*) n=0 ;; esac
n=$((n + 1))
echo "$n" > "$ATT"

if [ "$n" -le "$limit" ]; then
  logger -t openvtld-watchdog "pending update: start attempt $n/$limit" 2>/dev/null || true
  exit 0
fi

# Budget exhausted — the new binary is DOA. Revert the pair (binary + DB) and let
# systemd start the previous, known-good binary.
logger -t openvtld-watchdog "update failed ${n} start attempts — auto-rolling-back to the previous binary" 2>/dev/null || true
if [ -x "$OVTL_PREV_BINARY" ]; then
  cp -f "$OVTL_PREV_BINARY" "$OVTL_BINARY" 2>/dev/null || true
fi
if [ -n "${OVTL_DB_SNAPSHOT:-}" ] && [ -f "${OVTL_DB_SNAPSHOT:-}" ]; then
  cp -f "$OVTL_DB_SNAPSHOT" "$OVTL_DB" 2>/dev/null || true
  # Drop the WAL/SHM so SQLite can't replay a newer WAL over the restored file.
  rm -f "${OVTL_DB}-wal" "${OVTL_DB}-shm" 2>/dev/null || true
fi
rm -f "$MARKER" "$ENVF" "$ATT" 2>/dev/null || true
logger -t openvtld-watchdog "rollback complete — starting the previous binary" 2>/dev/null || true
exit 0

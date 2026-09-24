#!/usr/bin/env bash
# vswitch-tap-debug-test.sh — regression suite for vswitch-tap-debug.sh.
#
# The suite is self-contained by default: it boots a private throwaway switch
# (8 TAP ports, dedicated mgmt netns with the VIP), runs every case against it,
# and tears everything down (vswitch stop + netns delete, bpffs pins included).
# It never touches production switch pools.
#
# Usage:
#   CONNECTOR_CTL=/path/to/connector-ctl ./vswitch-tap-debug-test.sh            # self-contained switch
#   CONNECTOR_CTL=/path/to/connector-ctl ./vswitch-tap-debug-test.sh --use SW   # run against an existing switch
#
# Environment:
#   CONNECTOR_CTL     connector-ctl path (required unless on PATH; for --use it is
#                     also read from /run/sandbox-<NS>/deploy.state)
#   TEST_VIP          management VIP to probe (default 169.254.169.254)
#   TEST_FIP_BASE     self-contained switch floating-ip base (default: first free
#                     100.100.<208|224|240>.0 /20)
#   TEST_PORTS        self-contained switch port count (default 8)
#   KEEP_TEST_SWITCH  set to keep the private switch after the run (debugging)
#   SKIP_PROBES=1     skip external-path probes (--use mode only)
#   TEST_STATE_ROOT   fresh, test-owned session-state directory (optional)
set -uo pipefail

DT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
DT="$DT_DIR/vswitch-tap-debug.sh"
VIP=${TEST_VIP:-169.254.169.254}

MODE=self
USE_SWITCH=""
if [[ "${1:-}" == "--use" ]]; then
    MODE=use
    [[ -n "${2:-}" ]] || { echo "usage: $0 [--use <switch>]" >&2; exit 2; }
    USE_SWITCH=$2
fi

CTL=${CONNECTOR_CTL:-}
if [[ -z "$CTL" && "$MODE" == use ]]; then
    for st in "/run/sandbox-${USE_SWITCH#e2b}/deploy.state" "/run/sandbox-$USE_SWITCH/deploy.state"; do
        [[ -f "$st" ]] && { CTL=$(awk -F= '$1=="connector_path"{print $2}' "$st" 2>/dev/null || true); break; }
    done
fi
[[ -z "$CTL" ]] && CTL=$(command -v connector-ctl || true)
[[ -n "$CTL" && -x "$CTL" ]] || { echo "connector-ctl not found; set CONNECTOR_CTL" >&2; exit 2; }
[[ -x "$DT" ]] || { echo "vswitch-tap-debug.sh not found at $DT" >&2; exit 2; }
[[ $EUID -eq 0 ]] || { echo "run as root" >&2; exit 2; }

# ── private switch lifecycle ────────────────────────────────────────────────
TSW="dt${BASHPID}"
TSW_NS="${TSW}_ns"
TSW_MGMT="${TSW}_mgmt"
TSW_MGMT_DEV="${TSW}m0"
TSW_MAC=02:db:de:00:00:01
TEST_PORTS=${TEST_PORTS:-8}
FIP_BASE=""
SWITCH=""

fip_base_free() { # no root route / no netns route overlaps this /20
    local third=$1 line
    ip -4 route show table all 2>/dev/null | grep -qE "^100\.100\.(${third}|$((third+1))|$((third+8)))\." && return 1
    for ns in $(ip netns list | awk '{print $1}'); do
        line=$(ip -n "$ns" route show 2>/dev/null | grep -oE "^100\.100\.[0-9]+" | cut -d. -f3 | sort -u)
        for t in $line; do (( t / 16 == third / 16 )) && return 1; done
    done
    return 0
}

switch_up() {
    local cand
    for cand in ${TEST_FIP_BASE:-} 208 224 240 96 112; do
        [[ -z "$cand" ]] && continue
        if fip_base_free "$cand"; then FIP_BASE="100.100.$cand.0"; break; fi
    done
    [[ -n "$FIP_BASE" ]] || { echo "no free floating-ip /20 found" >&2; exit 2; }
    ip netns add "$TSW_NS"
    ip netns add "$TSW_MGMT"
    ip -n "$TSW_NS" link set lo up
    ip -n "$TSW_MGMT" link set lo up
    "$CTL" vswitch start "$TSW" --netns="$TSW_NS" --ports="$TEST_PORTS" --mode=tap \
        --mac-addr="$TSW_MAC" --floating-ip-base="$FIP_BASE" \
        --mgmt-extract="$TSW_MGMT:$TSW_MGMT_DEV:$VIP/32" >/dev/null || return 1
    # Same post-start wiring deploy-ns.sh applies on a managed switch: VIP on the
    # mgmt peer plus the FIP return route, so VIP probes answer from the mgmt netns.
    ip -n "$TSW_MGMT" addr replace "$VIP/32" dev "$TSW_MGMT_DEV"
    ip -n "$TSW_MGMT" route replace "${FIP_BASE}/20" dev "$TSW_MGMT_DEV" metric 100
    return 0
}

switch_down() {
    "$CTL" vswitch stop "$TSW" --force >/dev/null 2>&1
    ip netns del "$TSW_MGMT" 2>/dev/null
    ip netns del "$TSW_NS" 2>/dev/null
    if [[ -e "/sys/fs/bpf/$TSW" ]]; then
        echo "WARN: bpffs pins left behind for $TSW" >&2
        return 1
    fi
    return 0
}

# ── helpers ─────────────────────────────────────────────────────────────────
PASS=0; FAIL=0; SKIP=0
declare -a FAILED_LIST=()
result() { # result <pass|fail|skip> <name> [detail]
    case $1 in
        pass) PASS=$((PASS+1)); printf '  PASS %s %s\n' "$2" "${3:+— $3}" ;;
        fail) FAIL=$((FAIL+1)); FAILED_LIST+=("$2"); printf '  FAIL %s %s\n' "$2" "${3:+— $3}" ;;
        skip) SKIP=$((SKIP+1)); printf '  SKIP %s %s\n' "$2" "${3:+— $3}" ;;
    esac
}
run_dt() { CONNECTOR_CTL="$CTL" "$DT" "$@"; }
# NOTE: kill-scenario cases must launch "$DT" directly (not through run_dt) so
# that $! is the script process itself; a wrapper subshell would survive kill -9.
port_state() { "$CTL" vswitch show slots "$SWITCH" "$(($1 - 1))" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["state"])' 2>/dev/null; }
total_ports() { "$CTL" vswitch status "$SWITCH" | python3 -c 'import json,sys; print(json.load(sys.stdin)["ports"])' 2>/dev/null; }
switch_ns() { "$CTL" vswitch status "$SWITCH" | python3 -c 'import json,sys; print(json.load(sys.stdin)["switch_netns"])' 2>/dev/null; }
sessions_left() { ls "$STATE_ROOT" 2>/dev/null | grep -c '^session\.' || true; }
# Keep this suite's session records isolated from a user's interactive debug session.
TEST_TMP_DIR=$(mktemp -d /tmp/vswitch-tap-debug-test.XXXXXXXX)
STATE_ROOT=${TEST_STATE_ROOT:-$TEST_TMP_DIR/sessions}
if [[ -e "$STATE_ROOT" ]]; then
    echo "test state root already exists; choose a fresh TEST_STATE_ROOT: $STATE_ROOT" >&2
    exit 2
fi
mkdir -m 700 -p "$STATE_ROOT"
export DEBUG_STATE_ROOT="$STATE_ROOT"
BASE_DEBUG_NS=$(ip netns list | awk '/sandbox_debug_ns/{print $1}' | sort)
socat_count() {
    local found
    found=$(pgrep -a -x socat 2>/dev/null | grep -F "tun-name=${SWITCH}-t" || true)
    [[ -z "$found" ]] && echo 0 || wc -l <<<"$found"
}
dbg_ns_count() {
    local ns count=0
    while read -r ns; do
        [[ -n "$ns" ]] || continue
        grep -Fxq "$ns" <<<"$BASE_DEBUG_NS" || count=$((count + 1))
    done < <(ip netns list | awk '/sandbox_debug_ns/{print $1}')
    echo "$count"
}
# ethtool netlink has transient failure windows lasting tens of seconds; retry reads.
ethtool_k() {
    local i
    for i in 1 2 3 4; do ip netns exec "$1" ethtool -k "$2" >"$3" 2>/dev/null && return 0; sleep .5; done
    return 1
}
feat_dump() { awk '/^[[:space:]]*[^[:space:]]+:[[:space:]]+(on|off)/ {gsub(":", "", $1); print $1, $2}' "$1"; }

# ── boot ────────────────────────────────────────────────────────────────────
if [[ "$MODE" == self ]]; then
    echo "== booting private switch $TSW (ports=$TEST_PORTS fip=$FIP_BASE-pending) =="
    if switch_up && "$CTL" vswitch status "$TSW" --ready >/dev/null 2>&1; then
        result pass "private switch boots and reaches Ready" "fip=$FIP_BASE"
    else
        result fail "private switch boots and reaches Ready"
        switch_down >/dev/null 2>&1
        exit 1
    fi
    SWITCH=$TSW
else
    SWITCH=$USE_SWITCH
    "$CTL" vswitch status "$SWITCH" >/dev/null 2>&1 || { echo "switch unavailable: $SWITCH" >&2; exit 2; }
fi
TOTAL=$(total_ports)
PORT_A=${TEST_PORT_A:-$TOTAL}
PORT_B=${TEST_PORT_B:-$((TOTAL > 1 ? TOTAL - 1 : 1))}
cleanup_env() {
    # Clean only sessions recorded in this suite's private state directory.
    run_dt --cleanup-stale >/dev/null 2>&1 || true
    [[ "$(sessions_left)" == 0 && "$(socat_count)" == 0 && "$(dbg_ns_count)" == 0 ]]
}
trap 'rc=$?; cleanup_env || { echo "test-owned resources remain under $STATE_ROOT" >&2; rc=1; }; if [[ "$MODE" == self && -z "${KEEP_TEST_SWITCH:-}" ]]; then switch_down >/dev/null 2>&1 || { echo "private switch teardown failed: $SWITCH" >&2; rc=1; }; fi; if [[ $rc -eq 0 ]]; then rmdir "$STATE_ROOT" 2>/dev/null || true; rmdir "$TEST_TMP_DIR" 2>/dev/null || true; else echo "suite aborted rc=$rc; inspect test state at $STATE_ROOT" >&2; fi; exit $rc' EXIT

# ════════════════════════════════════════════════════════════════════════════
echo "== 1. entry points and argument validation =="
run_dt -h >/dev/null 2>&1 && result pass "-h exits 0" || result fail "-h exits 0"
run_dt >/dev/null 2>&1; [[ $? -eq 2 ]] && result pass "no args prints usage, exits 2" || result fail "no args prints usage, exits 2"
run_dt nosuch-switch-xyz true >/dev/null 2>&1; [[ $? -eq 1 ]] && result pass "unknown switch fails cleanly" || result fail "unknown switch fails cleanly"
CONNECTOR_CTL=/nonexistent-ctl "$DT" "$SWITCH" true >/dev/null 2>&1; [[ $? -eq 1 ]] && result pass "bad CONNECTOR_CTL fails cleanly" || result fail "bad CONNECTOR_CTL fails cleanly"

# ════════════════════════════════════════════════════════════════════════════
echo "== 2. auto mode: tail-port selection, probes, env fidelity =="
LOG=$(mktemp); PORT_SEL=""
http_proxy=http://1.2.3.4:1 https_proxy=http://1.2.3.4:1 run_dt "$SWITCH" sh -c '
    echo "proxy_count=$(env | grep -ci proxy)"
    echo "mtu=$(cat /sys/class/net/dbg0/mtu)"
    ping -c1 -W2 '"$VIP"' >/dev/null && echo ping=ok || echo ping=fail
' >"$LOG" 2>&1
PORT_SEL=$(grep -oE "auto-selected tail port=[0-9]+" "$LOG" | grep -oE "[0-9]+$")
if [[ -n "$PORT_SEL" && -n "$TOTAL" && "$PORT_SEL" -gt $((TOTAL / 2)) ]]; then
    result pass "auto mode picks a tail port" "port=$PORT_SEL/$TOTAL"
else
    result fail "auto mode picks a tail port" "port=${PORT_SEL:-none}/total=${TOTAL:-?}"
fi
grep -q "ping=ok" "$LOG" && result pass "ICMP to mgmt VIP" || result fail "ICMP to mgmt VIP"
[[ "$(grep -oE 'proxy_count=[0-9]+' "$LOG" | cut -d= -f2)" == "0" ]] \
    && result pass "host proxy vars stripped inside ns" || result fail "host proxy vars stripped inside ns"
TAP_MTU=$(ip netns exec "$(switch_ns)" ip link show "${SWITCH}-t${PORT_SEL:-0}" 2>/dev/null | awk '{for(i=1;i<NF;i++) if($i=="mtu"){print $(i+1); exit}}')
[[ -n "$TAP_MTU" && "$(grep -oE 'mtu=[0-9]+' "$LOG" | cut -d= -f2)" == "$TAP_MTU" ]] \
    && result pass "dbg0 MTU follows pool TAP" "mtu=$TAP_MTU" || result fail "dbg0 MTU follows pool TAP"
KEEP_LOG=$(mktemp)
http_proxy=http://1.2.3.4:1 DEBUG_KEEP_PROXY=1 run_dt "$SWITCH" sh -c 'echo proxy_count=$(env | grep -ci proxy)' >"$KEEP_LOG" 2>&1
[[ "$(grep -oE 'proxy_count=[0-9]+' "$KEEP_LOG" | cut -d= -f2)" -ge 1 ]] \
    && result pass "DEBUG_KEEP_PROXY=1 keeps proxy vars" || result fail "DEBUG_KEEP_PROXY=1 keeps proxy vars"
[[ -n "$PORT_SEL" && "$(port_state "$PORT_SEL")" == "free" ]] \
    && result pass "port returned after session" "port=${PORT_SEL:-?}" || result fail "port returned after session"

# ════════════════════════════════════════════════════════════════════════════
echo "== 3. explicit --port =="
if [[ "$(port_state "$PORT_A")" == "free" ]]; then
    if run_dt --port "$PORT_A" "$SWITCH" ping -c1 -W2 "$VIP" >"$LOG" 2>&1 \
       && grep -q "allocated port=$PORT_A " "$LOG" && grep -qE '(^|[[:space:]])0% packet loss' "$LOG"; then
        result pass "explicit --port session + probe" "port=$PORT_A"
    else
        result fail "explicit --port session + probe"
    fi
    [[ "$(port_state "$PORT_A")" == "free" ]] && result pass "explicit port returned" || result fail "explicit port returned"
else
    result skip "explicit --port case" "port $PORT_A not free"
fi

# ════════════════════════════════════════════════════════════════════════════
echo "== 4. external-path probes (--use mode only; need DNAT/NAT infra) =="
if [[ "$MODE" == use && -z "${SKIP_PROBES:-}" ]]; then
    run_dt "$SWITCH" getent hosts obs.cn-north-4.myhuaweicloud.com >/dev/null 2>&1 \
        && result pass "DNS via guest path (VIP resolver)" || result fail "DNS via guest path"
    run_dt "$SWITCH" curl -sS -m8 -o /dev/null -w '%{http_code}' https://obs.cn-north-4.myhuaweicloud.com/ 2>/dev/null | grep -qE '^[1-5][0-9][0-9]$' \
        && result pass "external HTTPS (full chain)" || result fail "external HTTPS (full chain)"
else
    result skip "external-path probes" "self-contained switch has no NAT uplink"
fi

# ════════════════════════════════════════════════════════════════════════════
echo "== 5. SIGKILL recovery =="
CONNECTOR_CTL="$CTL" "$DT" "$SWITCH" sleep 30 >"$TEST_TMP_DIR/dt-test-stale.log" 2>&1 &
KP=$!; sleep 3; kill -9 "$KP" 2>/dev/null; disown "$KP" 2>/dev/null; sleep 1
STALE_PORT=$(grep -oE "allocated port=[0-9]+" "$TEST_TMP_DIR/dt-test-stale.log" | grep -oE "[0-9]+$")
run_dt --list 2>&1 | grep -qE "status=stale" && result pass "--list reports stale session" || result fail "--list reports stale session"
run_dt --cleanup-stale >/dev/null 2>&1
CLEAN=1
[[ "$(port_state "${STALE_PORT:-0}")" == "free" ]] || CLEAN=0
[[ "$(socat_count)" == "0" ]] || CLEAN=0
[[ "$(dbg_ns_count)" == "0" ]] || CLEAN=0
[[ "$(sessions_left)" == "0" ]] || CLEAN=0
[[ "$CLEAN" == "1" ]] && result pass "cleanup-stale fully recovers" "port=$STALE_PORT freed" \
                     || result fail "cleanup-stale fully recovers" "port=${STALE_PORT:-?}=$(port_state "${STALE_PORT:-0}") socat=$(socat_count) ns=$(dbg_ns_count) dirs=$(sessions_left)"

# ════════════════════════════════════════════════════════════════════════════
echo "== 6. ownership change (no mis-detach of a reused port) =="
CONNECTOR_CTL="$CTL" "$DT" "$SWITCH" sleep 30 >"$TEST_TMP_DIR/dt-test-own.log" 2>&1 &
KP=$!; sleep 3; kill -9 "$KP" 2>/dev/null; disown "$KP" 2>/dev/null; sleep 1
OWN_PORT=$(grep -oE "allocated port=[0-9]+" "$TEST_TMP_DIR/dt-test-own.log" | grep -oE "[0-9]+$")
"$CTL" vswitch detach "$SWITCH" --port="$OWN_PORT" --skip-device >/dev/null           # simulate release
"$CTL" vswitch attach "$SWITCH" --port="$OWN_PORT" --inner-ip=169.254.9.9 >/dev/null  # simulate foreign re-allocation
OUT=$(run_dt --cleanup-stale 2>&1); RC=$?
OK=1
[[ "$RC" -eq 1 ]] || OK=0
echo "$OUT" | grep -q "changed ownership; own resources cleaned" || OK=0
[[ "$(port_state "$OWN_PORT")" == "allocated" ]] || OK=0     # foreign owner untouched
[[ "$(socat_count)" == "0" && "$(dbg_ns_count)" == "0" ]] || OK=0
[[ "$(sessions_left)" -ge 1 ]] || OK=0                        # session dir retained for forensics
[[ "$OK" == "1" ]] && result pass "ownership change: port untouched, own resources freed" \
                 || result fail "ownership change: port untouched, own resources freed" "rc=$RC first=$(echo "$OUT" | head -1)"
"$CTL" vswitch detach "$SWITCH" --port="$OWN_PORT" --skip-device >/dev/null 2>&1
rm -rf "$STATE_ROOT"/session.* 2>/dev/null

# ════════════════════════════════════════════════════════════════════════════
echo "== 7. concurrent agents =="
LOGA=$(mktemp); LOGB=$(mktemp)
CONNECTOR_CTL="$CTL" "$DT" "$SWITCH" sh -c 'sleep 4; ping -c1 -W2 '"$VIP"' >/dev/null && echo ok' >"$LOGA" 2>&1 &
A=$!
CONNECTOR_CTL="$CTL" "$DT" "$SWITCH" sh -c 'sleep 4; ping -c1 -W2 '"$VIP"' >/dev/null && echo ok' >"$LOGB" 2>&1 &
B=$!
wait "$A" "$B"
PA=$(grep -oE "auto-selected tail port=[0-9]+" "$LOGA" | grep -oE "[0-9]+$")
PB=$(grep -oE "auto-selected tail port=[0-9]+" "$LOGB" | grep -oE "[0-9]+$")
if [[ -n "$PA" && -n "$PB" && "$PA" != "$PB" ]] && grep -q '^ok$' "$LOGA" && grep -q '^ok$' "$LOGB"; then
    result pass "two concurrent agents get distinct tail ports" "A=$PA B=$PB"
else
    result fail "two concurrent agents get distinct tail ports" "A=${PA:-?}($(grep -c '^ok$' "$LOGA")) B=${PB:-?}($(grep -c '^ok$' "$LOGB"))"
fi
[[ "$(port_state "${PA:-0}")" == "free" && "$(port_state "${PB:-0}")" == "free" ]] \
    && result pass "concurrent sessions return ports" || result fail "concurrent sessions return ports"

CONNECTOR_CTL="$CTL" "$DT" "$SWITCH" sleep 30 >"$TEST_TMP_DIR/dt-test-race.log" 2>&1 &
KP=$!; sleep 3; kill -9 "$KP" 2>/dev/null; disown "$KP" 2>/dev/null; sleep 1
RACE_PORT=$(grep -oE "allocated port=[0-9]+" "$TEST_TMP_DIR/dt-test-race.log" | grep -oE "[0-9]+$")
run_dt --cleanup-stale >/dev/null 2>&1 & C1=$!
run_dt --cleanup-stale >/dev/null 2>&1 & C2=$!
wait "$C1"; C1_RC=$?
wait "$C2"; C2_RC=$?
[[ "$C1_RC" -eq 0 && "$C2_RC" -eq 0 && "$(port_state "${RACE_PORT:-0}")" == "free" && "$(sessions_left)" == "0" && "$(socat_count)" == "0" ]] \
    && result pass "racing cleanup-stale runs reach a clean end state" "port=$RACE_PORT freed" \
    || result fail "racing cleanup-stale runs reach a clean end state" "state=$(port_state "${RACE_PORT:-0}") dirs=$(sessions_left) socat=$(socat_count)"

# ════════════════════════════════════════════════════════════════════════════
echo "== 8. offload cleanliness (actual values before vs after) =="
if [[ "$(port_state "$PORT_B")" == "free" ]]; then
    NS=$(switch_ns); TAPB="${SWITCH}-t${PORT_B}"
    F1=$(mktemp); F2=$(mktemp)
    if ethtool_k "$NS" "$TAPB" "$F1"; then
        if run_dt --port "$PORT_B" "$SWITCH" ping -c1 -W2 "$VIP" >/dev/null 2>&1 \
           && ethtool_k "$NS" "$TAPB" "$F2" && diff <(feat_dump "$F1") <(feat_dump "$F2") >/dev/null; then
            result pass "offload actual values identical before/after" "port=$PORT_B"
        else
            result fail "offload actual values identical before/after" "port=$PORT_B"
        fi
    else
        result skip "offload cleanliness" "ethtool transient failure"
    fi
    rm -f "$F1" "$F2"
else
    result skip "offload cleanliness" "port $PORT_B not free"
fi

# ════════════════════════════════════════════════════════════════════════════
rm -f "$LOG" "$KEEP_LOG" "$LOGA" "$LOGB" "$TEST_TMP_DIR"/dt-test-*.log
echo
echo "======== RESULT: PASS=$PASS FAIL=$FAIL SKIP=$SKIP ========"
if [[ "$FAIL" -gt 0 ]]; then
    printf 'failed: %s\n' "${FAILED_LIST[*]}"
    exit 1
fi
exit 0

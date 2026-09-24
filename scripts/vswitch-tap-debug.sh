#!/usr/bin/env bash
# Open a disposable debug netns on an allocated connector TAP.
set -Eeuo pipefail
export LC_ALL=C

CONNECTOR_CTL=${CONNECTOR_CTL:-${CONNECTOR_CTL_PATH:-/opt/sandbox/bin/connector-ctl}}
DEBUG_CIDR=${DEBUG_CIDR:-169.254.0.21/30}
DEBUG_GATEWAY=${DEBUG_GATEWAY:-169.254.0.22}
DEBUG_DNS=${DEBUG_DNS:-169.254.169.253}
DEBUG_PORT=${DEBUG_PORT:-}
TRANSIT_GATEWAY_IP=${TRANSIT_GATEWAY_IP:-}
TRANSIT_GENEVE_VNI=${TRANSIT_GENEVE_VNI:-}
TRANSIT_MAC_ADDR=${TRANSIT_MAC_ADDR:-}
STATE_ROOT=${DEBUG_STATE_ROOT:-/run/connector-debug-tap}
DEBUG_CTL_TIMEOUT=${DEBUG_CTL_TIMEOUT:-30}

die() { echo "vswitch-tap-debug: $*" >&2; exit 1; }
usage() { echo "Usage: $0 [--port N] <switch> [command [args...]]" >&2; echo "       $0 --list | --cleanup-stale" >&2; }
help() {
    cat <<'EOF'
Usage: vswitch-tap-debug.sh [--port N] <switch> [command [args...]]
       vswitch-tap-debug.sh --list
       vswitch-tap-debug.sh --cleanup-stale

Open a temporary network namespace connected to an allocated connector TAP.
Without a command, enter an interactive Bash shell. Type "exit" to stop socat,
restore TAP offloads, remove the namespace, and release the port.

Options:
  --port N          Allocate exactly port N; fail if it is unavailable.
                    Without --port the highest free port is chosen, leaving the
                    front of the pool to the sequential sandbox allocator.
  --list            Show active and stale debug sessions.
  --cleanup-stale   Clean sessions whose owner process has exited.
  -h, --help        Show this help and exit.

Environment:
  CONNECTOR_CTL      connector-ctl path (default: /opt/sandbox/bin/connector-ctl;
                     falls back to connector-ctl in PATH).
  CONNECTOR_CTL_PATH Alternate path variable when CONNECTOR_CTL is unset.
  DEBUG_PORT         Port number, equivalent to --port N (default: highest free port).
  DEBUG_CIDR         Debug TAP IPv4/CIDR (default: 169.254.0.21/30).
  DEBUG_GATEWAY      Default gateway (default: 169.254.0.22).
  DEBUG_DNS          Nameserver in the namespace (default: 169.254.169.253).
  TRANSIT_GATEWAY_IP Geneve gateway IP, when transit is configured.
  TRANSIT_GENEVE_VNI Geneve VNI for this port.
  TRANSIT_MAC_ADDR   Optional transit next-hop MAC address.
  DEBUG_STATE_ROOT   Session state directory (default: /run/connector-debug-tap;
                     caller-owned and not group/other writable).
  DEBUG_TAIL_TRIES   Auto mode: consider at most this many highest numbered slots
                     within the upper half of the pool (default 8).
  DEBUG_CTL_TIMEOUT  connector-ctl command timeout in seconds (default 30).
  DEBUG_KEEP_PROXY   Set to keep host proxy variables inside the namespace
                     (they are stripped by default, matching the guest env).

Examples:
  vswitch-tap-debug.sh e2br0919
  vswitch-tap-debug.sh --port 4 e2br0919
  vswitch-tap-debug.sh e2br0919 ping -c 3 169.254.169.254
  CONNECTOR_CTL=/path/to/connector-ctl vswitch-tap-debug.sh e2br0919
  TRANSIT_GATEWAY_IP=10.12.0.2 TRANSIT_GENEVE_VNI=100 vswitch-tap-debug.sh sw1

Requires root, connector-ctl, ip, socat, ethtool, flock, and standard shell tools.
Uncertain allocation or cleanup failures retain the port and session record.
Do not manually detach/reassign a debug port before its session is cleaned.
EOF
}
case "${1:-}" in -h|--help) help; exit 0 ;; esac
json_string() { sed -n "s/^[[:space:]]*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\"[[:space:]]*,*[[:space:]]*$/\1/p" | head -n 1; }
json_number() { sed -n "s/^[[:space:]]*\"$1\"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\)[[:space:]]*,*[[:space:]]*$/\1/p" | head -n 1; }
field() { sed -n "s/^$2=//p" "$1/session" | head -n 1; }
ctl() { timeout -k 5s "${DEBUG_CTL_TIMEOUT}s" "$CONNECTOR_CTL" "$@"; }
proc_start() { awk '{sub(/^.*\) /, ""); print $20}' "/proc/$1/stat" 2>/dev/null; }
ns_identity() { stat -Lc '%d:%i' "$1" 2>/dev/null; }
process_running() {
    local status
    status=$(awk '{sub(/^.*\) /, ""); print $20, $1}' "/proc/$1/stat" 2>/dev/null) || return 1
    [[ "$status" == "$2 "* && "$status" != *' Z' && "$status" != *' X' ]]
}
stop_process() { # PID + start time; a zombie no longer owns file descriptors.
    local pid=$1 start=$2 sig i
    [[ -n "$start" ]] || return 1
    for sig in TERM KILL; do
        process_running "$pid" "$start" || return 0
        kill -"$sig" "$pid" 2>/dev/null || true
        for i in {1..20}; do
            process_running "$pid" "$start" || return 0
            sleep .05
        done
    done
    echo "process $pid did not exit; port will not be released" >&2
    return 1
}
session_socats() { # Exact executable and argv matching also covers an unsaved PID.
    local tap=$1 ns=$2 expected_exe=$3 proc exe arg first second
    [[ -n "$expected_exe" ]] || expected_exe=$(readlink -f "$(command -v socat)") || return 1
    for proc in /proc/[0-9]*; do
        [[ -r "$proc/comm" ]] || continue
        [[ "$(<"$proc/comm")" == socat* ]] || continue
        exe=$(readlink "$proc/exe" 2>/dev/null) || continue
        [[ "$exe" == "$expected_exe" || "$exe" == "$expected_exe (deleted)" ]] || continue
        first=0; second=0
        while IFS= read -r -d '' arg; do
            [[ "$arg" != "TUN,tun-type=tap,tun-name=$tap,no-pi" ]] || first=1
            [[ "$arg" != "TUN,tun-type=tap,tun-name=dbg0,no-pi,netns=$ns" ]] || second=1
        done <"$proc/cmdline" 2>/dev/null
        ((first && second)) && echo "${proc##*/}"
    done
    return 0
}
tap_idle() {
    # fdinfo exposes the TAP name even if its holder has since changed netns.
    # Use batched grep to avoid ARG_MAX on nodes with many sandbox processes.
    local holders errors
    errors=$(mktemp) || return 1
    holders=$(LC_ALL=C find /proc/[0-9]*/fdinfo -maxdepth 1 -type f \
        -exec grep -lFx -- $'iff:\t'"$1" {} + 2>"$errors") || true
    if grep -Evq 'No such file or directory|No such process' "$errors"; then
        echo "cannot verify TAP descriptors: $(cat "$errors")" >&2
        rm -f "$errors"; return 2
    fi
    rm -f "$errors"
    if [[ -n "$holders" ]]; then
        echo "TAP $1 still has open descriptors; port left allocated: $holders" >&2
        return 1
    fi
}
owner_alive() {
    local pid start
    pid=$(field "$1" owner_pid); start=$(field "$1" owner_start)
    [[ "$pid" =~ ^[0-9]+$ && -n "$start" ]] && process_running "$pid" "$start"
}
valid_ip() {
    local a b c d octet
    [[ "$1" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]] || return 1
    IFS=. read -r a b c d <<<"$1"
    for octet in "$a" "$b" "$c" "$d"; do (( 10#$octet <= 255 )) || return 1; done
}
features() { awk '/^[[:space:]]*[^[:space:]]+:[[:space:]]+(on|off)/ { gsub(":", "", $1); print $1, $2 }' "$1"; }

ethtool_k() { # <netns> <tap> <outfile>: ethtool netlink has transient failure windows; retry reads.
    local i
    for i in 1 2 3; do
        ip netns exec "$1" ethtool -k "$2" >"$3" 2>/dev/null && return 0
        sleep .3
    done
    return 1
}

restore_offloads() {
    local sw=$1 tap=$2 saved=$3 now feat wanted actual pass
    [[ -s "$saved" && -n "$(features "$saved")" ]] || {
        echo "missing or empty offload snapshot: $saved" >&2; return 1;
    }
    for pass in 1 2 3; do
        now=$(mktemp)
        ethtool_k "$sw" "$tap" "$now" || { rm -f "$now"; return 1; }
        while read -r feat wanted; do
            actual=$(features "$now" | awk -v key="$feat" '$1 == key {print $2}')
            if [[ -n "$actual" && "$actual" != "$wanted" ]]; then
                ip netns exec "$sw" ethtool -K "$tap" "$feat" "$wanted" >/dev/null 2>&1 || true
            fi
        done < <(features "$saved")
        rm -f "$now"
        now=$(mktemp)
        ethtool_k "$sw" "$tap" "$now" || { rm -f "$now"; return 1; }
        if diff -q <(features "$saved") <(features "$now") >/dev/null; then rm -f "$now"; return 0; fi
        rm -f "$now"
        sleep .3
    done
    echo "offload restore incomplete for $sw/$tap" >&2
    return 1
}

cleanup_session() (
    local dir=$1 stale=${2:-0} sw ns tap port ipaddr pid ns_pid proc argv slot state current_ip current_ifindex current_fip expected_fip
    local saved_start own_port=1 restore_ok=1 detach_ok=1 lock_fd pending_port uncertain=0
    local attach_lock processes_ok=1 expected_ns expected_sw pids start idle_rc
    [[ -f "$dir/session" ]] || return 0
    exec {lock_fd}<"$dir/session" || return 1
    flock -w 60 -x "$lock_fd" || { echo "session lock timed out: $dir" >&2; exec {lock_fd}<&-; return 1; }
    [[ -f "$dir/session" ]] || { exec {lock_fd}<&-; return 0; }
    if [[ "$stale" -eq 1 ]] && owner_alive "$dir"; then echo "active, skipped: $dir" >&2; return 0; fi
    if [[ -e "$dir/attach.lock" ]]; then
        exec {attach_lock}<"$dir/attach.lock" || return 1
        flock -n -x "$attach_lock" || {
            echo "attach still running; session retained: $dir" >&2; return 1;
        }
    fi
    sw=$(field "$dir" switch_ns); ns=$(field "$dir" debug_ns)
    tap=$(field "$dir" tap_name); port=$(field "$dir" port)
    ipaddr=$(field "$dir" inner_ip); pid=$(field "$dir" socat_pid)
    state=$(field "$dir" state)
    pending_port=$(field "$dir" pending_port)
    expected_fip=$(field "$dir" floating_ip)
    if [[ "$state" == preparing && -s "$dir/attach.json" ]]; then
        port=$(json_number port <"$dir/attach.json")
        tap=$(json_string port_dev <"$dir/attach.json")
        expected_fip=$(json_string floating_ip <"$dir/attach.json")
        # A successful attach means the port is allocated (veth slots must be
        # released too); do not require mode=tap here.
        if [[ "$port" =~ ^[1-9][0-9]*$ && ( -z "$pending_port" || "$port" == "$pending_port" ) && \
              ( "$(field "$dir" state_format)" != 2 || "$(cat "$dir/attach.rc" 2>/dev/null)" == 0 ) ]]; then
            state=allocated
        else
            uncertain=1
        fi
    fi
    if [[ "$state" == preparing && "$pending_port" =~ ^[1-9][0-9]*$ ]]; then
        if ! slot=$(ctl vswitch show slots "$(field "$dir" switch)" "$((pending_port - 1))"); then
            uncertain=1
        elif [[ "$(json_string state <<<"$slot")" != free ]]; then
            uncertain=1
        fi
        ((uncertain == 0)) || echo "pending allocation $pending_port has unknown ownership; session retained: $dir" >&2
    fi
    if [[ "$state" == allocated && "$port" =~ ^[1-9][0-9]*$ ]]; then
        if ! slot=$(ctl vswitch show slots "$(field "$dir" switch)" "$((port - 1))"); then
            echo "cannot inspect port $port; own resources will be cleaned, session retained: $dir" >&2
            own_port=0; uncertain=1
        else
            current_ip=$(json_string inner_ip <<<"$slot")
            current_ifindex=$(json_number ifindex <<<"$slot")
            current_fip=$(json_string floating_ip <<<"$slot")
            if [[ "$(json_string state <<<"$slot")" != allocated || "$current_ip" != "$ipaddr" || \
              ( -n "$(field "$dir" ifindex)" && "$current_ifindex" != "$(field "$dir" ifindex)" ) || \
              "$current_fip" != "$expected_fip" ]]; then
                # A leftover socat holds the TAP fd and may block the new
                # owner's open-port, even though attach itself is slot-only.
                echo "port $port has changed ownership; own resources cleaned, port left untouched: $dir" >&2
                own_port=0
                : >"$dir/ownership_lost"
            fi
        fi
    fi
    [[ ! -e "$dir/ownership_lost" ]] || own_port=0
    expected_sw=$(field "$dir" switch_ns_id)
    if [[ -n "$expected_sw" && "$(ns_identity "/var/run/netns/$sw")" != "$expected_sw" ]]; then
        echo "switch namespace changed; port left untouched: $dir" >&2
        own_port=0; uncertain=1
    fi
    # Kill this session's socat before a replaced namespace clears ns below.
    if [[ -n "$tap" && -n "$ns" ]]; then
        while read -r ns_pid; do
            [[ "$ns_pid" =~ ^[1-9][0-9]*$ ]] || continue
            saved_start=$(proc_start "$ns_pid") || continue
            stop_process "$ns_pid" "$saved_start" || processes_ok=0
        done < <(session_socats "$tap" "$ns" "$(field "$dir" socat_exe)")
    fi
    expected_ns=$(field "$dir" debug_ns_id)
    if [[ -n "$ns" && -e "/var/run/netns/$ns" && \
          ( ( -n "$expected_ns" && "$(ns_identity "/var/run/netns/$ns")" != "$expected_ns" ) || \
            ( -z "$expected_ns" && "$(field "$dir" state_format)" == 2 ) ) ]]; then
        echo "debug namespace changed; namespace left untouched: $dir" >&2
        ns=""; uncertain=1; processes_ok=0
    fi
    # Both normal exit and stale recovery must stop background commands too.
    if [[ -n "$ns" && -e "/var/run/netns/$ns" ]]; then
        for _ in 1 2 3; do
            pids=$(ip netns pids "$ns") || { processes_ok=0; break; }
            [[ -n "$pids" ]] || break
            while read -r ns_pid; do
                [[ "$ns_pid" =~ ^[1-9][0-9]*$ ]] || continue
                start=$(proc_start "$ns_pid") || continue
                stop_process "$ns_pid" "$start" || processes_ok=0
            done <<<"$pids"
        done
        pids=$(ip netns pids "$ns") || processes_ok=0
        # Zombies may still appear in proc but no longer pin the namespace.
        for ns_pid in $pids; do
            start=$(proc_start "$ns_pid") || continue
            if process_running "$ns_pid" "$start"; then processes_ok=0; fi
        done
    fi
    if [[ -n "$tap" ]]; then
        if tap_idle "$tap"; then :; else
            idle_rc=$?
            if [[ "$processes_ok" -eq 1 && "$idle_rc" -eq 1 ]]; then : >"$dir/ownership_lost"; fi
            processes_ok=0
        fi
    fi
    # No shared-device mutation while an unknown holder may still use the TAP.
    if [[ "$own_port" -eq 1 && "$processes_ok" -eq 1 && -n "$tap" ]]; then
        if [[ -s "$dir/offloads" || "$(field "$dir" offloads_changed)" == 1 ]]; then
            restore_offloads "$sw" "$tap" "$dir/offloads" || restore_ok=0
        fi
    fi
    if [[ "$processes_ok" -eq 1 && -n "$ns" && -e "/var/run/netns/$ns" ]]; then
        for _ in 1 2 3 4 5; do
            ip netns del "$ns" >/dev/null 2>&1 && break
            sleep .1
        done
        [[ ! -e "/var/run/netns/$ns" ]] || { echo "failed to delete netns $ns" >&2; return 1; }
    fi
    if [[ -n "$ns" && ! -e "/var/run/netns/$ns" && -f "/etc/netns/$ns/resolv.conf" ]]; then
        rm -f -- "/etc/netns/$ns/resolv.conf"
        rmdir -- "/etc/netns/$ns" 2>/dev/null || true
    fi
    if [[ "$restore_ok" -eq 0 || "$processes_ok" -eq 0 || "$uncertain" -eq 1 ]]; then
        echo "cleanup incomplete; port and recovery record retained: $dir" >&2
        return 1
    fi
    if [[ "$state" == allocated && -z "$tap" ]]; then
        echo "allocated port has no recorded TAP; session retained: $dir" >&2
        return 1
    fi
    if [[ "$own_port" -eq 1 && "$state" == allocated && "$port" =~ ^[1-9][0-9]*$ && -n "$tap" ]]; then
        slot=$(ctl vswitch show slots "$(field "$dir" switch)" "$((port - 1))") || return 1
        state=$(json_string state <<<"$slot"); current_ip=$(json_string inner_ip <<<"$slot")
        current_ifindex=$(json_number ifindex <<<"$slot")
        current_fip=$(json_string floating_ip <<<"$slot")
        if [[ "$state" == allocated && "$current_ip" == "$ipaddr" && \
              ( -z "$(field "$dir" ifindex)" || "$current_ifindex" == "$(field "$dir" ifindex)" ) && \
              "$current_fip" == "$expected_fip" ]]; then
            if tap_idle "$tap"; then
                ctl vswitch detach "$(field "$dir" switch)" --port="$port" --skip-device || detach_ok=0
            else
                idle_rc=$?
                ((idle_rc != 1)) || : >"$dir/ownership_lost"
                detach_ok=0
            fi
        elif [[ "$state" != free ]]; then
            echo "port $port has changed ownership during cleanup; session retained: $dir" >&2
            return 1
        fi
    fi
    if [[ "$own_port" -eq 1 && "$uncertain" -eq 0 && "$restore_ok" -eq 1 && "$detach_ok" -eq 1 ]]; then
        rm -f -- "$dir/session" "$dir/session.tmp" "$dir/offloads" "$dir/offloads.tmp" \
            "$dir/attach.json" "$dir/attach.rc" "$dir/attach.rc.tmp" "$dir/attach.err" "$dir/attach.lock"
        rmdir -- "$dir" 2>/dev/null || true
        return 0
    fi
    return 1
)

[[ $EUID -eq 0 ]] || die "run as root"
check_tools() {
    local tool
    for tool in ip socat ethtool sed awk grep diff mktemp tr flock stat readlink find timeout date; do
        command -v "$tool" >/dev/null || die "missing command: $tool"
    done
    [[ "$DEBUG_CTL_TIMEOUT" =~ ^[1-9][0-9]*$ ]] || die "DEBUG_CTL_TIMEOUT must be a positive integer"
    if [[ "$CONNECTOR_CTL" == /opt/sandbox/bin/connector-ctl && ! -x "$CONNECTOR_CTL" ]]; then
        CONNECTOR_CTL=$(command -v connector-ctl || true)
    fi
    [[ -n "$CONNECTOR_CTL" && -x "$CONNECTOR_CTL" ]] || die "connector-ctl not found; set CONNECTOR_CTL"
}
validate_state_root() {
    local state_mode
    [[ ! -L "$STATE_ROOT" && -d "$STATE_ROOT" ]] || die "DEBUG_STATE_ROOT must be a directory, not a symlink"
    [[ "$(stat -Lc '%u' "$STATE_ROOT")" == "$EUID" ]] || die "DEBUG_STATE_ROOT must be owned by the caller"
    state_mode=$(stat -Lc '%a' "$STATE_ROOT")
    (( (8#$state_mode & 022) == 0 )) || die "DEBUG_STATE_ROOT must not be group/other writable"
}

case "${1:-}" in
    --list|--cleanup-stale)
        action=$1
        [[ "$action" == --list ]] || check_tools
        if [[ -d "$STATE_ROOT" ]]; then
            validate_state_root
            for session_dir in "$STATE_ROOT"/session.*; do
                [[ -d "$session_dir" ]] || continue
                if [[ "$action" == --list ]]; then
                    listed_port=$(field "$session_dir" port)
                    [[ -n "$listed_port" ]] || listed_port=$(field "$session_dir" pending_port)
                    printf '%s switch=%s port=%s fip=%s created=%s ns=%s status=' "$session_dir" \
                        "$(field "$session_dir" switch)" "$listed_port" \
                        "$(field "$session_dir" floating_ip)" "$(field "$session_dir" created_at)" \
                        "$(field "$session_dir" debug_ns)"
                    if owner_alive "$session_dir"; then echo active; else echo stale; fi
                else cleanup_session "$session_dir" 1 || failed=1; fi
            done
        fi
        exit "${failed:-0}" ;;
esac

check_tools
if [[ "${1:-}" == --port ]]; then
    [[ $# -ge 3 ]] || { usage; exit 2; }
    DEBUG_PORT=$2; shift 2
fi
SWITCH_NAME=${1:-}; [[ -n "$SWITCH_NAME" ]] || { usage; exit 2; }; shift
[[ -z "$DEBUG_PORT" || "$DEBUG_PORT" =~ ^[1-9][0-9]{0,3}$ ]] || die "DEBUG_PORT must be an integer from 1 to 4096"
[[ -z "$DEBUG_PORT" ]] || ((DEBUG_PORT <= 4096)) || die "DEBUG_PORT must be an integer from 1 to 4096"
[[ "$DEBUG_CIDR" == */* ]] || die "DEBUG_CIDR must be IPv4 CIDR"
inner_ip=${DEBUG_CIDR%/*}; prefix=${DEBUG_CIDR#*/}
valid_ip "$inner_ip" && [[ "$prefix" =~ ^[0-9]+$ ]] && (( 10#$prefix <= 32 )) || die "invalid DEBUG_CIDR: $DEBUG_CIDR"
valid_ip "$DEBUG_GATEWAY" || die "invalid DEBUG_GATEWAY"
valid_ip "$DEBUG_DNS" || die "invalid DEBUG_DNS"
if [[ -n "$TRANSIT_GATEWAY_IP" ]]; then valid_ip "$TRANSIT_GATEWAY_IP" || die "invalid TRANSIT_GATEWAY_IP"; fi

status_json=$(ctl vswitch status "$SWITCH_NAME") || die "switch unavailable: $SWITCH_NAME"
ready_error=$(ctl vswitch status "$SWITCH_NAME" --ready 2>&1) || die "switch readiness check failed: $SWITCH_NAME: $ready_error"
switch_ns=$(json_string switch_netns <<<"$status_json")
[[ -n "$switch_ns" && -e "/var/run/netns/$switch_ns" ]] || die "switch netns unavailable: $switch_ns"
switch_ns_id=$(ns_identity "/var/run/netns/$switch_ns") || die "cannot identify switch netns"
socat_exe=$(readlink -f "$(command -v socat)") || die "cannot identify socat executable"
[[ ! -L "$STATE_ROOT" ]] || die "DEBUG_STATE_ROOT must not be a symlink"
mkdir -p -m 700 "$STATE_ROOT"
validate_state_root
session_dir=$(mktemp -d "$STATE_ROOT/session.XXXXXXXX")
debug_ns="sandbox_debug_ns_$(basename "$session_dir" | sed 's/^session\.//')"
port=""; pending_port=""; tap_name=""; socat_pid=""; socat_start=""; ifindex=""; floating_ip=""; allocated=0
attach_fd=""; debug_ns_id=""; offloads_changed=0
owner_start=$(proc_start $$)
created_at=$(date -u +%FT%TZ)
save_session() {
    local tmp="$session_dir/session.tmp"
    {
        printf 'owner_pid=%s\nowner_start=%s\nswitch=%s\nswitch_ns=%s\n' "$$" "$owner_start" "$SWITCH_NAME" "$switch_ns"
        printf 'debug_ns=%s\nport=%s\npending_port=%s\ntap_name=%s\ninner_ip=%s\nsocat_pid=%s\nsocat_start=%s\n' "$debug_ns" "$port" "$pending_port" "$tap_name" "$inner_ip" "$socat_pid" "$socat_start"
        printf 'ifindex=%s\nfloating_ip=%s\n' "$ifindex" "$floating_ip"
        printf 'state_format=2\nswitch_ns_id=%s\ndebug_ns_id=%s\noffloads_changed=%s\n' "$switch_ns_id" "$debug_ns_id" "$offloads_changed"
        printf 'socat_exe=%s\n' "$socat_exe"
        printf 'created_at=%s\n' "$created_at"
        if ((allocated)); then echo state=allocated; else echo state=preparing; fi
    } >"$tmp"
    mv -f -- "$tmp" "$session_dir/session"
}
save_session
cleanup() {
    local rc=$?
    trap - EXIT INT TERM HUP
    set +e
    [[ -z "$attach_fd" ]] || exec {attach_fd}>&-
    cleanup_session "$session_dir" || { echo "recovery: $0 --cleanup-stale" >&2; rc=1; }
    exit "$rc"
}
trap cleanup EXIT INT TERM HUP

attach_args=(vswitch attach "$SWITCH_NAME" --inner-ip="$inner_ip")
[[ -z "$TRANSIT_GATEWAY_IP" ]] || attach_args+=(--transit-gateway-ip="$TRANSIT_GATEWAY_IP")
[[ -z "$TRANSIT_GENEVE_VNI" ]] || attach_args+=(--transit-geneve-vni="$TRANSIT_GENEVE_VNI")
[[ -z "$TRANSIT_MAC_ADDR" ]] || attach_args+=(--transit-mac-addr="$TRANSIT_MAC_ADDR")
attach_once() {
    local candidate=$1 worker rc slot
    # Write the intent first, then hold a lock inherited by the worker and
    # connector. A killed parent cannot race recovery against a live attach.
    pending_port=$candidate; save_session || die "cannot save allocation intent"
    exec {attach_fd}>"$session_dir/attach.lock" || die "cannot open attach lock"
    flock -w 60 -x "$attach_fd" || die "attach lock timed out"
    rm -f "$session_dir/attach.rc" || die "cannot reset attach result"
    (
        trap - EXIT INT TERM HUP
        set +e
        ctl "${attach_args[@]}" --port="$candidate" >"$session_dir/attach.json" 2>"$session_dir/attach.err"
        rc=$?
        printf '%s\n' "$rc" >"$session_dir/attach.rc.tmp"
        mv -f "$session_dir/attach.rc.tmp" "$session_dir/attach.rc"
    ) &
    worker=$!
    wait "$worker" || true
    exec {attach_fd}>&-; attach_fd=""
    rc=$(cat "$session_dir/attach.rc" 2>/dev/null) || die "attach outcome unknown; session retained: $session_dir"
    [[ "$rc" =~ ^[0-9]+$ ]] && ((rc < 128)) || die "attach interrupted (status=$rc); session retained: $session_dir"
    if ((rc == 0)); then return 0; fi
    # Only a confirmed rejection or a currently free slot is safe to forget.
    if [[ ! -s "$session_dir/attach.json" ]] && \
       grep -Fq "port $candidate: port already allocated" "$session_dir/attach.err"; then
        pending_port=""; save_session || die "cannot save rejected allocation"; return 2
    fi
    slot=$(ctl vswitch show slots "$SWITCH_NAME" "$((candidate - 1))") || die "cannot inspect failed attach: $session_dir"
    if [[ "$(json_string state <<<"$slot")" == free && ! -s "$session_dir/attach.json" ]]; then
        pending_port=""; save_session || die "cannot save rejected allocation"
    fi
    cat "$session_dir/attach.err" >&2
    die "attach failed; inspect $session_dir"
}
if [[ -n "$DEBUG_PORT" ]]; then
    attach_once "$DEBUG_PORT" || die "failed to allocate TAP port $DEBUG_PORT"
else
    # The sandbox allocator scans from the front. Consider only the last N
    # numbered slots, so an almost full pool cannot silently use a front slot.
    tail_tries=${DEBUG_TAIL_TRIES:-8}
    [[ "$tail_tries" =~ ^[1-9][0-9]{0,3}$ ]] && ((tail_tries <= 4096)) || die "DEBUG_TAIL_TRIES must be from 1 to 4096"
    total_ports=$(json_number ports <<<"$status_json")
    [[ "$total_ports" =~ ^[1-9][0-9]*$ ]] || die "cannot read switch port count"
    tail_limit=$((total_ports / 2))
    ((tail_limit > 0)) || tail_limit=1
    ((tail_tries <= tail_limit)) || tail_tries=$tail_limit
    slots_json=$(ctl vswitch show slots "$SWITCH_NAME") || die "cannot inspect switch slots"
    mapfile -t free_ports < <(awk '
        /"port":/  { split($0, a, ":"); gsub(/[^0-9]/, "", a[2]); p = a[2] + 0 }
        /"state":/ { split($0, a, ":"); gsub(/[^a-z]/, "", a[2]); s = a[2] }
        /"mode":/  { split($0, a, ":"); gsub(/[^a-z]/, "", a[2]); if (s == "free" && a[2] == "tap" && p > max - n) print p }
    ' max="$total_ports" n="$tail_tries" <<<"$slots_json" | sort -rn)
    ((${#free_ports[@]})) || die "no free TAP-mode port in the tail range of $SWITCH_NAME"
    got=0
    for candidate in "${free_ports[@]}"; do
        attach_once "$candidate" || continue
        cport=$(json_number port <"$session_dir/attach.json"); cmode=$(json_string mode <"$session_dir/attach.json")
        if [[ "$cport" == "$candidate" && "$cmode" == tap ]]; then
            DEBUG_PORT=$candidate; got=1; break
        fi
        die "unexpected attach response; session retained: $session_dir"
    done
    ((got)) || die "failed to allocate a TAP port (${#free_ports[@]} tail candidates lost the race)"
    echo "auto-selected tail port=$DEBUG_PORT (front ports left for the sequential allocator)" >&2
fi
attach_json=$(<"$session_dir/attach.json")
port=$(json_number port <<<"$attach_json")
tap_name=$(json_string port_dev <<<"$attach_json")
port_mac=$(json_string port_mac <<<"$attach_json")
floating_ip=$(json_string floating_ip <<<"$attach_json")
[[ "$port" == "$DEBUG_PORT" && -n "$tap_name" && -n "$port_mac" && "$(json_string mode <<<"$attach_json")" == tap ]] || die "unexpected attach response: $attach_json"
allocated=1; save_session
slot=$(ctl vswitch show slots "$SWITCH_NAME" "$((port - 1))")
ifindex=$(json_number ifindex <<<"$slot")
[[ "$ifindex" =~ ^[1-9][0-9]*$ && "$floating_ip" == "$(json_string floating_ip <<<"$slot")" ]] || die "allocated slot identity changed"
save_session
echo "allocated port=$port TAP=$tap_name MAC=$port_mac debug_ns=$debug_ns" >&2
ip netns exec "$switch_ns" ip link show "$tap_name" >/dev/null || die "TAP device missing: $tap_name"
tap_idle "$tap_name" || die "allocated TAP is still in use"
ethtool_k "$switch_ns" "$tap_name" "$session_dir/offloads.tmp" || die "cannot snapshot TAP offloads"
[[ -n "$(features "$session_dir/offloads.tmp")" ]] || die "empty TAP offload snapshot"
mv "$session_dir/offloads.tmp" "$session_dir/offloads"

ip netns add "$debug_ns"
debug_ns_id=$(ns_identity "/var/run/netns/$debug_ns") || die "cannot identify debug netns"
save_session
mkdir -p -m 755 "/etc/netns/$debug_ns"
printf 'nameserver %s\n' "$DEBUG_DNS" >"/etc/netns/$debug_ns/resolv.conf"
offloads_changed=1; save_session
ip netns exec "$switch_ns" ethtool -K "$tap_name" tx off tso off gso off
ip netns exec "$switch_ns" socat \
    "TUN,tun-type=tap,tun-name=$tap_name,no-pi" \
    "TUN,tun-type=tap,tun-name=dbg0,no-pi,netns=$debug_ns" &
socat_pid=$!; socat_start=$(proc_start "$socat_pid"); save_session
for _ in {1..50}; do
    if ip -n "$debug_ns" link show dbg0 >/dev/null 2>&1; then break; fi
    kill -0 "$socat_pid" 2>/dev/null || die "socat exited before creating dbg0"
    sleep .1
done
ip -n "$debug_ns" link show dbg0 >/dev/null 2>&1 || die "debug TAP was not created"
ip -n "$debug_ns" link set dbg0 address "$port_mac"
# Keep dbg0 MTU in sync with the pool TAP (mismatched MTU would distort
# large-packet and Geneve diagnosis on deployments with non-default MTU).
tap_mtu=$(ip netns exec "$switch_ns" ip link show "$tap_name" | awk '{for (i = 1; i < NF; i++) if ($i == "mtu") {print $(i + 1); exit}}')
[[ "$tap_mtu" =~ ^[1-9][0-9]*$ ]] && ip -n "$debug_ns" link set dbg0 mtu "$tap_mtu"
ip -n "$debug_ns" addr add "$DEBUG_CIDR" dev dbg0
ip -n "$debug_ns" link set lo up
ip -n "$debug_ns" link set dbg0 up
ip -n "$debug_ns" route replace default via "$DEBUG_GATEWAY" dev dbg0
echo "entered $debug_ns; exit to release port $port" >&2
# The guest env has no proxy variables; leaking host proxy settings into
# the namespace would fake "network unreachable" as a proxy failure.
run_env=(env)
if [[ -z "${DEBUG_KEEP_PROXY:-}" ]]; then
    for proxy_var in http_proxy https_proxy all_proxy ftp_proxy no_proxy \
                     HTTP_PROXY HTTPS_PROXY ALL_PROXY FTP_PROXY NO_PROXY; do
        run_env+=(-u "$proxy_var")
    done
fi
if (($#)); then ip netns exec "$debug_ns" "${run_env[@]}" "$@"; else ip netns exec "$debug_ns" "${run_env[@]}" "PS1=debug-tap:${SWITCH_NAME}:${port}\$ " bash --noprofile --norc; fi

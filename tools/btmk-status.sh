#!/usr/bin/env bash
# btmk-status — Bitmark Explorer service status dashboard
# Usage: bash btmk-status.sh   (or symlink to /usr/local/bin/btmk-status)

# ── colours & symbols (disabled when not a tty) ───────────────────────────────

if [[ -t 1 ]]; then
    G='\033[0;32m' R='\033[0;31m' Y='\033[1;33m'
    B='\033[0;34m' D='\033[2m'    W='\033[1m'  X='\033[0m'
else
    G='' R='' Y='' B='' D='' W='' X=''
fi

# ── load central credentials ──────────────────────────────────────────────────

CREDS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/credentials.env"
if [[ -f "$CREDS" ]]; then
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
        [[ "$key" =~ ^[[:space:]]*(#|$) ]] && continue
        key="${key// /}"
        [[ -z "$key" ]] && continue
        [[ -v "$key" ]] && continue
        export "$key"="$val"
    done < "$CREDS"
fi

DB_USER="${DB_USER:-bitmark}"  DB_PASS="${DB_PASS:-}"
DB_HOST="${DB_HOST:-127.0.0.1}" DB_PORT="${DB_PORT:-5432}"
DB_NAME="${DB_NAME:-bitmark}"
RPC_USER="${RPC_USER:-}"  RPC_PASS="${RPC_PASS:-}"
RPC_PORT="${RPC_PORT:-}"

# If RPC credentials are missing, fall back to the indexer's env file which holds
# the authoritative RPC_URL, RPC_USER, and RPC_PASS written by setup.
INDEXER_ENV="/etc/bitmark-indexer.env"
if [[ -z "$RPC_PASS" && -f "$INDEXER_ENV" ]]; then
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
        [[ "$key" =~ ^[[:space:]]*(#|$) ]] && continue
        key="${key// /}"; [[ -z "$key" ]] && continue
        case "$key" in
            RPC_USER) [[ -z "$RPC_USER" ]] && RPC_USER="$val" ;;
            RPC_PASS) [[ -z "$RPC_PASS" ]] && RPC_PASS="$val" ;;
            RPC_URL)  [[ -z "$RPC_URL"  ]] && RPC_URL="$val"  ;;
        esac
    done < "$INDEXER_ENV"
fi

# Derive RPC_PORT from RPC_URL if it contains an explicit port.
if [[ -z "$RPC_PORT" && -n "$RPC_URL" ]]; then
    RPC_PORT="${RPC_URL##*:}"; RPC_PORT="${RPC_PORT%%/*}"
    [[ "$RPC_PORT" =~ ^[0-9]+$ ]] || RPC_PORT="9266"
fi
RPC_PORT="${RPC_PORT:-9266}"

# ── helpers ───────────────────────────────────────────────────────────────────

fmt_uptime() {
    local s=$1
    local d=$(( s/86400 )) h=$(( s%86400/3600 )) m=$(( s%3600/60 ))
    (( d > 0 )) && { printf "%dd %02dh" $d $h; return; }
    (( h > 0 )) && { printf "%dh %02dm" $h $m; return; }
                    printf "%dm %02ds" $m $(( s%60 ))
}

fmt_num() {
    local n="${1:-0}" r="" s="${1##-}" sign="${1%%[0-9]*}"
    while (( ${#s} > 3 )); do r=",${s: -3}${r}"; s="${s:0:${#s}-3}"; done
    echo "${sign}${s}${r}"
}

svc_uptime() {
    local ts; ts=$(systemctl show "$1" --property=ActiveEnterTimestamp --value 2>/dev/null)
    [[ -z "$ts" || "$ts" == "n/a" ]] && return
    local t0; t0=$(date -d "$ts" +%s 2>/dev/null) || return
    local e=$(( $(date +%s) - t0 )); (( e < 0 )) && e=0
    fmt_uptime "$e"
}

rpc_call() {
    [[ -z "$RPC_USER" || -z "$RPC_PASS" ]] && return 1
    # No -f: we want the body even on HTTP 5xx so we can parse error codes.
    curl -s --max-time 3 \
        -u "${RPC_USER}:${RPC_PASS}" \
        -H 'content-type: text/plain;' \
        --data-binary "{\"jsonrpc\":\"1.0\",\"method\":\"$1\",\"params\":[]}" \
        "http://127.0.0.1:${RPC_PORT}/" 2>/dev/null
}

pg_query() {
    [[ -z "$DB_PASS" ]] && return 1
    PGPASSWORD="$DB_PASS" psql -tAq \
        -U "$DB_USER" -h "$DB_HOST" -p "$DB_PORT" -d "$DB_NAME" \
        -c "$1" 2>/dev/null
}

# ── data collection ───────────────────────────────────────────────────────────

node_h="" node_status="" db_h=""

if [[ "$(systemctl is-active bitmarkd 2>/dev/null)" == "active" ]]; then
    _rpc=$(rpc_call getblockcount)
    node_h=$(echo "$_rpc" | grep -o '"result":[0-9]*' | grep -o '[0-9]*$')
    if [[ -z "$node_h" ]]; then
        if   echo "$_rpc" | grep -q '"code":-28';  then node_status="loading"
        elif echo "$_rpc" | grep -q '"code":-';    then node_status="rpc-err"
        elif [[ -z "$_rpc" ]];                     then node_status="no-rpc"
        fi
    fi
fi

[[ "$(systemctl is-active bitmark-indexer 2>/dev/null)" == "active" ]] && \
    db_h=$(pg_query "SELECT best_height FROM chain_state WHERE id=1")

# ── render ────────────────────────────────────────────────────────────────────

row() {
    local svc=$1 label=$2 extra=${3:-}

    local state; state=$(systemctl is-active "$svc" 2>/dev/null || echo "unknown")
    local dot col txt
    case "$state" in
        active)      dot="●" col=$G txt="running"  ;;
        activating)  dot="◑" col=$Y txt="starting" ;;
        inactive)    dot="○" col=$Y txt="stopped"  ;;
        failed)      dot="●" col=$R txt="FAILED "  ;;
        *)           dot="○" col=$D txt="unknown"  ;;
    esac

    local up=""; [[ "$state" == "active" ]] && up=$(svc_uptime "$svc")

    printf "  ${col}${dot}${X}  ${W}%-20s${X} ${col}%-8s${X}  ${D}%-11s${X}" \
        "$label" "$txt" "${up:----}"
    [[ -n "$extra" ]] && printf "  %b" "$extra"
    echo
}

echo
printf "  ${W}Bitmark Explorer${X}  ${D}%s${X}\n" \
    "$(date -u '+%a %d %b %Y  %H:%M UTC')"
printf "  ${D}%s${X}\n" "─────────────────────────────────────────────────────"

# bitmarkd metric
nd_extra=""
if   [[ -n "$node_h" ]];                  then nd_extra="${D}height${X}   $(fmt_num "$node_h")"
elif [[ "$node_status" == "loading" ]];   then nd_extra="${Y}loading block index…${X}"
elif [[ "$node_status" == "no-rpc" ]];    then nd_extra="${R}RPC unavailable${X}"
elif [[ "$node_status" == "rpc-err" ]];   then nd_extra="${R}RPC error${X}"
fi

# indexer metric
ix_extra=""
if [[ -n "$db_h" ]]; then
    ix_extra="${D}indexed${X}  $(fmt_num "$db_h")"
    if   [[ -n "$node_h" ]]; then
        lag=$(( node_h - db_h ))
        if   (( lag <= 0 )); then ix_extra+="  ${G}✔ in sync${X}"
        elif (( lag < 10 )); then ix_extra+="  ${Y}${lag} behind${X}"
        else                      ix_extra+="  ${R}${lag} behind${X}"
        fi
    elif [[ "$node_status" == "loading" ]]; then
        ix_extra+="  ${Y}waiting for node${X}"
    fi
fi

row postgresql       "PostgreSQL"
row bitmarkd         "bitmarkd"        "$nd_extra"
row bitmark-indexer  "bitmark-indexer" "$ix_extra"
row bitmark-hp-gen   "bitmark-hp-gen"
row apache2          "Apache"
echo

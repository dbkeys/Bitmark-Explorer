#!/usr/bin/env bash
# access-report — Bitmark Explorer HTTP access log analyser
#
# Produces a plain-text report covering:
#   • Overall request totals and unique IP counts
#   • Human vs bot breakdown (monthly and cumulative)
#   • Top 25 human visitor IPs
#   • Top 20 bot / crawler IPs
#   • HTTP status code distribution
#   • Sampled user-agents for the highest-volume bot IPs
#   • 404 Not Found detail: top missing paths (human & bot), broken-link referrers
#
# Usage:
#   bash access-report.sh              # print to stdout
#   bash access-report.sh -o FILE      # write report to FILE (also printed)
#   bash access-report.sh -q -o FILE   # quiet: FILE only (good for cron)
#
# Cron example — daily at 06:00:
#   0 6 * * * /usr/local/src/Bitmark-Explorer/tools/access-report.sh -q \
#       -o /var/log/btmk-access-report.txt
# ─────────────────────────────────────────────────────────────────────────────

set -eu

# ── configuration ─────────────────────────────────────────────────────────────

ACCESS_LOG="${ACCESS_LOG:-/var/www/${EXPLORER_DOMAIN:-your-explorer-domain.example.com}/logs/access.log}"

BOT_UA='[Bb]ot|[Cc]rawler|[Ss]pider|Googlebot|bingbot|Semrush|AhrefsBot|MJ12bot|DotBot|YandexBot|Bytespider|GPTBot|ClaudeBot|facebookexternalhit|LinkedInBot|Twitterbot|Slurp|curl|python|Go-http|Java/|Wget|libwww|zgrab|Nuclei|masscan|nmap|sqlmap|scanner|nikto|dirbuster|LetsEncrypt'
BOT_REQ='testnet|getblock|xmlrpc|\.php|\.asp|\.env|wp-|\.git'

# ── argument parsing ───────────────────────────────────────────────────────────

OUTPUT_FILE=""
QUIET=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        -o|--output) OUTPUT_FILE="$2"; shift 2 ;;
        -q|--quiet)  QUIET=1; shift ;;
        -h|--help)
            sed -n '2,/^# ─/p' "$0" | sed 's/^# \?//'
            exit 0 ;;
        *) echo "Unknown option: $1" >&2; exit 1 ;;
    esac
done

[[ ! -f "$ACCESS_LOG" ]] && { echo "ERROR: log file not found: $ACCESS_LOG" >&2; exit 1; }

NOW=$(date -u '+%Y-%m-%d %H:%M UTC')
TMPFILE=$(mktemp /tmp/access-report.XXXXXX)
trap 'rm -f "$TMPFILE"' EXIT

# ── pass 1: single awk sweep — emit pre-aggregated tagged records ──────────────
#
#  Output line types (written to TMPFILE):
#    SUM  <key>  <value>          — scalar summary values
#    SC   <code> <count>          — HTTP status code totals
#    MON  <month> <h|b> <count>   — monthly unique IPs per category
#    HIP  <count> <ip>            — human IP with request count
#    BIP  <count> <ip>            — bot IP with request count
#    F404 <h|b> <count> <path>   — 404 path with hit count per category
#    R404 <count> <referrer>      — referrer URL that led humans to a 404

awk -v BOT_UA="$BOT_UA" -v BOT_REQ="$BOT_REQ" '
{
    ip     = $1
    raw_dt = $4
    gsub(/\[/, "", raw_dt)
    split(raw_dt, dt, "/")
    month = dt[2] "/" substr(dt[3],1,4)

    if (NR == 1) first_date = dt[1] "/" dt[2] "/" substr(dt[3],1,4)
    last_date = dt[1] "/" dt[2] "/" substr(dt[3],1,4)

    statcodes[$9]++
    total++

    ua = ""; for (i = 12; i <= NF; i++) ua = ua " " $i
    req = $7
    no_ua    = (ua ~ /^[[:space:]]*"-"[[:space:]]*$/)
    is_bot   = (ua ~ BOT_UA) || (no_ua && req ~ BOT_REQ)

    if (is_bot) {
        bot_total++
        bot_ips[ip]++
        bot_month_ips[month, ip] = 1
    } else {
        human_ips[ip]++
        human_month_ips[month, ip] = 1
    }

    if ($9 == "404") {
        ref = $11; gsub(/"/, "", ref)
        cat = is_bot ? "b" : "h"
        not_found[cat SUBSEP req]++
        if (!is_bot && ref != "-" && ref != "") ref404[ref]++
    }
}

END {
    print "SUM first_date " first_date
    print "SUM last_date  " last_date
    print "SUM total      " total
    print "SUM bot_total  " bot_total
    print "SUM n_all      " (length(human_ips) + length(bot_ips))
    print "SUM n_human    " length(human_ips)
    print "SUM n_bot      " length(bot_ips)

    for (s in statcodes)
        print "SC " s " " statcodes[s]

    # Aggregate monthly unique IP counts
    for (k in human_month_ips) { split(k, a, SUBSEP); hm[a[1]]++ }
    for (k in bot_month_ips)   { split(k, a, SUBSEP); bm[a[1]]++ }
    for (m in hm) print "MON " m " h " hm[m]+0
    for (m in bm) print "MON " m " b " bm[m]+0

    for (ip in human_ips) print "HIP " human_ips[ip] " " ip
    for (ip in bot_ips)   print "BIP " bot_ips[ip]   " " ip

    for (k in not_found) {
        split(k, a, SUBSEP)
        print "F404 " a[1] " " not_found[k] " " a[2]
    }
    for (r in ref404) print "R404 " ref404[r] " " r
}
' "$ACCESS_LOG" > "$TMPFILE"

# ── extract scalars ────────────────────────────────────────────────────────────

gsv() { grep "^SUM $1 " "$TMPFILE" | awk '{print $3}'; }

FIRST_DATE=$(gsv first_date)
LAST_DATE=$(gsv  last_date)
TOTAL=$(gsv      total)
BOT_TOTAL=$(gsv  bot_total)
HUMAN_TOTAL=$(( TOTAL - BOT_TOTAL ))
N_ALL=$(gsv      n_all)
N_HUMAN=$(gsv    n_human)
N_BOT=$(gsv      n_bot)
BOT_PCT=$(awk    "BEGIN{printf \"%.0f\", $BOT_TOTAL*100/$TOTAL}")
HUMAN_PCT=$(awk  "BEGIN{printf \"%.0f\", $HUMAN_TOTAL*100/$TOTAL}")

# ── status codes ──────────────────────────────────────────────────────────────

STATUS_LINES=$(grep "^SC " "$TMPFILE" \
    | awk '{print $2, $3}' \
    | sort -k1,1n \
    | awk '{printf "  %-6s : %d\n", $1, $2}')

# ── monthly breakdown ──────────────────────────────────────────────────────────

MONTHLY_LINES=$(grep "^MON " "$TMPFILE" \
    | awk '
    {
        m=$2; t=$3; c=$4
        if (t=="h") h[m]=c; else b[m]=c
        months[m]=1
    }
    END { for (m in months) print m, h[m]+0, b[m]+0 }
    ' \
    | sort \
    | awk '{printf "  %-12s  %10d  %10d\n", $1, $2, $3}')

# ── top IPs ───────────────────────────────────────────────────────────────────

TOP_HUMAN=$(grep "^HIP " "$TMPFILE" \
    | awk '{print $2, $3}' \
    | sort -rn \
    | head -25 \
    | awk '{printf "  %8d  %s\n", $1, $2}')

TOP_BOT=$(grep "^BIP " "$TMPFILE" \
    | awk '{print $2, $3}' \
    | sort -rn \
    | head -20 \
    | awk '{printf "  %8d  %s\n", $1, $2}')

# ── 404 detail ────────────────────────────────────────────────────────────────

TOTAL_404=$(grep "^F404 " "$TMPFILE"   | awk '{s+=$3} END{print s+0}')
HUMAN_404=$(grep "^F404 h " "$TMPFILE" | awk '{s+=$3} END{print s+0}')
BOT_404=$(grep   "^F404 b " "$TMPFILE" | awk '{s+=$3} END{print s+0}')
UNIQUE_404_HUMAN=$(grep -c "^F404 h " "$TMPFILE" 2>/dev/null || echo 0)
UNIQUE_404_BOT=$(grep   -c "^F404 b " "$TMPFILE" 2>/dev/null || echo 0)

TOP_404_HUMAN=$(grep "^F404 h " "$TMPFILE" \
    | awk '{print $3, $4}' \
    | sort -rn \
    | head -30 \
    | awk '{printf "  %8d  %s\n", $1, $2}')

TOP_404_BOT=$(grep "^F404 b " "$TMPFILE" \
    | awk '{print $3, $4}' \
    | sort -rn \
    | head -20 \
    | awk '{printf "  %8d  %s\n", $1, $2}')

TOP_404_REF=$(grep "^R404 " "$TMPFILE" \
    | awk '{print $2, $3}' \
    | sort -rn \
    | head -20 \
    | awk '{printf "  %8d  %s\n", $1, $2}')

# ── pass 2: sample UA for top 5 bot IPs ───────────────────────────────────────

TOP5_BOT_IPS=$(grep "^BIP " "$TMPFILE" \
    | awk '{print $2, $3}' \
    | sort -rn \
    | head -5 \
    | awk '{print $2}')

UA_LINES=$(while IFS= read -r ip; do
    [[ -z "$ip" ]] && continue
    ua=$(grep "^${ip} " "$ACCESS_LOG" \
        | awk '{for(i=12;i<=NF;i++) printf $i " "; print ""}' \
        | sort | uniq -c | sort -rn \
        | head -1 \
        | sed 's/^ *[0-9]* //' \
        | cut -c1-70)
    printf "  %-20s  %s\n" "$ip" "$ua"
done <<< "$TOP5_BOT_IPS")

# ── assemble and emit report ───────────────────────────────────────────────────

REPORT=$(printf '%s\n' \
"════════════════════════════════════════════════════════════════════" \
"  BITMARK EXPLORER — Access Log Report" \
"  Generated: ${NOW}" \
"════════════════════════════════════════════════════════════════════" \
"" \
"── Date range ────────────────────────────────────────────────────────" \
"  First entry : ${FIRST_DATE}" \
"  Last entry  : ${LAST_DATE}" \
"" \
"── Overall request totals ────────────────────────────────────────────" \
"  Total requests    : ${TOTAL}" \
"  Human requests    : ${HUMAN_TOTAL} (${HUMAN_PCT}%)" \
"  Bot/crawler reqs  : ${BOT_TOTAL} (${BOT_PCT}%)" \
"" \
"── Unique IP counts ──────────────────────────────────────────────────" \
"  All unique IPs    : ${N_ALL}" \
"  Human visitor IPs : ${N_HUMAN}" \
"  Bot / scanner IPs : ${N_BOT}" \
"" \
"── HTTP status codes ─────────────────────────────────────────────────" \
"${STATUS_LINES}" \
"" \
"── Monthly unique IPs ────────────────────────────────────────────────" \
"  $(printf '%-12s  %10s  %10s' 'Month' 'Human IPs' 'Bot IPs')" \
"  $(printf '%-12s  %10s  %10s' '────────────' '─────────' '───────')" \
"${MONTHLY_LINES}" \
"" \
"── Top 25 human visitor IPs ──────────────────────────────────────────" \
"  $(printf '%8s  %-20s' 'Requests' 'IP')" \
"  $(printf '%8s  %-20s' '────────' '────────────────────')" \
"${TOP_HUMAN}" \
"" \
"── Top 20 bot / crawler IPs ──────────────────────────────────────────" \
"  $(printf '%8s  %-20s' 'Requests' 'IP')" \
"  $(printf '%8s  %-20s' '────────' '────────────────────')" \
"${TOP_BOT}" \
"" \
"── Top bot user-agents (sampled from highest-volume IPs) ─────────────" \
"${UA_LINES}" \
"" \
"── 404 Not Found detail ──────────────────────────────────────────────" \
"  Total 404s : ${TOTAL_404}  (human: ${HUMAN_404} across ${UNIQUE_404_HUMAN} paths,  bot/scanner: ${BOT_404} across ${UNIQUE_404_BOT} paths)" \
"" \
"  Missing / moved content — paths requested by real visitors (top 30):" \
"  $(printf '%8s  %s' 'Hits' 'Path')" \
"  $(printf '%8s  %s' '────────' '────────────────────────────────────────────────────────────')" \
"${TOP_404_HUMAN}" \
"" \
"  Bot / scanner probes returning 404 (top 20 — removed or renamed resources):" \
"  $(printf '%8s  %s' 'Hits' 'Path')" \
"  $(printf '%8s  %s' '────────' '────────────────────────────────────────────────────────────')" \
"${TOP_404_BOT}" \
"" \
"  Referrers sending visitors to missing pages — broken inbound links (top 20):" \
"  $(printf '%8s  %s' 'Hits' 'Referrer')" \
"  $(printf '%8s  %s' '────────' '────────────────────────────────────────────────────────────')" \
"${TOP_404_REF}" \
"" \
"════════════════════════════════════════════════════════════════════")

if [[ -n "$OUTPUT_FILE" ]]; then
    echo "$REPORT" > "$OUTPUT_FILE"
    [[ $QUIET -eq 0 ]] && echo "$REPORT"
    echo "Report written to: $OUTPUT_FILE" >&2
else
    echo "$REPORT"
fi

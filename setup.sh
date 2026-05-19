#!/usr/bin/env bash
# setup.sh — full Bitmark Explorer suite installer
#
# Covers README Steps 3–6 (PostgreSQL, indexer, generator, Apache).
# Complete README Steps 0–2 first:
#   0. System packages     (apt-get install ...)
#   1. Go 1.25.0           (download from go.dev)
#   2. Bitmark node        (build & start bitmarkd, wait for full sync)
#
# This script then runs, in order:
#   1. PostgreSQL schema     (database/verify-schema.sh)
#   2. Blockchain indexer    (build binaries + indexer/setup-env.sh)
#   3. Homepage generator    (generator/install.sh)
#   4. Apache virtual host
#
# Run once on a fresh Debian/Ubuntu VPS (must be run as root).
# Safe to re-run: answers are saved to credentials.env so subsequent
# runs require no user input.
#
# Prerequisites (checked at start):
#   - Go 1.25.0 installed at /usr/local/go/bin/go
#   - gcc and pkg-config present (CGo required for ZMQ/ncurses bindings)
#   - PostgreSQL installed and running
#   - bitmarkd fully synced and listening on RPC port
#   - This repository cloned to a local directory

set -euo pipefail

# ── directory layout ─────────────────────────────────────────────────────────
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
PSQL_DIR="${SELF_DIR}/database"
INDEXER_DIR="${SELF_DIR}/indexer"
HPGEN_DIR="${SELF_DIR}/generator"
CREDS_FILE="${SELF_DIR}/credentials.env"

# ── helpers ───────────────────────────────────────────────────────────────────
info()  { echo "[INFO]  $*"; }
warn()  { echo "[WARN]  $*" >&2; }
die()   { echo "[ERROR] $*" >&2; exit 1; }
step()  { echo; echo "=== Step ${1}: ${2} ==="; }

require_root() {
    [[ $EUID -eq 0 ]] || die "Run as root: sudo bash $0"
}

check_dirs() {
    [[ -d "${PSQL_DIR}" ]]    || die "database/ not found at ${PSQL_DIR}"
    [[ -d "${INDEXER_DIR}" ]] || die "indexer/ not found at ${INDEXER_DIR}"
    [[ -d "${HPGEN_DIR}" ]]   || die "generator/ not found at ${HPGEN_DIR}"
}

check_prereqs() {
    local ok=true

    # Go — must be at the path used by the Makefiles and README
    local go_bin="/usr/local/go/bin/go"
    if [[ ! -x "$go_bin" ]]; then
        warn "Go not found at $go_bin"
        warn "Install it first — see README Step 1."
        ok=false
    else
        local go_ver
        go_ver="$("$go_bin" version 2>/dev/null | awk '{print $3}')"
        info "Go: $go_ver"
    fi

    # CGo build tools (ZMQ and ncurses bindings require them)
    for tool in gcc pkg-config; do
        if ! command -v "$tool" &>/dev/null; then
            warn "Missing build tool: $tool  (apt-get install -y build-essential pkg-config)"
            ok=false
        fi
    done

    # PostgreSQL
    if ! systemctl is-active --quiet postgresql 2>/dev/null; then
        warn "PostgreSQL is not running.  Start it with: systemctl start postgresql"
        ok=false
    else
        info "PostgreSQL: running"
    fi

    # Apache2
    if ! command -v apache2ctl &>/dev/null; then
        warn "apache2 not installed.  The vhost step will be skipped."
        warn "Install with: apt-get install -y apache2"
        # Not fatal — setup_apache() handles the missing binary gracefully.
    fi

    [[ "$ok" == true ]] || die "One or more prerequisites are missing — see warnings above."
}

# ── credential prompts ────────────────────────────────────────────────────────

# prompt_text VAR "Description" [default]
# Skips prompt if VAR is already set.  With a default, Enter accepts it.
# Without a default, loops until the user types something.
prompt_text() {
    local var="$1" desc="$2" default="${3:-}"
    local current="${!var:-}"
    if [[ -n "$current" ]]; then
        info "${desc}: ${current}"
        return
    fi
    if [[ -n "$default" ]]; then
        read -rp "  ${desc} [${default}]: " input
        printf -v "$var" '%s' "${input:-$default}"
    else
        while true; do
            read -rp "  ${desc}: " input
            [[ -n "$input" ]] && break
            echo "  (required — cannot be empty)"
        done
        printf -v "$var" '%s' "$input"
    fi
    export "$var"
}

# prompt_pass VAR "Description" [default]
# Same as prompt_text but input is hidden.
prompt_pass() {
    local var="$1" desc="$2" default="${3:-}"
    local current="${!var:-}"
    if [[ -n "$current" ]]; then
        info "${desc}: ***"
        return
    fi
    if [[ -n "$default" ]]; then
        read -rsp "  ${desc} [${default}]: " input; echo
        printf -v "$var" '%s' "${input:-$default}"
    else
        while true; do
            read -rsp "  ${desc}: " input; echo
            [[ -n "$input" ]] && break
            echo "  (required — cannot be empty)"
        done
        printf -v "$var" '%s' "$input"
    fi
    export "$var"
}

# ── load existing credentials ─────────────────────────────────────────────────
load_creds() {
    local src="${CREDS_FILE}"
    [[ -f "$src" ]] || src="/etc/Bitmark-Explorer/credentials.env"
    [[ -f "$src" ]] || return 0
    info "Loading existing credentials from ${src}"
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
        [[ "$key" =~ ^[[:space:]]*(#|$) ]] && continue
        key="${key// /}"
        [[ -z "$key" ]] && continue
        [[ -v "$key" ]] && continue   # env var already set — don't override
        export "$key"="${val}"
    done < "${src}"
}

# ── collect configuration ─────────────────────────────────────────────────────
collect_config() {
    echo
    echo "  Enter values for this explorer instance."
    echo "  Press Enter to accept a [default].  All answers are saved to:"
    echo "  ${CREDS_FILE}"
    echo

    prompt_text EXPLORER_DOMAIN "Explorer hostname (e.g. explorer.example.com)"
    prompt_text INDEXER_USER    "OS user that owns bitmarkd / wallet files" "coins"
    prompt_text RPC_USER        "Bitmark RPC username"                      "bitmarkrpc"
    prompt_pass RPC_PASS        "Bitmark RPC password"
    prompt_text RPC_PORT        "Bitmark RPC port"                          "9266"
    prompt_pass PGSU_PASS       "PostgreSQL superuser password (blank = peer auth)" ""

    if [[ -z "${DB_PASS:-}" ]]; then
        DB_PASS="$(openssl rand -hex 20)"
        export DB_PASS
        info "Generated DB password for role 'bitmark': ${DB_PASS}"
        info "(This will be saved to credentials.env)"
    else
        info "DB password: ***"
    fi
}

# ── write credentials.env ─────────────────────────────────────────────────────
write_creds() {
    cat > "${CREDS_FILE}" <<EOF
# Bitmark Explorer — central credentials
# Written by setup.sh on $(date -u '+%Y-%m-%dT%H:%M:%SZ')
# Keep this file secure: chmod 600 ${CREDS_FILE}

EXPLORER_DOMAIN=${EXPLORER_DOMAIN}
INDEXER_USER=${INDEXER_USER}

PGSU_PASS=${PGSU_PASS}
DB_PASS=${DB_PASS}

RPC_USER=${RPC_USER}
RPC_PASS=${RPC_PASS}
RPC_PORT=${RPC_PORT}
RPC_URL=http://127.0.0.1:${RPC_PORT}
EOF
    chmod 600 "${CREDS_FILE}"
    info "Credentials saved to ${CREDS_FILE}"
}

# ── step 1: PostgreSQL schema ─────────────────────────────────────────────────
setup_schema() {
    step "1/5" "PostgreSQL schema"
    bash "${PSQL_DIR}/verify-schema.sh"
}

# ── step 2: blockchain indexer ────────────────────────────────────────────────
setup_indexer() {
    step "2/5" "Blockchain indexer  (building Go binaries — may take a minute)"

    local go_bin="/usr/local/go/bin/go"

    info "Building bitmark-indexer..."
    (cd "${INDEXER_DIR}" && CGO_ENABLED=1 "$go_bin" build -o bitmark-indexer    ./cmd/bitmark-indexer)    \
        || die "Failed to build bitmark-indexer"

    info "Building backfill-addresses..."
    (cd "${INDEXER_DIR}" && CGO_ENABLED=1 "$go_bin" build -o backfill-addresses ./cmd/backfill-addresses) \
        || die "Failed to build backfill-addresses"

    install -m 755 "${INDEXER_DIR}/bitmark-indexer"    /usr/local/bin/bitmark-indexer
    install -m 755 "${INDEXER_DIR}/backfill-addresses" /usr/local/bin/backfill-addresses
    info "Installed /usr/local/bin/bitmark-indexer and /usr/local/bin/backfill-addresses"

    bash "${INDEXER_DIR}/setup-env.sh"
}

# ── step 3: homepage generator ────────────────────────────────────────────────
setup_generator() {
    step "3/5" "Homepage generator  (builds Go binaries — may take a minute)"
    bash "${HPGEN_DIR}/install.sh"
}

# ── step 4: Apache virtual host ───────────────────────────────────────────────
setup_apache() {
    step "4/5" "Apache virtual host"

    if ! command -v apache2ctl &>/dev/null; then
        warn "apache2 not found — skipping vhost setup."
        warn "Install it with:  apt-get install -y apache2"
        return 0
    fi

    local domain="${EXPLORER_DOMAIN}"
    local conf_file="/etc/apache2/sites-available/${domain}.conf"
    local log_dir="/var/www/${domain}/logs"

    mkdir -p "${log_dir}"

    a2enmod proxy proxy_http rewrite >/dev/null 2>&1 || true

    if [[ -f "${conf_file}" ]]; then
        warn "${conf_file} already exists — leaving it unchanged."
        warn "Delete it and re-run setup.sh to regenerate."
    else
        info "Writing ${conf_file}"
        cat > "${conf_file}" <<VHOST
<VirtualHost *:80>
    ServerName ${domain}
    ServerAlias www.${domain}
    ServerAdmin webmaster@${domain}

    # All traffic is handled by the bitmark-hp-gen HTTP server (port 8088):
    #   /          → static homepage (regenerated on each block)
    #   /?q=...    → block / address / tx lookup
    #   /?view=... → multi-tx blocks view
    #   /events    → SSE push for live updates
    ProxyPreserveHost On
    ProxyPass        / http://127.0.0.1:8088/
    ProxyPassReverse / http://127.0.0.1:8088/

    ErrorLog  ${log_dir}/error.log
    CustomLog ${log_dir}/access.log combined
</VirtualHost>
VHOST

        a2ensite "${domain}" >/dev/null
        if apache2ctl configtest 2>/dev/null; then
            systemctl reload apache2
            info "Virtual host enabled: http://${domain}"
        else
            warn "Apache config test failed — check ${conf_file}"
        fi
    fi

    echo
    echo "  To add HTTPS with a free Let's Encrypt certificate:"
    echo "    apt-get install -y certbot python3-certbot-apache"
    echo "    certbot --apache -d ${domain}"
}

# ── step 5: install tools to /usr/local/bin ───────────────────────────────────
setup_tools() {
    step "5/5" "Command-line tools"
    local tools_dir="${SELF_DIR}/tools"
    local dest="/usr/local/bin"
    local installed=()

    for script in "${tools_dir}"/*.sh; do
        [[ -f "$script" ]] || continue
        local name
        name="$(basename "${script}" .sh)"
        install -m 755 "${script}" "${dest}/${name}"
        installed+=("${name}")
    done

    if [[ ${#installed[@]} -gt 0 ]]; then
        info "Installed to ${dest}: ${installed[*]}"
    else
        warn "No .sh files found in ${tools_dir}"
    fi
}

# ── main ──────────────────────────────────────────────────────────────────────
main() {
    require_root
    check_dirs
    check_prereqs

    echo
    echo "=================================================="
    echo "  Bitmark Explorer — Full Suite Installer"
    echo "=================================================="

    load_creds
    collect_config
    write_creds

    setup_schema
    setup_indexer
    setup_generator
    setup_apache
    setup_tools

    echo
    echo "=================================================="
    echo "  Installation complete"
    echo "=================================================="
    echo
    echo "  Domain:      ${EXPLORER_DOMAIN}"
    echo "  Credentials: ${CREDS_FILE}"
    echo
    echo "  Tools installed to /usr/local/bin:"
    echo "    btmk-status        — service status dashboard"
    echo "    access-report      — HTTP access log analyser"
    echo "    explorer-redirect  — toggle domain redirect in Apache"
    echo
    echo "  Attach to the live homepage-generator TUI:"
    echo "    tmux attach -t bitmark-explorer"
    echo
    echo "  Service logs:"
    echo "    journalctl -u bitmark-indexer -f"
    echo "    journalctl -u bitmark-hp-gen  -f"
    echo
    echo "  One-time address backfill (run after indexer reaches chain tip):"
    echo "    PG_DSN=\"\$(grep PG_DSN /etc/bitmark-indexer.env | cut -d= -f2-)\" \\"
    echo "      /usr/local/bin/backfill-addresses"
    echo
}

main "$@"

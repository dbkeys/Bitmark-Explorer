#!/usr/bin/env bash
# install.sh — build, configure, and install bitmark-hp-gen
#
# The binary runs inside a named tmux session so its ncurses TUI is always
# accessible via:
#
#   tmux attach -t bitmark-explorer
#
# Must be run as root (or with sudo) on a Debian/Ubuntu system.

set -euo pipefail

# Go is installed to /usr/local/go/bin by the README instructions.  That path
# is added to $PATH by /etc/profile.d/go.sh, which is only sourced for
# interactive login shells — not when this script is invoked non-interactively
# (e.g. from setup.sh or sudo).  Prepend it unconditionally so every subsequent
# `go` call in this script works regardless of how the script was launched.
export PATH="/usr/local/go/bin:${PATH}"

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY_NAME="bitmark-hp-gen"
BINARY_PATH="/usr/local/bin/${BINARY_NAME}"
WRAPPER_PATH="/usr/local/bin/${BINARY_NAME}-run"
SERVICE_NAME="bitmark-hp-gen"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
ENV_FILE="/etc/default/${SERVICE_NAME}"
SERVICE_USER="${SUDO_USER:-root}"

# ── load settings.conf ────────────────────────────────────────────────────────
# Defaults are defined here; settings.conf overrides any of them.
# EXPLORER_DOMAIN and the /var/www/* paths are left blank — configure_domain()
# will fill them in (from settings.conf, the env file, or an interactive prompt).
EXPLORER_DOMAIN=""
OUTPUT_DIR=""
OUTPUT_PATH=""
STATIC_DIR=""
TMUX_SESSION="bitmark-explorer"
TMUX_COLS=500
TMUX_ROWS=200

SETTINGS_FILE="${REPO_DIR}/settings.conf"
if [[ -f "${SETTINGS_FILE}" ]]; then
    # Parse settings.conf line-by-line so variables already exported by the
    # calling environment (e.g. from setup.sh) are not overridden.
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
        [[ "$key" =~ ^[[:space:]]*(#|$) ]] && continue
        key="${key// /}"
        [[ -z "$key" ]] && continue
        [[ -v "$key" ]] && continue   # exported env var takes priority
        export "$key"="${val}"
    done < "${SETTINGS_FILE}"
fi

# ── helpers ───────────────────────────────────────────────────────────────────
info()  { echo "[INFO]  $*"; }
warn()  { echo "[WARN]  $*" >&2; }
die()   { echo "[ERROR] $*" >&2; exit 1; }

require_root() {
    [[ $EUID -eq 0 ]] || die "Run this script as root: sudo $0"
}

# ── load central credentials ──────────────────────────────────────────────────
CENTRAL_CREDS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/credentials.env"
[[ -f "$CENTRAL_CREDS" ]] || CENTRAL_CREDS="/etc/Bitmark-Explorer/credentials.env"
if [[ -f "$CENTRAL_CREDS" ]]; then
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
        [[ "$key" =~ ^[[:space:]]*(#|$) ]] && continue
        key="${key// /}"
        [[ -z "$key" ]] && continue
        [[ -v "$key" ]] && continue   # env var takes priority
        export "$key"="$val"
    done < "$CENTRAL_CREDS"
    info "Loaded credentials from $CENTRAL_CREDS"
fi

# ── 1. system dependencies ────────────────────────────────────────────────────
install_deps() {
    info "Updating apt package lists..."
    apt-get update -qq

    local pkgs=()

    # CGo build tools
    dpkg -s gcc        &>/dev/null || pkgs+=(gcc)
    dpkg -s pkg-config &>/dev/null || pkgs+=(pkg-config)

    # ZMQ C library — required by github.com/pebbe/zmq4
    dpkg -s libzmq3-dev &>/dev/null || pkgs+=(libzmq3-dev)

    # ncurses C library — required by github.com/rthornton128/goncurses
    dpkg -s libncurses-dev &>/dev/null || pkgs+=(libncurses-dev)

    # tmux — hosts the ncurses TUI with a proper PTY
    dpkg -s tmux &>/dev/null || pkgs+=(tmux)

    if [[ ${#pkgs[@]} -gt 0 ]]; then
        info "Installing: ${pkgs[*]}"
        apt-get install -y --no-install-recommends "${pkgs[@]}"
    else
        info "System dependencies already present."
    fi
}

# ── 2. Go toolchain ───────────────────────────────────────────────────────────
check_go() {
    if ! command -v go &>/dev/null; then
        die "Go not found. Install Go 1.25.0 to /usr/local/go (see README Step 1) then re-run."
    fi

    local go_ver major minor
    go_ver=$(go version | awk '{print $3}' | sed 's/go//')
    IFS='.' read -r major minor _ <<< "$go_ver"
    if [[ "$major" -lt 1 ]] || { [[ "$major" -eq 1 ]] && [[ "$minor" -lt 21 ]]; }; then
        die "Go 1.21+ required; found go${go_ver}. Upgrade at https://go.dev/dl/"
    fi
    info "Go ${go_ver} found."
}

# ── 3. fetch dependencies & build ─────────────────────────────────────────────
build_binary() {
    info "Fetching Go module dependencies..."
    cd "${REPO_DIR}"
    # Ensure goncurses (and any other new deps) are in go.mod / go.sum
    CGO_ENABLED=1 go get github.com/rthornton128/goncurses
    go mod tidy

    info "Building ${BINARY_NAME}..."
    CGO_ENABLED=1 go build -o "${BINARY_PATH}" \
        ./cmd/bitmark-hp-gen

    chmod 755 "${BINARY_PATH}"
    info "Installed binary -> ${BINARY_PATH}"
}

# ── 4. explorer domain ───────────────────────────────────────────────────────
configure_domain() {
    # 1. settings.conf may have already set EXPLORER_DOMAIN (sourced at top).
    # 2. Fall back to the env file from a previous install run.
    # 3. If still unset, prompt the user.

    if [[ -z "${EXPLORER_DOMAIN}" && -f "${ENV_FILE}" ]]; then
        local saved
        saved=$(grep -Po '(?<=^EXPLORER_DOMAIN=).*' "${ENV_FILE}" 2>/dev/null || true)
        if [[ -n "$saved" ]]; then
            EXPLORER_DOMAIN="$saved"
            info "Domain: ${EXPLORER_DOMAIN} (from ${ENV_FILE})"
        fi
    fi

    if [[ -z "${EXPLORER_DOMAIN}" ]]; then
        echo
        echo "Enter the DNS hostname for this explorer (e.g. explorer.example.com):"
        while true; do
            read -rp "Domain: " EXPLORER_DOMAIN
            [[ -n "${EXPLORER_DOMAIN}" ]] && break
            warn "Domain cannot be empty."
        done
    else
        info "Domain: ${EXPLORER_DOMAIN}"
    fi

    # Derive web-root paths from the domain; these override any partial values
    # that settings.conf may have expanded before EXPLORER_DOMAIN was finalised.
    OUTPUT_DIR="/var/www/${EXPLORER_DOMAIN}/html"
    OUTPUT_PATH="${OUTPUT_DIR}/index.html"
    STATIC_DIR="${OUTPUT_DIR}"
    export EXPLORER_DOMAIN OUTPUT_DIR OUTPUT_PATH STATIC_DIR
    info "Web root: ${OUTPUT_DIR}"
}

# ── 5. PG_DSN ─────────────────────────────────────────────────────────────────
configure_pg_dsn() {
    local pg_dsn=""

    # Prefer an existing env file so re-runs are non-interactive by default.
    if [[ -f "${ENV_FILE}" ]]; then
        pg_dsn=$(grep -Po '(?<=^PG_DSN=).*' "${ENV_FILE}" 2>/dev/null || true)
        if [[ -n "$pg_dsn" ]]; then
            info "Existing PG_DSN found in ${ENV_FILE} — keeping it."
        fi
    fi

    if [[ -z "$pg_dsn" ]]; then
        # Build DSN from central credentials if DB_PASS is available.
        local db_user="${DB_USER:-bitmark}"
        local db_host="${DB_HOST:-127.0.0.1}"
        local db_port="${DB_PORT:-5432}"
        local db_name="${DB_NAME:-bitmark}"

        if [[ -n "${DB_PASS:-}" ]]; then
            pg_dsn="postgres://${db_user}:${DB_PASS}@${db_host}:${db_port}/${db_name}?sslmode=disable"
            info "Built PG_DSN from credentials.env."
        else
            # Fall back to interactive prompt.
            echo
            echo "DB_PASS not found in credentials.env — enter PG_DSN manually."
            echo "Format:  postgres://USER:PASSWORD@HOST:PORT/DBNAME"
            while true; do
                read -rp "PG_DSN: " pg_dsn
                [[ -n "${pg_dsn}" ]] && break
                warn "PG_DSN cannot be empty."
            done
        fi

        if command -v pg_isready &>/dev/null; then
            info "Testing database connectivity..."
            if pg_isready -d "${pg_dsn}" -q; then
                info "Database is reachable."
            else
                warn "pg_isready could not reach the database. Verify credentials and that PostgreSQL is running."
                read -rp "Continue anyway? [y/N]: " cont
                [[ "${cont,,}" =~ ^(y|yes)$ ]] || die "Aborted."
            fi
        fi
    fi

    # Always (re)write the env file so EXPLORER_DOMAIN and PG_DSN are both
    # persisted — future re-runs of install.sh will then be fully non-interactive.
    cat > "${ENV_FILE}" <<EOF
# Environment for ${SERVICE_NAME}
# Generated by install.sh on $(date -u '+%Y-%m-%dT%H:%M:%SZ')
EXPLORER_DOMAIN=${EXPLORER_DOMAIN}
PG_DSN=${pg_dsn}
EOF
    chmod 640 "${ENV_FILE}"
    info "Config saved to ${ENV_FILE}"
}

# ── 6. output directory ───────────────────────────────────────────────────────
create_output_dir() {
    if [[ ! -d "${OUTPUT_DIR}" ]]; then
        info "Creating output directory ${OUTPUT_DIR}"
        mkdir -p "${OUTPUT_DIR}"
    fi
    chown "${SERVICE_USER}" "${OUTPUT_DIR}"
    chmod 755 "${OUTPUT_DIR}"
    info "Output directory ready: ${OUTPUT_DIR}"
}

# ── 7. HTML templates ────────────────────────────────────────────────────────
# The binary reads HTML templates at runtime from TEMPLATE_PATH etc.
# Those paths point into /var/www/templates/bitmark-hp-gen/, which must be
# kept up-to-date whenever the repo templates change.
deploy_templates() {
    local src="${REPO_DIR}/templates"
    local dst="/var/www/templates/bitmark-hp-gen"
    mkdir -p "${dst}"
    local copied=0
    for f in "${src}"/*.html; do
        [[ -e "$f" ]] || continue
        cp "$f" "${dst}/"
        info "Deployed template: $(basename "$f") -> ${dst}/"
        (( copied++ )) || true
    done
    [[ $copied -gt 0 ]] && info "${copied} template(s) deployed to ${dst}." \
                        || info "No HTML templates found in ${src}."
}

# ── 8. static assets ─────────────────────────────────────────────────────────
deploy_static_assets() {
    local assets_dir="${REPO_DIR}/templates"
    local exts=("png" "jpg" "jpeg" "gif" "svg" "ico" "webp" "css" "js")
    local copied=0

    for ext in "${exts[@]}"; do
        for f in "${assets_dir}"/*.${ext}; do
            [[ -e "$f" ]] || continue
            cp "$f" "${OUTPUT_DIR}/"
            info "Deployed static asset: $(basename "$f") -> ${OUTPUT_DIR}/"
            (( copied++ )) || true
        done
    done

    if [[ $copied -eq 0 ]]; then
        info "No static assets found in ${assets_dir}."
    else
        info "${copied} static asset(s) deployed to ${OUTPUT_DIR}."
    fi
}

# ── 8. tmux wrapper script ────────────────────────────────────────────────────
# The wrapper is what runs inside the tmux session.  It sources the env file
# so PG_DSN is available, then execs the binary.  The systemd service starts
# the wrapper inside a detached tmux session and blocks until that session
# ends, giving systemd accurate lifecycle tracking.
install_wrapper() {
    info "Writing tmux wrapper -> ${WRAPPER_PATH}"
    cat > "${WRAPPER_PATH}" <<EOF
#!/usr/bin/env bash
# Wrapper executed inside the tmux session.
# Sources settings.conf (paths/ZMQ/tmux settings) then the env file (PG_DSN).
set -a
# shellcheck source=${SETTINGS_FILE}
[[ -f "${SETTINGS_FILE}" ]] && source "${SETTINGS_FILE}"
# shellcheck source=/etc/default/bitmark-hp-gen
source "${ENV_FILE}"
set +a
exec "${BINARY_PATH}"
EOF
    chmod 755 "${WRAPPER_PATH}"
}

# ── 9. systemd service ────────────────────────────────────────────────────────
# Pattern: a forking service starts the detached tmux session, then a
# separate ExecStartPost script blocks (waits) until the session disappears.
# Using two units is cleaner; here we use a single unit with Type=forking
# and a polling ExecStartPost so that systemd tracks the session lifetime.
#
# Simpler alternative used here: Type=simple with a blocking inline wait loop.
install_service() {
    info "Writing systemd service unit -> ${SERVICE_FILE}"

    cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=Bitmark Explorer Homepage Generator (tmux/ncurses)
Documentation=https://github.com/dbkeys/bitmark-hp-gen
After=network.target postgresql.service
Wants=postgresql.service

[Service]
Type=simple
User=${SERVICE_USER}
# Kill any stale session, start a new detached one at a fixed size so the
# ncurses layout is predictable, then block until the session ends.
# systemd RestartSec / Restart handle recovery if the binary crashes.
ExecStart=/bin/bash -c '\
    tmux kill-session -t ${TMUX_SESSION} 2>/dev/null || true; \
    tmux new-session -d -s ${TMUX_SESSION} \
        -x ${TMUX_COLS} -y ${TMUX_ROWS} \
        "${WRAPPER_PATH}"; \
    tmux set-option -t ${TMUX_SESSION} window-size latest 2>/dev/null || true; \
    while tmux has-session -t ${TMUX_SESSION} 2>/dev/null; do sleep 2; done'
ExecStop=/usr/bin/tmux kill-session -t ${TMUX_SESSION}
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}"
    info "Service enabled."
}

# ── 10. start / restart ───────────────────────────────────────────────────────
start_service() {
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        info "Restarting ${SERVICE_NAME}..."
        systemctl restart "${SERVICE_NAME}"
    else
        info "Starting ${SERVICE_NAME}..."
        systemctl start "${SERVICE_NAME}"
    fi

    sleep 3
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        info "Service is running."
    else
        warn "Service failed to start. Check logs:"
        echo "       journalctl -u ${SERVICE_NAME} -n 50 --no-pager"
        exit 1
    fi
}

# ── main ──────────────────────────────────────────────────────────────────────
main() {
    require_root
    info "=== Installing ${BINARY_NAME} ==="
    install_deps
    check_go
    build_binary
    configure_domain
    configure_pg_dsn
    create_output_dir
    deploy_templates
    deploy_static_assets
    install_wrapper
    install_service
    start_service

    echo
    info "=== Done ==="
    echo "  Domain:   ${EXPLORER_DOMAIN}"
    echo "  Binary:   ${BINARY_PATH}"
    echo "  Wrapper:  ${WRAPPER_PATH}"
    echo "  Config:   ${ENV_FILE}"
    echo "  Output:   ${OUTPUT_PATH}"
    echo ""
    echo "  Attach to the live TUI:"
    echo "    tmux attach -t ${TMUX_SESSION}"
    echo ""
    echo "  Detach again with:  Ctrl-b  d"
    echo "  Service logs:       journalctl -u ${SERVICE_NAME} -f"
}

main "$@"

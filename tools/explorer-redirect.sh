#!/usr/bin/env bash
# explorer-redirect.sh — toggle an old/legacy domain between local service and
# a redirect to the canonical explorer VPS.
#
# Useful when migrating from one hostname to another: keep the old vhost alive
# and redirect visitors to the new URL instead of serving a 404.
#
# Usage:
#   sudo ./explorer-redirect.sh redirect   # redirect OLD_DOMAIN → NEW_URL
#   sudo ./explorer-redirect.sh local      # revert — serve OLD_DOMAIN locally
#
# Configuration — set via environment variables or credentials.env:
#   OLD_DOMAIN   hostname being redirected (e.g. explorer.old-domain.com)
#   NEW_URL      full URL to redirect to  (e.g. https://explorer.new-domain.com)
#   SSL_DOMAIN   domain whose Let's Encrypt cert covers OLD_DOMAIN
#                (defaults to OLD_DOMAIN itself; override if a wildcard or
#                 shared cert lives under a different name)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CREDS_FILE="${SCRIPT_DIR}/../credentials.env"
[[ -f "$CREDS_FILE" ]] || CREDS_FILE="/etc/Bitmark-Explorer/credentials.env"

# Load credentials.env if present (env vars always take priority)
if [[ -f "$CREDS_FILE" ]]; then
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
        [[ "$key" =~ ^[[:space:]]*(#|$) ]] && continue
        key="${key// /}"
        [[ -z "$key" ]] && continue
        [[ -v "$key" ]] && continue
        export "$key"="$val"
    done < "$CREDS_FILE"
fi

OLD_DOMAIN="${OLD_DOMAIN:-}"
NEW_URL="${NEW_URL:-}"
SSL_DOMAIN="${SSL_DOMAIN:-${OLD_DOMAIN}}"

[[ $EUID -eq 0 ]] || { echo "Run as root: sudo $0 <redirect|local>"; exit 1; }

if [[ -z "$OLD_DOMAIN" || -z "$NEW_URL" ]]; then
    echo "ERROR: OLD_DOMAIN and NEW_URL must be set."
    echo "  Set them in credentials.env or export them before running:"
    echo "    OLD_DOMAIN=explorer.old-domain.com NEW_URL=https://explorer.new-domain.com sudo $0 redirect"
    exit 1
fi

CONF_NAME="${OLD_DOMAIN}-redirect"
CONF_FILE="/etc/apache2/sites-available/${CONF_NAME}.conf"
SSL_CERT="/etc/letsencrypt/live/${SSL_DOMAIN}/fullchain.pem"
SSL_KEY="/etc/letsencrypt/live/${SSL_DOMAIN}/privkey.pem"
SSL_OPTS="/etc/letsencrypt/options-ssl-apache.conf"

write_redirect_conf() {
    cat > "${CONF_FILE}" <<EOF
# Redirect ${OLD_DOMAIN} to the canonical explorer URL.
# Managed by explorer-redirect.sh — do not edit by hand.

<VirtualHost *:80>
    ServerName ${OLD_DOMAIN}
    Redirect permanent / ${NEW_URL}/
</VirtualHost>

<IfModule mod_ssl.c>
<VirtualHost *:443>
    ServerName ${OLD_DOMAIN}
    SSLEngine on
    SSLCertificateFile    ${SSL_CERT}
    SSLCertificateKeyFile ${SSL_KEY}
    Include               ${SSL_OPTS}
    Redirect permanent / ${NEW_URL}/
</VirtualHost>
</IfModule>
EOF
}

case "${1:-}" in
    redirect)
        write_redirect_conf
        a2ensite "${CONF_NAME}"
        apache2ctl configtest
        systemctl reload apache2
        echo "OK: ${OLD_DOMAIN} → ${NEW_URL}"
        ;;
    local)
        if a2dissite "${CONF_NAME}" 2>/dev/null; then
            apache2ctl configtest
            systemctl reload apache2
        fi
        echo "OK: ${OLD_DOMAIN} served locally again"
        ;;
    *)
        echo "Usage: $0 <redirect|local>"
        echo
        echo "  redirect   write Apache redirect conf and reload"
        echo "  local      disable redirect conf and reload"
        exit 1
        ;;
esac

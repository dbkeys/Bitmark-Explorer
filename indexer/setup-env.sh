#!/usr/bin/env bash

set -euo pipefail

echo "============================================="
echo " Bitmark Indexer Environment Setup"
echo "============================================="

INDEXER_USER="${INDEXER_USER:-coins}"
DB_NAME="${DB_NAME:-bitmark}"
DB_USER="${DB_USER:-bitmark}"
DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-5432}"
RPC_PORT="${RPC_PORT:-9266}"
ENV_FILE="/etc/bitmark-indexer.env"
BITMARK_CONF="/home/${INDEXER_USER}/.bitmark/bitmark.conf"
CENTRAL_CREDS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/credentials.env"
[[ -f "$CENTRAL_CREDS" ]] || CENTRAL_CREDS="/etc/Bitmark-Explorer/credentials.env"

# ------------------------------------------------------------
# 1. Load central credentials (env vars always override file)
# ------------------------------------------------------------

if [[ -f "$CENTRAL_CREDS" ]]; then
    echo "Loading credentials from $CENTRAL_CREDS"
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
        [[ "$key" =~ ^[[:space:]]*(#|$) ]] && continue
        key="${key// /}"
        [[ -z "$key" ]] && continue
        [[ -v "$key" ]] && continue   # env var takes priority
        export "$key"="$val"
    done < "$CENTRAL_CREDS"
else
    echo "WARNING: credentials.env not found locally or in /etc/Bitmark-Explorer/ — falling back to bitmark.conf and auto-generation."
fi

# ------------------------------------------------------------
# 2. Resolve RPC credentials
#    Priority: credentials.env / env vars > bitmark.conf
# ------------------------------------------------------------

RPC_USER="${RPC_USER:-}"
RPC_PASS="${RPC_PASS:-}"

if [[ -z "$RPC_USER" || -z "$RPC_PASS" ]]; then
    if [[ -f "$BITMARK_CONF" ]]; then
        echo "Reading missing RPC credentials from $BITMARK_CONF..."
        [[ -z "$RPC_USER" ]] && RPC_USER=$(grep -E "^rpcuser="     "$BITMARK_CONF" | cut -d= -f2)
        [[ -z "$RPC_PASS" ]] && RPC_PASS=$(grep -E "^rpcpassword=" "$BITMARK_CONF" | cut -d= -f2)
        conf_port=$(grep -E "^rpcport=" "$BITMARK_CONF" | cut -d= -f2)
        [[ -n "$conf_port" ]] && RPC_PORT="$conf_port"
    fi
fi

[[ -z "$RPC_USER" ]] && { echo "ERROR: RPC_USER not set in credentials.env or $BITMARK_CONF"; exit 1; }
[[ -z "$RPC_PASS" ]] && { echo "ERROR: RPC_PASS not set in credentials.env or $BITMARK_CONF"; exit 1; }

RPC_URL="http://127.0.0.1:${RPC_PORT}"
echo "RPC endpoint: $RPC_URL (user: $RPC_USER)"

# ------------------------------------------------------------
# 2b. Sync rpcpassword into bitmark.conf
#     The indexer authenticates with user+password.  If bitmark.conf
#     lacks rpcpassword the node falls back to cookie-only auth and
#     rejects every Basic-auth attempt with HTTP 401.  We write the
#     password here so credentials.env is the single source of truth.
# ------------------------------------------------------------

BITMARK_CONF_CHANGED=false

if [[ -f "$BITMARK_CONF" ]]; then
    existing_rpc_pass=$(grep -E "^rpcpassword=" "$BITMARK_CONF" | cut -d= -f2-)
    if [[ -z "$existing_rpc_pass" ]]; then
        echo "Adding rpcpassword to $BITMARK_CONF"
        echo "rpcpassword=$RPC_PASS" >> "$BITMARK_CONF"
        BITMARK_CONF_CHANGED=true
    elif [[ "$existing_rpc_pass" != "$RPC_PASS" ]]; then
        echo "Updating rpcpassword in $BITMARK_CONF"
        sed -i "s|^rpcpassword=.*|rpcpassword=$RPC_PASS|" "$BITMARK_CONF"
        BITMARK_CONF_CHANGED=true
    else
        echo "rpcpassword already correct in $BITMARK_CONF"
    fi
else
    echo "WARNING: $BITMARK_CONF not found — cannot write rpcpassword."
    echo "Add  rpcpassword=$RPC_PASS  to bitmark.conf before starting the node."
fi

if [[ "$BITMARK_CONF_CHANGED" == true ]]; then
    echo
    echo "NOTICE: $BITMARK_CONF was updated — restart bitmarkd to apply:"
    echo "  systemctl restart bitmarkd"
    echo
fi

# ------------------------------------------------------------
# 3. Resolve DB password
#    Use credentials.env value; auto-generate only if absent
# ------------------------------------------------------------

DB_PASS="${DB_PASS:-}"

if [[ -z "$DB_PASS" ]]; then
    DB_PASS=$(openssl rand -hex 20)
    echo "Generated DB password for role '$DB_USER': $DB_PASS"
    echo "Add  DB_PASS=$DB_PASS  to $CENTRAL_CREDS so other services can use it."
fi

# ------------------------------------------------------------
# 4. Create PostgreSQL role if missing, set password either way
# ------------------------------------------------------------

echo "Ensuring PostgreSQL role '$DB_USER' exists..."

sudo -u postgres psql <<EOF
DO \$\$
BEGIN
   IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$DB_USER') THEN
      CREATE ROLE $DB_USER LOGIN PASSWORD '$DB_PASS';
   ELSE
      ALTER ROLE $DB_USER WITH PASSWORD '$DB_PASS';
   END IF;
END
\$\$;
EOF

echo "Ensuring database ownership..."

sudo -u postgres psql <<EOF
ALTER DATABASE $DB_NAME OWNER TO $DB_USER;
GRANT ALL PRIVILEGES ON DATABASE $DB_NAME TO $DB_USER;
EOF

sudo -u postgres psql -d "$DB_NAME" <<EOF
GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO $DB_USER;
GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO $DB_USER;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO $DB_USER;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO $DB_USER;
EOF

echo "PostgreSQL permissions configured."

# ------------------------------------------------------------
# 5. Write service environment file
# ------------------------------------------------------------

echo "Writing environment file to $ENV_FILE"

cat > "$ENV_FILE" <<EOF
# Bitmark Indexer — service environment
# Generated by indexer/setup-env.sh on $(date -u '+%Y-%m-%dT%H:%M:%SZ')
# Source of truth for passwords: $CENTRAL_CREDS

PG_DSN=postgres://$DB_USER:$DB_PASS@$DB_HOST:$DB_PORT/$DB_NAME?sslmode=disable

RPC_URL=$RPC_URL
RPC_USER=$RPC_USER
RPC_PASS=$RPC_PASS

CONFIRMATIONS=0
BATCH_SIZE=500
POLL_INTERVAL=2s
ZMQ_ENDPOINT=tcp://127.0.0.1:28332
VERBOSE=false
EOF

chmod 640 "$ENV_FILE"
chown root:"$INDEXER_USER" "$ENV_FILE"

echo "Environment file secured (readable by '$INDEXER_USER' group)."

# ------------------------------------------------------------
# 6. Ensure indexer binary exists
# ------------------------------------------------------------

if [[ ! -f "/usr/local/bin/bitmark-indexer" ]]; then
    echo "WARNING: /usr/local/bin/bitmark-indexer not found"
    echo "Build and install it before starting the service:"
    echo "  cd $(dirname "$(realpath "$0")") && go build -o bitmark-indexer ./cmd/bitmark-indexer && cp bitmark-indexer /usr/local/bin/"
fi

# ------------------------------------------------------------
# 7. Systemd service file
# ------------------------------------------------------------

SERVICE_FILE="/etc/systemd/system/bitmark-indexer.service"

echo "Writing systemd service to $SERVICE_FILE"

cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Bitmark Blockchain Indexer
After=network.target postgresql.service bitmarkd.service

[Service]
User=$INDEXER_USER
Group=$INDEXER_USER
EnvironmentFile=$ENV_FILE
ExecStart=/usr/local/bin/bitmark-indexer
Restart=always
RestartSec=5
LimitNOFILE=65536
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
EOF

chmod 644 "$SERVICE_FILE"

systemctl daemon-reload
systemctl enable bitmark-indexer

echo
echo "============================================="
echo " Setup Complete"
echo "============================================="
echo
echo "Start indexer with:   systemctl start bitmark-indexer"
echo "Check status with:    systemctl status bitmark-indexer"
echo "Logs:                 journalctl -u bitmark-indexer -f"
echo

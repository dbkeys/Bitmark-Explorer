# Bitmark Explorer

A self-hosted blockchain explorer for the [Bitmark](https://bitmark.cc) network. Indexes blocks and transactions into PostgreSQL and serves a live-updating HTML explorer via a built-in HTTP server.

```
bitmarkd ──RPC:9266──► bitmark-indexer ──► PostgreSQL :5432
         └──ZMQ:28332──► bitmark-indexer               ▲
         └──ZMQ:28332──► bitmark-hp-gen ───────────────┘
                                 │
                       HTTP :8088 ◄── Apache :80/443 ◄── browser
```

## Components

| Directory | Purpose |
|-----------|---------|
| `database/` | PostgreSQL schema and idempotent migration script |
| `indexer/` | Blockchain indexer — polls bitmarkd via JSON-RPC, listens for new blocks via ZMQ, stores data in PostgreSQL |
| `generator/` | Static homepage generator — renders HTML from the database, serves it over HTTP with SSE live-updates |
| `tools/` | Status dashboard (`btmk-status.sh`) and access-log analyser (`access-report.sh`) |

## Quick start (automated)

```bash
# Clone and run the one-shot installer (as root):
git clone https://github.com/your-org/Bitmark-Explorer.git /usr/local/src/Bitmark-Explorer
cd /usr/local/src/Bitmark-Explorer
sudo bash setup.sh
```

`setup.sh` prompts for your domain, RPC credentials, and database password, then runs all four steps in order. It is safe to re-run — answers are saved to `credentials.env` so subsequent runs require no user input.

---

## Manual step-by-step installation

### Prerequisites

A fresh **Debian 12 / Ubuntu 22.04+** VPS with root access. All commands below run as root unless otherwise noted.

---

### Step 0 — System packages

```bash
apt-get update && apt-get install -y \
  build-essential autoconf automake libtool pkg-config python3 \
  libboost-all-dev libevent-dev libzmq3-dev libncurses-dev \
  postgresql postgresql-contrib apache2 tmux curl git openssl
```

Create the OS user that will own the Bitmark node process and wallet files:

```bash
useradd -m -s /bin/bash coins
```

---

### Step 1 — Install Go 1.21+

Debian's packaged Go is often too old. Install the upstream release:

```bash
GO_VERSION=1.25.0
curl -OL https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz
tar -C /usr/local -xzf go${GO_VERSION}.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> /etc/profile.d/go.sh
source /etc/profile.d/go.sh
go version   # should print go1.25.0 linux/amd64
```

---

### Step 2 — Build and install the Bitmark node

The indexer requires **bitmarkd v27** built with ZMQ support (`--with-zmq`).

```bash
# Install Bitmark build dependencies (if not already covered by step 0)
apt-get install -y libdb5.3++-dev libminiupnpc-dev libnatpmp-dev

# Clone and build (adjust tag/branch as needed)
git clone https://github.com/bitmark-inc/bitmark.git /usr/local/src/bitmark.cc
cd /usr/local/src/bitmark.cc
git checkout v27.0   # use the latest stable v27.x tag
./autogen.sh
./configure --with-zmq --without-gui --disable-wallet
make -j$(nproc)
make install   # installs bitmarkd, bitmark-cli, etc. into /usr/local/bin
```

#### Configure the node

```bash
install -d -o coins -g coins -m 750 /home/coins/.bitmark

cat > /home/coins/.bitmark/bitmark.conf <<'EOF'
server=1
daemon=0
rpcuser=bitmarkrpc
rpcpassword=CHANGE_THIS_STRONG_RPC_PASSWORD
rpcallowip=127.0.0.1
rpcbind=127.0.0.1
rpcport=9266
zmqpubhashblock=tcp://127.0.0.1:28332
zmqpubrawtx=tcp://127.0.0.1:28332
EOF
chmod 640 /home/coins/.bitmark/bitmark.conf
chown -R coins:coins /home/coins/.bitmark
```

> **Ports**: Bitmark RPC is **9266** (P2P is **9265**) — distinct from Bitcoin's 8332/8333.

#### Create a systemd service for bitmarkd

```bash
cat > /etc/systemd/system/bitmarkd.service <<'EOF'
[Unit]
Description=Bitmark Node
After=network.target

[Service]
User=coins
Group=coins
ExecStart=/usr/local/bin/bitmarkd -datadir=/home/coins/.bitmark
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now bitmarkd
```

#### Monitor sync progress

```bash
# Watch the block count rise to the current chain tip:
watch -n 10 'bitmark-cli -rpcuser=bitmarkrpc \
  -rpcpassword=CHANGE_THIS_STRONG_RPC_PASSWORD \
  -rpcport=9266 getblockcount'
```

> **Important**: let the node fully sync before starting the indexer. Initial sync can take several hours to days depending on your hardware.

---

### Step 3 — Set up PostgreSQL

#### Enable and start PostgreSQL

```bash
systemctl enable --now postgresql
```

#### Create the database role and schema

The recommended approach is the idempotent script in `database/`:

```bash
# Copy and fill in credentials (see credentials.env.example):
cp credentials.env.example credentials.env
chmod 600 credentials.env
$EDITOR credentials.env   # set PGSU_PASS and DB_PASS at minimum

# Run the schema script (creates role, database, all tables):
sudo bash database/verify-schema.sh
```

The script is safe to re-run on an existing installation — it only adds missing columns and indexes, never drops anything.

**What it creates:**

| Table | Purpose |
|-------|---------|
| `blocks` | One row per block: height, hash, algo, difficulty, fee totals |
| `transactions` | One row per transaction: txid, fees, coinbase flag |
| `tx_inputs` | Each input: prev txid/vout, address, value |
| `tx_outputs` | Each output: address, value, spent-by pointer |
| `chain_state` | Single-row checkpoint: best indexed height and hash |
| `algo_stats_daily` | Daily per-algorithm statistics aggregates |
| `algo_stats_current` | Latest per-algorithm stats (difficulty, hashrate, reward) |

#### Optional: performance tuning

Edit `/etc/postgresql/*/main/postgresql.conf` and restart:

```
shared_buffers = 2GB          # ~25% of system RAM
work_mem = 64MB
maintenance_work_mem = 512MB
checkpoint_completion_target = 0.9
wal_buffers = 16MB
```

```bash
systemctl restart postgresql
```

---

### Step 4 — Build and install the indexer

The indexer is a Go program that connects to bitmarkd via JSON-RPC, decodes every block, and writes the data to PostgreSQL. It also subscribes to bitmarkd's ZMQ `hashblock` topic so it processes new blocks immediately as they arrive.

#### Build

```bash
cd indexer
CGO_ENABLED=1 /usr/local/go/bin/go build -o bitmark-indexer ./cmd/bitmark-indexer
CGO_ENABLED=1 /usr/local/go/bin/go build -o backfill-addresses ./cmd/backfill-addresses
cp bitmark-indexer backfill-addresses /usr/local/bin/
```

Or use the Makefile (which sets the Go path and GOPATH correctly):

```bash
cd indexer && make build
```

#### Configure the indexer service

Run the automated setup script (reads from `credentials.env` you created in step 3):

```bash
sudo bash indexer/setup-env.sh
```

This writes `/etc/bitmark-indexer.env` (mode 640, owned by root:coins) and installs the systemd service. Alternatively, create it manually:

```bash
cat > /etc/bitmark-indexer.env <<'EOF'
PG_DSN=postgres://bitmark:YOUR_DB_PASSWORD@127.0.0.1:5432/bitmark?sslmode=disable
RPC_URL=http://127.0.0.1:9266
RPC_USER=bitmarkrpc
RPC_PASS=CHANGE_THIS_STRONG_RPC_PASSWORD
ZMQ_ENDPOINT=tcp://127.0.0.1:28332
CONFIRMATIONS=0
BATCH_SIZE=500
POLL_INTERVAL=2s
VERBOSE=false
EOF
chmod 640 /etc/bitmark-indexer.env
chown root:coins /etc/bitmark-indexer.env
```

#### Create the systemd service

```bash
cat > /etc/systemd/system/bitmark-indexer.service <<'EOF'
[Unit]
Description=Bitmark Blockchain Indexer
After=network.target postgresql.service bitmarkd.service

[Service]
User=coins
Group=coins
EnvironmentFile=/etc/bitmark-indexer.env
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

systemctl daemon-reload
systemctl enable --now bitmark-indexer
journalctl -u bitmark-indexer -f
```

#### Monitor indexing progress

```bash
# Watch the indexed height catch up to the node tip:
watch -n 5 'psql -U bitmark -h 127.0.0.1 -d bitmark \
  -c "SELECT best_height, updated_at FROM chain_state;"'
```

#### Backfill address history (one-time, after initial sync)

Once the indexer has caught up to the tip, run the address backfiller once to populate `prev_address` and `prev_value` on all historical inputs:

```bash
PG_DSN="postgres://bitmark:YOUR_DB_PASSWORD@127.0.0.1:5432/bitmark?sslmode=disable" \
  /usr/local/bin/backfill-addresses
```

This is a one-time operation. The indexer fills these fields automatically for new blocks.

#### Configuration reference

All settings can be placed in `indexer/settings.conf` (copy from `settings.conf.example`) or set as environment variables. Environment variables always take priority.

| Variable | Default | Description |
|----------|---------|-------------|
| `PG_DSN` | — | PostgreSQL connection string (required) |
| `RPC_URL` | `http://127.0.0.1:9266` | bitmarkd JSON-RPC endpoint |
| `RPC_USER` | — | RPC username |
| `RPC_PASS` | — | RPC password |
| `ZMQ_ENDPOINT` | `tcp://127.0.0.1:28332` | ZMQ hashblock publisher (empty = poll only) |
| `CONFIRMATIONS` | `0` | Blocks behind tip before indexing (0 = index at tip) |
| `BATCH_SIZE` | `500` | Blocks per database transaction |
| `POLL_INTERVAL` | `2s` | RPC poll interval when ZMQ is unavailable |
| `VERBOSE` | `false` | Enable verbose logging |

---

### Step 5 — Build and install the page generator

The generator reads from PostgreSQL, renders HTML pages using Go templates, and serves them over a built-in HTTP server on `127.0.0.1:8088`. It subscribes to the same ZMQ feed as the indexer and broadcasts a Server-Sent Events (SSE) notification to connected browsers on each new block, causing them to reload automatically.

The generator runs inside a **named tmux session** so its ncurses TUI stays accessible after install:

```bash
tmux attach -t bitmark-explorer   # attach to the live dashboard
# Ctrl-b d  to detach without stopping it
```

#### Configure the domain

Before installing, set your explorer's public domain. The simplest way is to have it in `credentials.env`:

```
EXPLORER_DOMAIN=explorer.yourdomain.com
```

Or export it before running the installer:

```bash
export EXPLORER_DOMAIN=explorer.yourdomain.com
```

#### Run the installer

```bash
sudo bash generator/install.sh
```

The installer:
1. Installs system dependencies (`libzmq3-dev`, `libncurses-dev`, `tmux`)
2. Builds the `bitmark-hp-gen` binary (CGo required)
3. Copies HTML templates to `/var/www/templates/bitmark-hp-gen/`
4. Creates the web root at `/var/www/<EXPLORER_DOMAIN>/html/`
5. Copies static assets (logo, CSS, JS) to the web root
6. Writes `/etc/default/bitmark-hp-gen` with all environment variables
7. Installs and starts the `bitmark-hp-gen` systemd service

Or build and configure manually:

```bash
cd generator
CGO_ENABLED=1 /usr/local/go/bin/go build -o bitmark-hp-gen ./cmd/bitmark-hp-gen
cp bitmark-hp-gen /usr/local/bin/

mkdir -p /var/www/templates/bitmark-hp-gen
cp templates/*.html /var/www/templates/bitmark-hp-gen/

mkdir -p /var/www/explorer.yourdomain.com/html
cp templates/*.png templates/*.svg templates/*.ico \
   templates/*.css templates/*.js \
   /var/www/explorer.yourdomain.com/html/ 2>/dev/null || true
```

#### Generator configuration reference

All settings can be placed in `generator/settings.conf` (copy from `settings.conf.example`) or set as environment variables.

| Variable | Description |
|----------|-------------|
| `EXPLORER_DOMAIN` | Public DNS hostname — drives all `/var/www/<domain>/` paths |
| `PG_DSN` | PostgreSQL connection string |
| `TEMPLATE_PATH` | Path to `homepage.html` template |
| `BLOCK_TEMPLATE_PATH` | Path to `blockdetail.html` template |
| `ADDR_TEMPLATE_PATH` | Path to `addressdetail.html` template |
| `MULTITX_TEMPLATE_PATH` | Path to `multitx.html` template |
| `OUTPUT_PATH` | Where the rendered `index.html` is written |
| `OUTPUT_DIR` | Web root directory (also serves static files) |
| `LISTEN_ADDR` | HTTP server bind address (default: `127.0.0.1:8088`) |
| `STATIC_DIR` | Directory for static file serving |
| `ZMQ_ENDPOINT` | ZMQ hashblock publisher (default: `tcp://127.0.0.1:28332`) |
| `ZMQ_RECV_TIMEOUT` | Seconds before treating ZMQ as dead (default: `120`) |
| `ZMQ_RECONNECT_DELAY` | Seconds before reconnecting (default: `5`) |
| `TMUX_SESSION` | tmux session name (default: `bitmark-explorer`) |
| `LOG_PATH` | Log file path (default: `/var/log/bitmark-hp-gen.log`) |

---

### Step 6 — Configure Apache as reverse proxy

The generator's HTTP server listens on `127.0.0.1:8088` (localhost only). Apache proxies public traffic to it.

```bash
a2enmod proxy proxy_http rewrite
```

```bash
DOMAIN=explorer.yourdomain.com

cat > /etc/apache2/sites-available/${DOMAIN}.conf <<EOF
<VirtualHost *:80>
    ServerName ${DOMAIN}
    ServerAlias www.${DOMAIN}
    ServerAdmin webmaster@${DOMAIN}

    # All requests go to the bitmark-hp-gen HTTP server:
    #   /          → static homepage, refreshed every block
    #   /?q=...    → block / address / transaction search
    #   /?view=... → multi-transaction block view
    #   /events    → Server-Sent Events stream (live update push)
    ProxyPreserveHost On
    ProxyPass        / http://127.0.0.1:8088/
    ProxyPassReverse / http://127.0.0.1:8088/

    ErrorLog  /var/www/${DOMAIN}/logs/error.log
    CustomLog /var/www/${DOMAIN}/logs/access.log combined
</VirtualHost>
EOF

mkdir -p /var/www/${DOMAIN}/logs
a2ensite ${DOMAIN}
apache2ctl configtest && systemctl reload apache2
```

#### Add HTTPS with Let's Encrypt

```bash
apt-get install -y certbot python3-certbot-apache
certbot --apache -d explorer.yourdomain.com
```

Certbot modifies the vhost automatically and sets up auto-renewal.

---

## Service management

```bash
# Check all services at a glance:
bash tools/btmk-status.sh

# Indexer logs:
journalctl -u bitmark-indexer -f

# Generator logs:
journalctl -u bitmark-hp-gen -f

# Attach to the generator's live TUI:
tmux attach -t bitmark-explorer
# Ctrl-b d  to detach

# Restart after a rebuild:
cd indexer && make install    # stop → copy binary → start
cd generator && make install  # same

# Deploy updated templates/assets without a full reinstall:
cd generator && sudo make deploy-assets
```

---

## Project layout

```
Bitmark-Explorer/
├── README.md                    this file
├── setup.sh                     one-shot full installer
├── credentials.env.example      template — copy to credentials.env and fill in
├── .gitignore
│
├── database/
│   ├── schema.sql               PostgreSQL schema (all tables, indexes, migrations)
│   └── verify-schema.sh         idempotent schema provisioner (safe to re-run)
│
├── indexer/                     Go module: github.com/bitmark/bitmark-indexer
│   ├── Makefile
│   ├── settings.conf.example    config template (copy to settings.conf)
│   ├── setup-env.sh             writes /etc/bitmark-indexer.env and systemd unit
│   ├── go.mod / go.sum
│   ├── cmd/
│   │   ├── bitmark-indexer/     main entry point
│   │   └── backfill-addresses/  one-time historical address filler
│   └── internal/
│       ├── config/              settings loader (file + env var override)
│       ├── db/                  PostgreSQL connection pool
│       ├── rpc/                 JSON-RPC client and block/tx data models
│       ├── ingest/              main ingestion loop, algo stats, status reporting
│       ├── ui/                  ncurses TUI
│       └── zmq/                 ZMQ hashblock subscriber
│
├── generator/                   Go module: github.com/dbkeys/bitmark-hp-gen
│   ├── Makefile
│   ├── settings.conf.example    config template (copy to settings.conf)
│   ├── install.sh               full system installer
│   ├── go.mod / go.sum
│   ├── cmd/bitmark-hp-gen/      main entry point
│   ├── internal/
│   │   ├── db/                  database queries
│   │   ├── render/              HTML page renderers
│   │   ├── rpc/                 optional node RPC client
│   │   ├── sse/                 Server-Sent Events broker
│   │   ├── stats/               statistics models
│   │   ├── tui/                 ncurses TUI
│   │   └── zmq/                 ZMQ subscriber
│   └── templates/               HTML templates and static assets
│       ├── homepage.html
│       ├── blockdetail.html
│       ├── addressdetail.html
│       ├── multitx.html
│       └── bitmark128.png
│
└── tools/
    ├── btmk-status.sh           service status dashboard (coloured terminal output)
    └── access-report.sh         Apache access-log analyser (human vs bot traffic, 404s)
```

---

## Credentials and security

- Copy `credentials.env.example` to `credentials.env` and set `chmod 600 credentials.env`.
- `credentials.env` is listed in `.gitignore` and must **never be committed**.
- The database password and RPC password are the only secrets required. The PostgreSQL superuser password (`PGSU_PASS`) can be left blank on standard Debian/Ubuntu installations that use peer auth.
- All service environment files (`/etc/bitmark-indexer.env`, `/etc/default/bitmark-hp-gen`) are created with mode 640, owned by `root:<service-user>`.

---

## Building from source

Both Go components require **CGo** for their ZMQ and ncurses bindings:

```bash
# System libraries:
apt-get install -y libzmq3-dev libncurses-dev pkg-config gcc

# indexer
cd indexer && CGO_ENABLED=1 go build -o bitmark-indexer ./cmd/bitmark-indexer

# generator
cd generator && CGO_ENABLED=1 go build -o bitmark-hp-gen ./cmd/bitmark-hp-gen
```

The Makefiles in each subdirectory handle this automatically:

```bash
cd indexer   && make build
cd generator && make build
```

---

## Troubleshooting

**Indexer won't start — RPC connection refused**
- Confirm bitmarkd is running: `systemctl status bitmarkd`
- Confirm it is listening on port 9266: `ss -tlnp | grep 9266`
- Check `RPC_URL`, `RPC_USER`, `RPC_PASS` in `/etc/bitmark-indexer.env`

**Indexer hammering the node with retries**
- During node overload the indexer backs off exponentially up to 10 minutes between retries — this is by design.
- Check `journalctl -u bitmark-indexer -n 50` for the current backoff status.

**Generator shows stale data / homepage not updating**
- Check ZMQ is enabled in `bitmark.conf`: look for `zmqpubhashblock=tcp://127.0.0.1:28332`
- Restart bitmarkd after changing `bitmark.conf`
- The generator falls back to DB polling every 2 s if ZMQ is unavailable

**Templates not found / logo missing**
- Run `sudo bash generator/install.sh` again, or just `cd generator && sudo make deploy-assets`
- Templates must be at `/var/www/templates/bitmark-hp-gen/`

**Browser not receiving live updates (SSE)**
- Verify Apache has `ProxyPass / http://127.0.0.1:8088/` (full proxy, not just `/?`)
- The 25 s SSE keepalive prevents Apache from closing idle connections; check Apache timeout settings if it still drops

**Schema verification fails**
- Re-run `sudo bash database/verify-schema.sh` — it will add any missing columns
- If the role or database was manually dropped, the script will recreate them

---

## License

Released under the MIT License. See `LICENSE` for details.

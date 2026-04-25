#!/usr/bin/env bash
# verify-schema.sh — Idempotent schema verification and migration for the
# Bitmark Explorer PostgreSQL database.
#
# Safe to run on:
#   • A brand-new server   (creates role, database, and full schema)
#   • A v1-schema install  (migrates NUMERIC→BIGINT types, adds missing tables/columns)
#   • A current install    (verifies completeness, makes no changes if all is correct)
#   • A broken/partial install (adds any missing columns, fixes broken chain_state)
#
# Usage (run as root or a user that can sudo to postgres):
#   sudo bash verify-schema.sh
#
# Credentials are loaded from credentials.env in the same directory as this
# script (copy credentials.env.example and fill in the values).  Variables
# already set in the calling environment always take priority over the file.

set -euo pipefail

# ── helpers ───────────────────────────────────────────────────────────────────

info()  { echo "[INFO]  $*"; }
ok()    { echo "[OK]    $*"; }
warn()  { echo "[WARN]  $*"; }
die()   { echo "[ERROR] $*" >&2; exit 1; }

# ── load credentials (env vars always override file) ─────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CENTRAL_CREDS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/credentials.env"
LOCAL_CREDS="${SCRIPT_DIR}/credentials.env"
# Explicit override > central location > local fallback
if [[ -z "${CREDS_FILE:-}" ]]; then
    if   [[ -f "$CENTRAL_CREDS" ]]; then CREDS_FILE="$CENTRAL_CREDS"
    elif [[ -f "$LOCAL_CREDS"   ]]; then CREDS_FILE="$LOCAL_CREDS"
    fi
fi

if [[ -f "$CREDS_FILE" ]]; then
    # Read each KEY=VALUE line; skip comments and blanks; never override a
    # variable already set in the calling environment.
    while IFS='=' read -r key val || [[ -n "$key" ]]; do
        [[ "$key" =~ ^[[:space:]]*(#|$) ]] && continue
        key="${key// /}"   # strip stray spaces from the key
        [[ -z "$key" ]] && continue
        [[ -v "$key" ]] && continue   # already set — env var takes priority
        export "$key"="$val"
    done < "$CREDS_FILE"
    info "Loaded credentials from $CREDS_FILE"
fi

# ── configuration ─────────────────────────────────────────────────────────────

DB_NAME="${DB_NAME:-bitmark}"
DB_USER="${DB_USER:-bitmark}"
DB_PASS="${DB_PASS:-}"           # leave empty to auto-generate on first run
DB_HOST="${DB_HOST:-127.0.0.1}"
DB_PORT="${DB_PORT:-5432}"
SU="${PSQL_SUPERUSER:-postgres}" # OS/DB superuser to bootstrap role+database
PGSU_PASS="${PGSU_PASS:-}"       # postgres superuser password (empty = peer/trust auth)

# Run SQL as the postgres superuser via the UNIX socket.
# Uses PGSU_PASS when set (for servers with password auth on the socket);
# otherwise relies on peer auth (the Debian default — no password needed).
pg_super() {
    if [[ -n "$PGSU_PASS" ]]; then
        sudo -u "$SU" env PGPASSWORD="$PGSU_PASS" psql -v ON_ERROR_STOP=1 -p "$DB_PORT" "$@"
    else
        sudo -u "$SU" psql -v ON_ERROR_STOP=1 -p "$DB_PORT" "$@"
    fi
}

# Run SQL as the postgres superuser, connected to the bitmark database.
# DDL (CREATE/ALTER TABLE, CREATE INDEX) must run as superuser so it works
# regardless of who originally created the tables.
pg_super_db() {
    pg_super -d "$DB_NAME" "$@"
}

# Run SQL as the bitmark app user against the bitmark database.
# Sets PGPASSWORD only when DB_PASS is non-empty; otherwise lets pg_hba / .pgpass handle auth.
pg_app() {
    if [[ -n "$DB_PASS" ]]; then
        PGPASSWORD="$DB_PASS" psql -v ON_ERROR_STOP=1 \
            -U "$DB_USER" -h "$DB_HOST" -p "$DB_PORT" -d "$DB_NAME" "$@"
    else
        psql -v ON_ERROR_STOP=1 \
            -U "$DB_USER" -h "$DB_HOST" -p "$DB_PORT" -d "$DB_NAME" "$@"
    fi
}

# ── step 0: require root ──────────────────────────────────────────────────────

[[ $EUID -eq 0 ]] || die "Run as root: sudo bash $0"

# ── step 1: ensure PostgreSQL role exists ─────────────────────────────────────

info "Checking PostgreSQL role '$DB_USER'..."
ROLE_EXISTS=$(pg_super -tAc "SELECT 1 FROM pg_roles WHERE rolname='$DB_USER'" postgres)

if [[ "$ROLE_EXISTS" != "1" ]]; then
    if [[ -z "$DB_PASS" ]]; then
        DB_PASS=$(openssl rand -hex 16)
        info "Generated password for role '$DB_USER': $DB_PASS"
        info "(Save this — it will not be shown again.)"
    fi
    pg_super -c "CREATE ROLE $DB_USER LOGIN PASSWORD '$DB_PASS';" postgres
    ok "Role '$DB_USER' created."
else
    ok "Role '$DB_USER' already exists."
    if [[ -z "$DB_PASS" ]]; then
        # No password is fine — psql will use trust/peer auth or ~/.pgpass
        info "No DB_PASS set; relying on pg_hba.conf auth for role '$DB_USER'."
    fi
fi

# ── step 2: ensure database exists ───────────────────────────────────────────

info "Checking database '$DB_NAME'..."
DB_EXISTS=$(pg_super -tAc "SELECT 1 FROM pg_database WHERE datname='$DB_NAME'" postgres)

if [[ "$DB_EXISTS" != "1" ]]; then
    pg_super -c "CREATE DATABASE $DB_NAME OWNER $DB_USER;" postgres
    ok "Database '$DB_NAME' created."
else
    ok "Database '$DB_NAME' already exists."
fi

# Grant privileges (idempotent)
pg_super -d "$DB_NAME" -c "GRANT ALL ON SCHEMA public TO $DB_USER;" 2>/dev/null || true

# ── step 3: apply schema migrations ──────────────────────────────────────────

info "Applying schema migrations to '$DB_NAME'..."

pg_super_db <<'SQL'

BEGIN;

-- ─────────────────────────────────────────────────────────────────────────────
-- blocks
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS blocks (
  id               BIGSERIAL PRIMARY KEY,
  height           INTEGER NOT NULL,
  hash             CHAR(64) NOT NULL UNIQUE,
  prev_hash        CHAR(64),
  merkle_root      CHAR(64),
  time_utc         TIMESTAMPTZ NOT NULL DEFAULT now(),
  time_unix        BIGINT NOT NULL DEFAULT 0,
  version          BIGINT,
  bits             TEXT,
  nonce            BIGINT,
  size_bytes       INTEGER,
  weight           INTEGER,
  difficulty       DOUBLE PRECISION,
  chainwork        TEXT,
  tx_count         INTEGER NOT NULL DEFAULT 0,
  total_out        BIGINT DEFAULT 0,
  fees             BIGINT DEFAULT 0,
  algo_id          SMALLINT,
  algo_name        TEXT,
  auxpow           BOOLEAN DEFAULT FALSE,
  auxpow_sign      TEXT,
  coreversion      TEXT,
  in_best_chain    BOOLEAN NOT NULL DEFAULT TRUE,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- All optional / later-added columns — safe to run on any pre-existing table
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS prev_hash     CHAR(64);
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS merkle_root   CHAR(64);
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS time_unix     BIGINT NOT NULL DEFAULT 0;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS version       BIGINT;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS bits          TEXT;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS nonce         BIGINT;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS size_bytes    INTEGER;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS weight        INTEGER;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS difficulty    DOUBLE PRECISION;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS chainwork     TEXT;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS total_out     BIGINT DEFAULT 0;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS fees          BIGINT DEFAULT 0;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS algo_id       SMALLINT;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS algo_name     TEXT;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS auxpow        BOOLEAN DEFAULT FALSE;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS auxpow_sign   TEXT;
ALTER TABLE blocks ADD COLUMN IF NOT EXISTS coreversion   TEXT;

-- v1 stored bits as BIGINT; current schema uses TEXT (hex string like "1c00a1ff")
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'blocks' AND column_name = 'bits'
      AND data_type IN ('bigint', 'integer', 'smallint')
  ) THEN
    ALTER TABLE blocks ALTER COLUMN bits TYPE TEXT USING bits::TEXT;
    RAISE NOTICE 'blocks.bits migrated BIGINT → TEXT';
  END IF;
END $$;

-- v1 stored monetary values as NUMERIC(32,8) where 1.00000000 = 1 BTM.
-- Current schema stores satoshis (BIGINT): 1 BTM = 100000000.
DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'blocks' AND column_name = 'total_out'
      AND data_type = 'numeric'
  ) THEN
    ALTER TABLE blocks
      ALTER COLUMN total_out  TYPE BIGINT USING ROUND(COALESCE(total_out,  0) * 1e8)::BIGINT,
      ALTER COLUMN fees       TYPE BIGINT USING ROUND(COALESCE(fees,       0) * 1e8)::BIGINT,
      ALTER COLUMN difficulty TYPE DOUBLE PRECISION USING difficulty::DOUBLE PRECISION;
    RAISE NOTICE 'blocks monetary columns migrated NUMERIC → BIGINT';
  END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- transactions
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS transactions (
  id                 BIGSERIAL PRIMARY KEY,
  txid               CHAR(64) NOT NULL UNIQUE,
  block_hash         CHAR(64),
  block_height       INTEGER,
  position_in_block  INTEGER,
  version            INTEGER,
  locktime           BIGINT,
  size_bytes         INTEGER,
  vsize              INTEGER,
  weight             INTEGER,
  is_coinbase        BOOLEAN NOT NULL DEFAULT FALSE,
  total_in           BIGINT DEFAULT 0,
  total_out          BIGINT DEFAULT 0,
  fee                BIGINT DEFAULT 0,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE transactions ADD COLUMN IF NOT EXISTS block_hash        CHAR(64);
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS block_height      INTEGER;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS position_in_block INTEGER;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS version           INTEGER;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS locktime          BIGINT;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS size_bytes        INTEGER;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS vsize             INTEGER;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS weight            INTEGER;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS total_in          BIGINT DEFAULT 0;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS total_out         BIGINT DEFAULT 0;
ALTER TABLE transactions ADD COLUMN IF NOT EXISTS fee               BIGINT DEFAULT 0;

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'transactions' AND column_name = 'total_out'
      AND data_type = 'numeric'
  ) THEN
    ALTER TABLE transactions
      ALTER COLUMN total_in  TYPE BIGINT USING ROUND(COALESCE(total_in, 0) * 1e8)::BIGINT,
      ALTER COLUMN total_out TYPE BIGINT USING ROUND(COALESCE(total_out,0) * 1e8)::BIGINT,
      ALTER COLUMN fee       TYPE BIGINT USING ROUND(COALESCE(fee,      0) * 1e8)::BIGINT;
    RAISE NOTICE 'transactions monetary columns migrated NUMERIC → BIGINT';
  END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- tx_outputs
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tx_outputs (
  id               BIGSERIAL PRIMARY KEY,
  txid             CHAR(64) NOT NULL,
  vout             INTEGER NOT NULL,
  value            BIGINT NOT NULL,
  script_type      TEXT,
  script_hex       TEXT,
  address          TEXT,
  spent_by_txid    CHAR(64),
  spent_by_vin     INTEGER,
  spent_in_height  INTEGER,
  UNIQUE(txid, vout)
);

ALTER TABLE tx_outputs ADD COLUMN IF NOT EXISTS vout             INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tx_outputs ADD COLUMN IF NOT EXISTS script_type      TEXT;
ALTER TABLE tx_outputs ADD COLUMN IF NOT EXISTS script_hex       TEXT;
ALTER TABLE tx_outputs ADD COLUMN IF NOT EXISTS address          TEXT;
ALTER TABLE tx_outputs ADD COLUMN IF NOT EXISTS spent_by_txid    CHAR(64);
ALTER TABLE tx_outputs ADD COLUMN IF NOT EXISTS spent_by_vin     INTEGER;
ALTER TABLE tx_outputs ADD COLUMN IF NOT EXISTS spent_in_height  INTEGER;

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'tx_outputs' AND column_name = 'value'
      AND data_type = 'numeric'
  ) THEN
    ALTER TABLE tx_outputs
      ALTER COLUMN value TYPE BIGINT USING ROUND(value * 1e8)::BIGINT;
    RAISE NOTICE 'tx_outputs.value migrated NUMERIC → BIGINT';
  END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- tx_inputs
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS tx_inputs (
  id               BIGSERIAL PRIMARY KEY,
  txid             CHAR(64) NOT NULL,
  vin              INTEGER NOT NULL,
  prev_txid        CHAR(64),
  prev_vout        INTEGER,
  script_sig_hex   TEXT,
  sequence         BIGINT,
  prev_value       BIGINT,
  prev_address     TEXT,
  is_coinbase      BOOLEAN NOT NULL DEFAULT FALSE,
  coinbase_hex     TEXT,
  UNIQUE(txid, vin)
);

ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS vin            INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS prev_txid      CHAR(64);
ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS prev_vout      INTEGER;
ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS script_sig_hex TEXT;
ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS sequence       BIGINT;
ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS prev_value     BIGINT;
ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS prev_address   TEXT;
ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS is_coinbase    BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE tx_inputs ADD COLUMN IF NOT EXISTS coinbase_hex   TEXT;

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'tx_inputs' AND column_name = 'prev_value'
      AND data_type = 'numeric'
  ) THEN
    ALTER TABLE tx_inputs
      ALTER COLUMN prev_value TYPE BIGINT USING ROUND(COALESCE(prev_value, 0) * 1e8)::BIGINT;
    RAISE NOTICE 'tx_inputs.prev_value migrated NUMERIC → BIGINT';
  END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- chain_state  (single-row checkpoint)
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS chain_state (
  id          SMALLINT PRIMARY KEY DEFAULT 1,
  best_height INTEGER  NOT NULL DEFAULT -1,
  best_hash   CHAR(64) NOT NULL DEFAULT repeat('0', 64),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Fix any pre-existing table that is missing the expected columns
-- (handles tables created by unrelated software or partial migrations).
ALTER TABLE chain_state ADD COLUMN IF NOT EXISTS best_height INTEGER   NOT NULL DEFAULT -1;
ALTER TABLE chain_state ADD COLUMN IF NOT EXISTS best_hash   CHAR(64)  NOT NULL DEFAULT repeat('0', 64);
ALTER TABLE chain_state ADD COLUMN IF NOT EXISTS updated_at  TIMESTAMPTZ NOT NULL DEFAULT now();

-- Ensure the singleton row exists
INSERT INTO chain_state (id, best_height, best_hash)
VALUES (1, -1, repeat('0', 64))
ON CONFLICT (id) DO NOTHING;

-- ─────────────────────────────────────────────────────────────────────────────
-- algo_stats_daily
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS algo_stats_daily (
  day                         DATE NOT NULL,
  algo_id                     SMALLINT NOT NULL,
  blocks                      INTEGER NOT NULL DEFAULT 0,
  avg_difficulty              DOUBLE PRECISION,
  avg_block_spacing_seconds   DOUBLE PRECISION,
  est_hashrate_ghs            DOUBLE PRECISION,
  money_supply                BIGINT,
  computed_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (day, algo_id)
);

-- v1 had NUMERIC types and was missing computed_at
ALTER TABLE algo_stats_daily ADD COLUMN IF NOT EXISTS day                         DATE NOT NULL DEFAULT CURRENT_DATE;
ALTER TABLE algo_stats_daily ADD COLUMN IF NOT EXISTS algo_id                     SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE algo_stats_daily ADD COLUMN IF NOT EXISTS blocks                      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE algo_stats_daily ADD COLUMN IF NOT EXISTS avg_difficulty              DOUBLE PRECISION;
ALTER TABLE algo_stats_daily ADD COLUMN IF NOT EXISTS avg_block_spacing_seconds   DOUBLE PRECISION;
ALTER TABLE algo_stats_daily ADD COLUMN IF NOT EXISTS est_hashrate_ghs            DOUBLE PRECISION;
ALTER TABLE algo_stats_daily ADD COLUMN IF NOT EXISTS money_supply                BIGINT;
ALTER TABLE algo_stats_daily ADD COLUMN IF NOT EXISTS computed_at                 TIMESTAMPTZ NOT NULL DEFAULT now();

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'algo_stats_daily' AND column_name = 'avg_difficulty'
      AND data_type = 'numeric'
  ) THEN
    ALTER TABLE algo_stats_daily
      ALTER COLUMN avg_difficulty            TYPE DOUBLE PRECISION USING avg_difficulty::DOUBLE PRECISION,
      ALTER COLUMN avg_block_spacing_seconds TYPE DOUBLE PRECISION USING avg_block_spacing_seconds::DOUBLE PRECISION,
      ALTER COLUMN est_hashrate_ghs          TYPE DOUBLE PRECISION USING est_hashrate_ghs::DOUBLE PRECISION,
      ALTER COLUMN money_supply              TYPE BIGINT USING ROUND(COALESCE(money_supply, 0) * 1e8)::BIGINT;
    RAISE NOTICE 'algo_stats_daily columns migrated NUMERIC → DOUBLE PRECISION / BIGINT';
  END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- algo_stats_current  (not present in v1 schema)
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS algo_stats_current (
  algo_id              SMALLINT PRIMARY KEY,
  algo_name            TEXT NOT NULL,
  difficulty           DOUBLE PRECISION NOT NULL,
  hashrate             DOUBLE PRECISION NOT NULL,
  peak_hashrate        DOUBLE PRECISION NOT NULL,
  money_supply         BIGINT NOT NULL,
  block_reward         DOUBLE PRECISION NOT NULL,
  nominal_block_reward DOUBLE PRECISION NOT NULL DEFAULT 0,
  nssf_remaining       INTEGER NOT NULL,
  block_spacing        DOUBLE PRECISION NOT NULL,
  computed_at          TIMESTAMPTZ NOT NULL
);

-- Columns added after initial current-schema release
ALTER TABLE algo_stats_current ADD COLUMN IF NOT EXISTS nominal_block_reward DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE algo_stats_current ADD COLUMN IF NOT EXISTS nssf_remaining       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE algo_stats_current ADD COLUMN IF NOT EXISTS peak_hashrate        DOUBLE PRECISION NOT NULL DEFAULT 0;

-- ─────────────────────────────────────────────────────────────────────────────
-- ─────────────────────────────────────────────────────────────────────────────
-- Unique constraints required by ON CONFLICT clauses in bitmark-indexer.
-- CREATE UNIQUE INDEX IF NOT EXISTS is idempotent and satisfies ON CONFLICT.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE UNIQUE INDEX IF NOT EXISTS uq_blocks_hash
  ON blocks (hash);

CREATE UNIQUE INDEX IF NOT EXISTS uq_transactions_txid
  ON transactions (txid);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tx_inputs_txid_vin
  ON tx_inputs (txid, vin);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tx_outputs_txid_vout
  ON tx_outputs (txid, vout);

-- chain_state uses ON CONFLICT (id) which requires a primary key or unique constraint.
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.table_constraints
    WHERE table_name = 'chain_state' AND constraint_type = 'PRIMARY KEY'
  ) THEN
    ALTER TABLE chain_state ADD PRIMARY KEY (id);
  END IF;
END $$;

-- algo_stats_current uses ON CONFLICT (algo_id) DO UPDATE.
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.table_constraints
    WHERE table_name = 'algo_stats_current' AND constraint_type = 'PRIMARY KEY'
  ) THEN
    ALTER TABLE algo_stats_current ADD PRIMARY KEY (algo_id);
  END IF;
END $$;

-- ─────────────────────────────────────────────────────────────────────────────
-- indexes
-- ─────────────────────────────────────────────────────────────────────────────

-- ─────────────────────────────────────────────────────────────────────────────

CREATE INDEX IF NOT EXISTS blocks_height_best_idx
  ON blocks (height DESC) WHERE in_best_chain;

CREATE INDEX IF NOT EXISTS blocks_algo_height_idx
  ON blocks (algo_id, height DESC) WHERE in_best_chain;

CREATE INDEX IF NOT EXISTS blocks_time_utc_best_idx
  ON blocks (time_utc) WHERE in_best_chain = TRUE;

CREATE INDEX IF NOT EXISTS transactions_block_height_idx
  ON transactions (block_height DESC);

CREATE INDEX IF NOT EXISTS transactions_block_hash_idx
  ON transactions (block_hash);

CREATE INDEX IF NOT EXISTS tx_outputs_address_idx
  ON tx_outputs (address);

CREATE INDEX IF NOT EXISTS tx_outputs_unspent_idx
  ON tx_outputs (address) WHERE spent_by_txid IS NULL;

CREATE INDEX IF NOT EXISTS tx_inputs_txid_idx
  ON tx_inputs (txid);

CREATE INDEX IF NOT EXISTS tx_inputs_prevout_idx
  ON tx_inputs (prev_txid, prev_vout);

COMMIT;

SQL

# Transfer ownership of every table and sequence to the app user so that
# bitmark-indexer and bitmark-hp-gen can operate without superuser rights.
info "Setting table ownership and privileges for role '$DB_USER'..."
for tbl in blocks transactions tx_outputs tx_inputs chain_state \
            algo_stats_daily algo_stats_current; do
    pg_super_db -c "ALTER TABLE $tbl OWNER TO $DB_USER;" 2>/dev/null && \
        ok "  $tbl OWNER → $DB_USER" || \
        warn "  $tbl: could not change owner (may not exist yet)"
done
pg_super_db -c "GRANT ALL ON ALL TABLES    IN SCHEMA public TO $DB_USER;"
pg_super_db -c "GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO $DB_USER;"
ok "Privileges granted to '$DB_USER'."

# ── step 4: verify ────────────────────────────────────────────────────────────

info "Verifying schema..."

ERRORS=0

check_col() {
    local tbl=$1 col=$2 found
    local sql="SELECT 1 FROM information_schema.columns
               WHERE table_name='$tbl' AND column_name='$col'"
    if [[ -n "$DB_PASS" ]]; then
        found=$(PGPASSWORD="$DB_PASS" psql -tAq \
            -U "$DB_USER" -h "$DB_HOST" -p "$DB_PORT" -d "$DB_NAME" \
            -c "$sql" 2>/dev/null)
    else
        found=$(psql -tAq \
            -U "$DB_USER" -h "$DB_HOST" -p "$DB_PORT" -d "$DB_NAME" \
            -c "$sql" 2>/dev/null)
    fi
    if [[ "$found" == "1" ]]; then
        ok "$tbl.$col"
    else
        warn "MISSING: $tbl.$col"
        ERRORS=$(( ERRORS + 1 ))
    fi
}

check_col blocks         height
check_col blocks         hash
check_col blocks         time_unix
check_col blocks         bits
check_col blocks         difficulty
check_col blocks         total_out
check_col blocks         fees
check_col blocks         algo_id
check_col blocks         algo_name
check_col blocks         auxpow_sign
check_col blocks         coreversion
check_col blocks         in_best_chain

check_col transactions   txid
check_col transactions   block_hash
check_col transactions   block_height
check_col transactions   position_in_block
check_col transactions   is_coinbase
check_col transactions   total_out
check_col transactions   fee

check_col tx_outputs     txid
check_col tx_outputs     vout
check_col tx_outputs     value
check_col tx_outputs     address
check_col tx_outputs     spent_by_txid

check_col tx_inputs      txid
check_col tx_inputs      vin
check_col tx_inputs      prev_txid
check_col tx_inputs      prev_vout
check_col tx_inputs      prev_value
check_col tx_inputs      prev_address
check_col tx_inputs      is_coinbase
check_col tx_inputs      coinbase_hex

check_col chain_state    best_height
check_col chain_state    best_hash
check_col chain_state    updated_at

check_col algo_stats_daily    day
check_col algo_stats_daily    algo_id
check_col algo_stats_daily    avg_difficulty
check_col algo_stats_daily    computed_at

check_col algo_stats_current  algo_id
check_col algo_stats_current  algo_name
check_col algo_stats_current  difficulty
check_col algo_stats_current  hashrate
check_col algo_stats_current  peak_hashrate
check_col algo_stats_current  money_supply
check_col algo_stats_current  block_reward
check_col algo_stats_current  nominal_block_reward
check_col algo_stats_current  nssf_remaining
check_col algo_stats_current  block_spacing
check_col algo_stats_current  computed_at

# ── summary ───────────────────────────────────────────────────────────────────

echo
if [[ $ERRORS -eq 0 ]]; then
    ok "Schema verification passed — all required columns present."
    echo
    info "PG_DSN to use in service env files:"
    echo "  postgres://${DB_USER}:${DB_PASS}@${DB_HOST}:${DB_PORT}/${DB_NAME}?sslmode=disable"
else
    die "$ERRORS column(s) still missing after migration. Check output above."
fi

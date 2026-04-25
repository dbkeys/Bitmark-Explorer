BEGIN;

DROP TABLE IF EXISTS algo_stats_current CASCADE;
DROP TABLE IF EXISTS algo_stats_daily CASCADE;
DROP TABLE IF EXISTS tx_inputs CASCADE;
DROP TABLE IF EXISTS tx_outputs CASCADE;
DROP TABLE IF EXISTS transactions CASCADE;
DROP TABLE IF EXISTS blocks CASCADE;
DROP TABLE IF EXISTS chain_state CASCADE;

-- ============================================================
-- Blocks
-- ============================================================

CREATE TABLE blocks (
  id               BIGSERIAL PRIMARY KEY,
  height           INTEGER NOT NULL,
  hash             CHAR(64) NOT NULL UNIQUE,
  prev_hash        CHAR(64),
  merkle_root      CHAR(64),

  time_utc         TIMESTAMPTZ NOT NULL,
  time_unix        BIGINT NOT NULL,

  version          BIGINT,
  bits             TEXT,          -- hex string e.g. "1c00a1ff"
  nonce            BIGINT,
  size_bytes       INTEGER,
  weight           INTEGER,

  difficulty       DOUBLE PRECISION,  -- weighted: raw × algo_weight
  chainwork        TEXT,

  tx_count         INTEGER NOT NULL,

  total_out        BIGINT DEFAULT 0,   -- base units (satoshis)
  fees             BIGINT DEFAULT 0,   -- base units (satoshis)

  algo_id          SMALLINT,
  algo_name        TEXT,
  auxpow           BOOLEAN DEFAULT FALSE,
  auxpow_sign      TEXT,

  coreversion      TEXT,
  in_best_chain    BOOLEAN NOT NULL DEFAULT TRUE,

  created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX blocks_height_best_idx
  ON blocks (height DESC)
  WHERE in_best_chain;

CREATE INDEX blocks_algo_height_idx
  ON blocks (algo_id, height DESC)
  WHERE in_best_chain;

CREATE INDEX blocks_time_utc_best_idx
  ON blocks (time_utc)
  WHERE in_best_chain = TRUE;

-- ============================================================
-- Transactions
-- ============================================================

CREATE TABLE transactions (
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

  total_in           BIGINT DEFAULT 0,   -- base units
  total_out          BIGINT DEFAULT 0,   -- base units
  fee                BIGINT DEFAULT 0,   -- base units

  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX transactions_block_height_idx
  ON transactions (block_height DESC);

CREATE INDEX transactions_block_hash_idx
  ON transactions (block_hash);

-- ============================================================
-- Outputs
-- ============================================================

CREATE TABLE tx_outputs (
  id               BIGSERIAL PRIMARY KEY,
  txid             CHAR(64) NOT NULL,
  vout             INTEGER NOT NULL,

  value            BIGINT NOT NULL,   -- base units
  script_type      TEXT,
  script_hex       TEXT,
  address          TEXT,

  spent_by_txid    CHAR(64),
  spent_by_vin     INTEGER,
  spent_in_height  INTEGER,

  UNIQUE(txid, vout)
);

CREATE INDEX tx_outputs_address_idx
  ON tx_outputs (address);

CREATE INDEX tx_outputs_unspent_idx
  ON tx_outputs (address)
  WHERE spent_by_txid IS NULL;

-- ============================================================
-- Inputs
-- ============================================================

CREATE TABLE tx_inputs (
  id               BIGSERIAL PRIMARY KEY,
  txid             CHAR(64) NOT NULL,
  vin              INTEGER NOT NULL,

  prev_txid        CHAR(64),
  prev_vout        INTEGER,

  script_sig_hex   TEXT,
  sequence         BIGINT,

  prev_value       BIGINT,  -- base units
  prev_address     TEXT,

  is_coinbase      BOOLEAN NOT NULL DEFAULT FALSE,
  coinbase_hex     TEXT,

  UNIQUE(txid, vin)
);

-- ============================================================
-- Chain State
-- ============================================================

CREATE TABLE chain_state (
  id               SMALLINT PRIMARY KEY DEFAULT 1,
  best_height      INTEGER NOT NULL,
  best_hash        CHAR(64) NOT NULL,
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO chain_state (id, best_height, best_hash)
VALUES (1, -1, repeat('0', 64))
ON CONFLICT (id) DO NOTHING;

-- ============================================================
-- Algo Daily Stats
-- ============================================================

CREATE TABLE algo_stats_daily (
  day                         DATE NOT NULL,
  algo_id                     SMALLINT NOT NULL,
  blocks                      INTEGER NOT NULL DEFAULT 0,
  avg_difficulty              DOUBLE PRECISION,
  avg_block_spacing_seconds   DOUBLE PRECISION,
  est_hashrate_ghs            DOUBLE PRECISION,
  money_supply                BIGINT,          -- base units
  computed_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY (day, algo_id)
);

-- ============================================================
-- Algo Current Stats
-- Updated by bitmark-indexer every ~90 same-algo blocks (~1 day).
-- difficulty, hashrate, peak_hashrate come from chaindynamics RPC.
-- nominal_block_reward is derived via inverse CERM v1 formula.
-- block_spacing is in minutes (from chaindynamics directly).
-- money_supply is stored in base units (satoshis).
-- ============================================================

CREATE TABLE algo_stats_current (
  algo_id              SMALLINT PRIMARY KEY,
  algo_name            TEXT NOT NULL,
  difficulty           DOUBLE PRECISION NOT NULL,
  hashrate             DOUBLE PRECISION NOT NULL,
  peak_hashrate        DOUBLE PRECISION NOT NULL,
  money_supply         BIGINT NOT NULL,          -- base units
  block_reward         DOUBLE PRECISION NOT NULL,
  nominal_block_reward DOUBLE PRECISION NOT NULL DEFAULT 0,
  nssf_remaining       INTEGER NOT NULL,
  block_spacing        DOUBLE PRECISION NOT NULL, -- minutes
  computed_at          TIMESTAMPTZ NOT NULL
);

COMMIT;

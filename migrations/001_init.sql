CREATE TABLE IF NOT EXISTS funding_candidates (
  id BIGSERIAL PRIMARY KEY,
  exchange TEXT NOT NULL,
  symbol TEXT NOT NULL,
  funding_rate DOUBLE PRECISION NOT NULL,
  next_funding_time TIMESTAMPTZ,
  price DOUBLE PRECISION,
  mark_price DOUBLE PRECISION,
  volume_24h_usdt DOUBLE PRECISION,
  raw_fair_price DOUBLE PRECISION,
  planned_entry DOUBLE PRECISION,
  planned_tp DOUBLE PRECISION,
  selected BOOLEAN DEFAULT FALSE,
  created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS trades (
  id BIGSERIAL PRIMARY KEY,
  exchange TEXT NOT NULL,
  symbol TEXT NOT NULL,
  scenario TEXT,
  side TEXT,
  status TEXT,
  entry_price DOUBLE PRECISION,
  exit_price DOUBLE PRECISION,
  qty DOUBLE PRECISION,
  funding_rate DOUBLE PRECISION,
  funding_fee DOUBLE PRECISION,
  fees DOUBLE PRECISION,
  gross_pnl DOUBLE PRECISION,
  net_pnl DOUBLE PRECISION,
  pnl_percent DOUBLE PRECISION,
  opened_at TIMESTAMPTZ,
  closed_at TIMESTAMPTZ,
  close_reason TEXT,
  balance_before DOUBLE PRECISION,
  balance_after DOUBLE PRECISION,
  created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders (
  id BIGSERIAL PRIMARY KEY,
  trade_id BIGINT REFERENCES trades(id),
  exchange_order_id TEXT,
  exchange TEXT NOT NULL,
  symbol TEXT NOT NULL,
  side TEXT,
  type TEXT,
  price DOUBLE PRECISION,
  qty DOUBLE PRECISION,
  status TEXT,
  reduce_only BOOLEAN DEFAULT FALSE,
  created_at TIMESTAMPTZ DEFAULT now(),
  updated_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS balance_snapshots (
  id BIGSERIAL PRIMARY KEY,
  exchange TEXT NOT NULL,
  wallet_usdt DOUBLE PRECISION,
  available_usdt DOUBLE PRECISION,
  created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS telegram_events (
  id BIGSERIAL PRIMARY KEY,
  event_type TEXT,
  message TEXT,
  trade_id BIGINT,
  created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS latency_logs (
  id BIGSERIAL PRIMARY KEY,
  exchange TEXT,
  endpoint TEXT,
  request_sent_at TIMESTAMPTZ,
  response_received_at TIMESTAMPTZ,
  latency_ms BIGINT,
  exchange_time_offset_ms BIGINT,
  created_at TIMESTAMPTZ DEFAULT now()
);

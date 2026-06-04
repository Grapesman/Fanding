CREATE TABLE IF NOT EXISTS funding_candidates (
                                                  exchange TEXT NOT NULL,
                                                  symbol TEXT NOT NULL,
                                                  native_symbol TEXT NOT NULL DEFAULT '',

                                                  price DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                  mark_price DOUBLE PRECISION NOT NULL DEFAULT 0,

                                                  funding_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                  funding_interval_hours DOUBLE PRECISION NOT NULL DEFAULT 8,
                                                  next_funding_time TIMESTAMPTZ,

                                                  volume_24h_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,

                                                  bid DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                  ask DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                  spread DOUBLE PRECISION NOT NULL DEFAULT 0,

                                                  raw_fair_price DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                  planned_entry DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                  safe_tp_price DOUBLE PRECISION NOT NULL DEFAULT 0,

                                                  pass_funding BOOLEAN NOT NULL DEFAULT false,
                                                  pass_volume BOOLEAN NOT NULL DEFAULT false,
                                                  pass_spread BOOLEAN NOT NULL DEFAULT true,
                                                  selected BOOLEAN NOT NULL DEFAULT false,

                                                  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    PRIMARY KEY(exchange, symbol)
    );

CREATE INDEX IF NOT EXISTS idx_funding_candidates_exchange_rate
    ON funding_candidates(exchange, funding_rate DESC);

CREATE INDEX IF NOT EXISTS idx_funding_candidates_updated_at
    ON funding_candidates(updated_at DESC);

CREATE TABLE IF NOT EXISTS candidate_overrides (
                                                   exchange TEXT NOT NULL,
                                                   symbol TEXT NOT NULL,
                                                   field TEXT NOT NULL,
                                                   value DOUBLE PRECISION NOT NULL,
                                                   updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(exchange, symbol, field)
    );

CREATE TABLE IF NOT EXISTS current_balances (
                                                exchange TEXT PRIMARY KEY,
                                                wallet_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                available_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                private_ok BOOLEAN NOT NULL DEFAULT false,
                                                error TEXT NOT NULL DEFAULT '',
                                                updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
    );

CREATE TABLE IF NOT EXISTS balance_snapshots (
                                                 id BIGSERIAL PRIMARY KEY,
                                                 exchange TEXT NOT NULL,
                                                 wallet_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                 available_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,
                                                 private_ok BOOLEAN NOT NULL DEFAULT false,
                                                 error TEXT NOT NULL DEFAULT '',
                                                 created_at TIMESTAMPTZ NOT NULL DEFAULT now()
    );

CREATE TABLE IF NOT EXISTS trades (
                                      id BIGSERIAL PRIMARY KEY,
                                      exchange TEXT NOT NULL,
                                      symbol TEXT NOT NULL,
                                      native_symbol TEXT NOT NULL DEFAULT '',
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
    client_order_id TEXT,
    exchange TEXT NOT NULL,
    symbol TEXT NOT NULL,
    native_symbol TEXT NOT NULL DEFAULT '',
    side TEXT,
    type TEXT,
    price DOUBLE PRECISION,
    qty DOUBLE PRECISION,
    status TEXT,
    reduce_only BOOLEAN DEFAULT FALSE,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now()
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
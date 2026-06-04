package storage

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/lib/pq"

	"funding-bot/internal/domain"
)

type Postgres struct {
	db *sql.DB
}

func OpenPostgres(dsn string) (*Postgres, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Postgres{db: db}, nil
}

func (p *Postgres) Close() error {
	if p == nil || p.db == nil {
		return nil
	}
	return p.db.Close()
}

func (p *Postgres) Migrate() error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	_, err := p.db.Exec(schemaSQL)
	return err
}

func (p *Postgres) SaveCandidates(candidates []domain.Candidate) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	overrides, err := p.loadOverridesTx(tx)
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`
		INSERT INTO funding_candidates (
			exchange,
			symbol,
			native_symbol,
			price,
			mark_price,
			funding_rate,
			funding_interval_hours,
			next_funding_time,
			volume_24h_usdt,
			bid,
			ask,
			spread,
			raw_fair_price,
			planned_entry,
			safe_tp_price,
			pass_funding,
			pass_volume,
			pass_spread,
			selected,
			updated_at
		)
		VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,
			$11,$12,$13,$14,$15,$16,$17,$18,$19,$20
		)
		ON CONFLICT(exchange, symbol)
		DO UPDATE SET
			native_symbol = EXCLUDED.native_symbol,
			price = EXCLUDED.price,
			mark_price = EXCLUDED.mark_price,
			funding_rate = EXCLUDED.funding_rate,
			funding_interval_hours = EXCLUDED.funding_interval_hours,
			next_funding_time = EXCLUDED.next_funding_time,
			volume_24h_usdt = EXCLUDED.volume_24h_usdt,
			bid = EXCLUDED.bid,
			ask = EXCLUDED.ask,
			spread = EXCLUDED.spread,
			raw_fair_price = EXCLUDED.raw_fair_price,
			planned_entry = EXCLUDED.planned_entry,
			safe_tp_price = EXCLUDED.safe_tp_price,
			pass_funding = EXCLUDED.pass_funding,
			pass_volume = EXCLUDED.pass_volume,
			pass_spread = EXCLUDED.pass_spread,
			selected = EXCLUDED.selected,
			updated_at = EXCLUDED.updated_at
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UTC()

	for _, c := range candidates {
		if c.FundingIntervalHours <= 0 {
			c.FundingIntervalHours = 8
		}
		if c.UpdatedAt.IsZero() {
			c.UpdatedAt = now
		}
		if c.NativeSymbol == "" {
			c.NativeSymbol = c.Symbol
		}

		c = applyOverrideMap(c, overrides)

		_, err := stmt.Exec(
			string(c.Exchange),
			c.Symbol,
			c.NativeSymbol,
			c.Price,
			c.MarkPrice,
			c.FundingRate,
			c.FundingIntervalHours,
			nullableTime(c.NextFundingTime),
			c.Volume24hUSDT,
			c.Bid,
			c.Ask,
			c.Spread,
			c.RawFairPrice,
			c.PlannedEntry,
			c.SafeTPPrice,
			c.PassFunding,
			c.PassVolume,
			c.PassSpread,
			c.Selected,
			c.UpdatedAt,
		)
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (p *Postgres) Candidates() ([]domain.Candidate, error) {
	if p == nil || p.db == nil {
		return nil, fmt.Errorf("postgres is nil")
	}

	rows, err := p.db.Query(`
		SELECT
			exchange,
			symbol,
			native_symbol,
			price,
			mark_price,
			funding_rate,
			funding_interval_hours,
			next_funding_time,
			volume_24h_usdt,
			bid,
			ask,
			spread,
			raw_fair_price,
			planned_entry,
			safe_tp_price,
			pass_funding,
			pass_volume,
			pass_spread,
			selected,
			updated_at
		FROM funding_candidates
		ORDER BY exchange ASC, funding_rate DESC, volume_24h_usdt DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.Candidate{}

	for rows.Next() {
		var c domain.Candidate
		var ex string
		var nextFunding sql.NullTime
		var updatedAt sql.NullTime

		err := rows.Scan(
			&ex,
			&c.Symbol,
			&c.NativeSymbol,
			&c.Price,
			&c.MarkPrice,
			&c.FundingRate,
			&c.FundingIntervalHours,
			&nextFunding,
			&c.Volume24hUSDT,
			&c.Bid,
			&c.Ask,
			&c.Spread,
			&c.RawFairPrice,
			&c.PlannedEntry,
			&c.SafeTPPrice,
			&c.PassFunding,
			&c.PassVolume,
			&c.PassSpread,
			&c.Selected,
			&updatedAt,
		)
		if err != nil {
			return nil, err
		}

		c.Exchange = domain.ExchangeName(ex)

		if nextFunding.Valid {
			c.NextFundingTime = nextFunding.Time
		}
		if updatedAt.Valid {
			c.UpdatedAt = updatedAt.Time
		}

		if c.FundingIntervalHours <= 0 {
			c.FundingIntervalHours = 8
		}
		if c.NativeSymbol == "" {
			c.NativeSymbol = c.Symbol
		}

		out = append(out, c)
	}

	return out, rows.Err()
}

func (p *Postgres) SetCandidateOverride(exchange domain.ExchangeName, symbol, field string, value float64) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	_, err := p.db.Exec(`
		INSERT INTO candidate_overrides(exchange, symbol, field, value, updated_at)
		VALUES($1,$2,$3,$4,now())
		ON CONFLICT(exchange, symbol, field)
		DO UPDATE SET
			value = EXCLUDED.value,
			updated_at = now()
	`, string(exchange), symbol, field, value)

	return err
}

func (p *Postgres) SaveBalance(b domain.Balance) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	_, err := p.db.Exec(`
		INSERT INTO current_balances(
			exchange,
			wallet_usdt,
			available_usdt,
			private_ok,
			error,
			updated_at
		)
		VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(exchange)
		DO UPDATE SET
			wallet_usdt = EXCLUDED.wallet_usdt,
			available_usdt = EXCLUDED.available_usdt,
			private_ok = EXCLUDED.private_ok,
			error = EXCLUDED.error,
			updated_at = EXCLUDED.updated_at
	`, string(b.Exchange), b.WalletUSDT, b.AvailableUSDT, b.PrivateOK, b.Error, time.Now().UTC())
	if err != nil {
		return err
	}

	_, _ = p.db.Exec(`
		INSERT INTO balance_snapshots(
			exchange,
			wallet_usdt,
			available_usdt,
			private_ok,
			error,
			created_at
		)
		VALUES($1,$2,$3,$4,$5,now())
	`, string(b.Exchange), b.WalletUSDT, b.AvailableUSDT, b.PrivateOK, b.Error)

	return nil
}

func (p *Postgres) loadOverridesTx(tx *sql.Tx) (map[string]CandidateOverride, error) {
	rows, err := tx.Query(`
		SELECT exchange, symbol, field, value, updated_at
		FROM candidate_overrides
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]CandidateOverride{}

	for rows.Next() {
		var o CandidateOverride
		var ex string

		if err := rows.Scan(&ex, &o.Symbol, &o.Field, &o.Value, &o.UpdatedAt); err != nil {
			return nil, err
		}

		o.Exchange = domain.ExchangeName(ex)
		out[overrideKey(o.Exchange, o.Symbol, o.Field)] = o
	}

	return out, rows.Err()
}

func applyOverrideMap(c domain.Candidate, overrides map[string]CandidateOverride) domain.Candidate {
	if o, ok := overrides[overrideKey(c.Exchange, c.Symbol, "planned_entry")]; ok {
		c.PlannedEntry = o.Value
	}
	if o, ok := overrides[overrideKey(c.Exchange, c.Symbol, "safe_tp_price")]; ok {
		c.SafeTPPrice = o.Value
	}
	return c
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

const schemaSQL = `
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
`

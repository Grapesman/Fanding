package storage

import (
	"database/sql"
	"encoding/json"
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
			can_trade_live,
			live_reject_reason,
			min_order_usdt,
			estimated_order_qty,
			estimated_order_usdt,
			updated_at
		)
		VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,
			$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,
			$21,$22,$23,$24,$25
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
			can_trade_live = EXCLUDED.can_trade_live,
			live_reject_reason = EXCLUDED.live_reject_reason,
			min_order_usdt = EXCLUDED.min_order_usdt,
			estimated_order_qty = EXCLUDED.estimated_order_qty,
			estimated_order_usdt = EXCLUDED.estimated_order_usdt,
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
			c.CanTradeLive,
			c.LiveRejectReason,
			c.MinOrderUSDT,
			c.EstimatedOrderQty,
			c.EstimatedOrderUSDT,
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
			can_trade_live,
			live_reject_reason,
			min_order_usdt,
			estimated_order_qty,
			estimated_order_usdt,
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
			&c.CanTradeLive,
			&c.LiveRejectReason,
			&c.MinOrderUSDT,
			&c.EstimatedOrderQty,
			&c.EstimatedOrderUSDT,
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

	now := time.Now().UTC()

	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = now
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
	`, string(b.Exchange), b.WalletUSDT, b.AvailableUSDT, b.PrivateOK, b.Error, b.UpdatedAt)
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

func (p *Postgres) CurrentBalances() (map[domain.ExchangeName]domain.Balance, error) {
	if p == nil || p.db == nil {
		return nil, fmt.Errorf("postgres is nil")
	}

	rows, err := p.db.Query(`
		SELECT exchange, wallet_usdt, available_usdt, private_ok, error, updated_at
		FROM current_balances
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[domain.ExchangeName]domain.Balance{}

	for rows.Next() {
		var b domain.Balance
		var ex string

		if err := rows.Scan(&ex, &b.WalletUSDT, &b.AvailableUSDT, &b.PrivateOK, &b.Error, &b.UpdatedAt); err != nil {
			return nil, err
		}

		b.Exchange = domain.ExchangeName(ex)
		out[b.Exchange] = b
	}

	return out, rows.Err()
}

func (p *Postgres) SavePlannedTrades(plans []domain.PlannedTrade) error {
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

	for _, plan := range plans {
		if err := savePlannedTradeTx(tx, plan); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (p *Postgres) SavePlannedTrade(plan domain.PlannedTrade) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	return savePlannedTradeDB(p.db, plan)
}

func (p *Postgres) PlannedTrades() ([]domain.PlannedTrade, error) {
	if p == nil || p.db == nil {
		return nil, fmt.Errorf("postgres is nil")
	}

	rows, err := p.db.Query(`
		SELECT payload
		FROM planned_trades
		WHERE status NOT IN ('CLOSED', 'CANCELLED', 'SKIPPED', 'ERROR')
		ORDER BY funding_time ASC, exchange ASC, funding_rate DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.PlannedTrade{}

	for rows.Next() {
		var raw []byte
		var p domain.PlannedTrade

		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}

		out = append(out, p)
	}

	return out, rows.Err()
}

func (p *Postgres) UpdatePlannedTradeStatus(planID string, status domain.TradeStatus, errText string) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	row := p.db.QueryRow(`SELECT payload FROM planned_trades WHERE plan_id=$1`, planID)

	var raw []byte
	var plan domain.PlannedTrade

	if err := row.Scan(&raw); err != nil {
		return err
	}

	if err := json.Unmarshal(raw, &plan); err != nil {
		return err
	}

	plan.Status = status
	plan.UpdatedAt = time.Now().UTC()

	if errText != "" {
		plan.LiveRejectReason = errText
	}

	return savePlannedTradeDB(p.db, plan)
}

func (p *Postgres) DeletePlannedTrade(planID string) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	_, err := p.db.Exec(`DELETE FROM planned_trades WHERE plan_id=$1`, planID)

	return err
}

func (p *Postgres) SaveActiveTrade(t domain.ActiveTrade) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	return saveActiveTradeDB(p.db, t)
}

func (p *Postgres) ActiveTrades() ([]domain.ActiveTrade, error) {
	if p == nil || p.db == nil {
		return nil, fmt.Errorf("postgres is nil")
	}

	rows, err := p.db.Query(`
		SELECT payload
		FROM active_trades
		WHERE status NOT IN ('CLOSED', 'CANCELLED', 'ERROR')
		ORDER BY opened_at DESC, updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.ActiveTrade{}

	for rows.Next() {
		var raw []byte
		var t domain.ActiveTrade

		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}

		out = append(out, t)
	}

	return out, rows.Err()
}

func (p *Postgres) SaveClosedTrade(t domain.ClosedTrade) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	return saveClosedTradeDB(p.db, t)
}

func (p *Postgres) ClosedTrades() ([]domain.ClosedTrade, error) {
	if p == nil || p.db == nil {
		return nil, fmt.Errorf("postgres is nil")
	}

	rows, err := p.db.Query(`
		SELECT payload
		FROM closed_trades
		ORDER BY closed_at DESC, created_at DESC
		LIMIT 5000
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.ClosedTrade{}

	for rows.Next() {
		var raw []byte
		var t domain.ClosedTrade

		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}

		out = append(out, t)
	}

	return out, rows.Err()
}

func (p *Postgres) CloseActiveTrade(tradeID string, closed domain.ClosedTrade) error {
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

	var activeRaw []byte

	err = tx.QueryRow(`SELECT payload FROM active_trades WHERE trade_id=$1`, tradeID).Scan(&activeRaw)
	if err != nil && err != sql.ErrNoRows {
		return err
	}

	if err == nil && closed.ActiveTrade.TradeID == "" {
		var active domain.ActiveTrade
		if err := json.Unmarshal(activeRaw, &active); err != nil {
			return err
		}
		closed.ActiveTrade = active
	}

	if closed.TradeID == "" {
		closed.TradeID = tradeID
	}
	if closed.Status == "" {
		closed.Status = domain.TradeClosed
	}
	if closed.ClosedAt.IsZero() {
		closed.ClosedAt = time.Now().UTC()
	}
	if closed.UpdatedAt.IsZero() {
		closed.UpdatedAt = time.Now().UTC()
	}

	if err := saveClosedTradeTx(tx, closed); err != nil {
		return err
	}

	if _, err := tx.Exec(`DELETE FROM active_trades WHERE trade_id=$1`, tradeID); err != nil {
		return err
	}

	return tx.Commit()
}

func (p *Postgres) TradeJournal() ([]domain.ClosedTrade, error) {
	return p.ClosedTrades()
}

func (p *Postgres) SaveOrder(tradeID string, r domain.OrderResult) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("postgres is nil")
	}

	raw, _ := json.Marshal(r)

	_, err := p.db.Exec(`
		INSERT INTO orders(
			trade_key,
			exchange_order_id,
			client_order_id,
			exchange,
			symbol,
			native_symbol,
			side,
			type,
			price,
			avg_price,
			qty,
			filled_qty,
			filled_notional,
			status,
			order_status,
			reduce_only,
			fee,
			fee_asset,
			raw_payload,
			created_at,
			updated_at
		)
		VALUES(
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,
			$11,$12,$13,$14,$15,$16,$17,$18,$19,
			COALESCE(NULLIF($20::timestamptz, NULL), now()),
			now()
		)
		ON CONFLICT(exchange, exchange_order_id)
		DO UPDATE SET
			client_order_id = EXCLUDED.client_order_id,
			status = EXCLUDED.status,
			order_status = EXCLUDED.order_status,
			price = EXCLUDED.price,
			avg_price = EXCLUDED.avg_price,
			qty = EXCLUDED.qty,
			filled_qty = EXCLUDED.filled_qty,
			filled_notional = EXCLUDED.filled_notional,
			reduce_only = EXCLUDED.reduce_only,
			fee = EXCLUDED.fee,
			fee_asset = EXCLUDED.fee_asset,
			raw_payload = EXCLUDED.raw_payload,
			updated_at = now()
	`,
		tradeID,
		r.ExchangeOrderID,
		r.ClientOrderID,
		string(r.Exchange),
		r.Symbol,
		r.NativeSymbol,
		r.Side,
		r.Type,
		r.Price,
		r.AvgPrice,
		r.Qty,
		r.FilledQty,
		r.FilledNotional,
		r.Status,
		string(r.OrderStatus),
		r.ReduceOnly,
		r.Fee,
		r.FeeAsset,
		string(raw),
		nullableTime(r.CreatedAt),
	)

	return err
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

func savePlannedTradeDB(db *sql.DB, plan domain.PlannedTrade) error {
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}

	if plan.ID == "" {
		return fmt.Errorf("planned trade id is empty")
	}
	if plan.UpdatedAt.IsZero() {
		plan.UpdatedAt = time.Now().UTC()
	}

	_, err = db.Exec(`
		INSERT INTO planned_trades(
			plan_id,
			exchange,
			symbol,
			native_symbol,
			status,
			scenario,
			funding_rate,
			funding_time,
			position_usdt,
			payload,
			planned_at,
			updated_at
		)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT(plan_id)
		DO UPDATE SET
			exchange = EXCLUDED.exchange,
			symbol = EXCLUDED.symbol,
			native_symbol = EXCLUDED.native_symbol,
			status = EXCLUDED.status,
			scenario = EXCLUDED.scenario,
			funding_rate = EXCLUDED.funding_rate,
			funding_time = EXCLUDED.funding_time,
			position_usdt = EXCLUDED.position_usdt,
			payload = EXCLUDED.payload,
			planned_at = EXCLUDED.planned_at,
			updated_at = EXCLUDED.updated_at
	`,
		plan.ID,
		string(plan.Exchange),
		plan.Symbol,
		plan.NativeSymbol,
		string(plan.Status),
		string(plan.Scenario),
		plan.FundingRate,
		nullableTime(plan.FundingTime),
		plan.PositionUSDT,
		string(raw),
		nullableTime(plan.PlannedAt),
		plan.UpdatedAt,
	)

	return err
}

func savePlannedTradeTx(tx *sql.Tx, plan domain.PlannedTrade) error {
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}

	if plan.ID == "" {
		return fmt.Errorf("planned trade id is empty")
	}
	if plan.UpdatedAt.IsZero() {
		plan.UpdatedAt = time.Now().UTC()
	}

	_, err = tx.Exec(`
		INSERT INTO planned_trades(
			plan_id,
			exchange,
			symbol,
			native_symbol,
			status,
			scenario,
			funding_rate,
			funding_time,
			position_usdt,
			payload,
			planned_at,
			updated_at
		)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT(plan_id)
		DO UPDATE SET
			exchange = EXCLUDED.exchange,
			symbol = EXCLUDED.symbol,
			native_symbol = EXCLUDED.native_symbol,
			status = EXCLUDED.status,
			scenario = EXCLUDED.scenario,
			funding_rate = EXCLUDED.funding_rate,
			funding_time = EXCLUDED.funding_time,
			position_usdt = EXCLUDED.position_usdt,
			payload = EXCLUDED.payload,
			planned_at = EXCLUDED.planned_at,
			updated_at = EXCLUDED.updated_at
	`,
		plan.ID,
		string(plan.Exchange),
		plan.Symbol,
		plan.NativeSymbol,
		string(plan.Status),
		string(plan.Scenario),
		plan.FundingRate,
		nullableTime(plan.FundingTime),
		plan.PositionUSDT,
		string(raw),
		nullableTime(plan.PlannedAt),
		plan.UpdatedAt,
	)

	return err
}

func saveActiveTradeDB(db *sql.DB, t domain.ActiveTrade) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}

	tradeID := t.TradeID
	if tradeID == "" {
		tradeID = t.PlanID
	}
	if tradeID == "" {
		return fmt.Errorf("active trade id is empty")
	}

	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now().UTC()
	}

	_, err = db.Exec(`
		INSERT INTO active_trades(
			trade_id,
			plan_id,
			exchange,
			symbol,
			native_symbol,
			status,
			scenario,
			side,
			entry_price,
			qty,
			funding_rate,
			funding_time,
			position_usdt,
			unrealized_pnl,
			payload,
			opened_at,
			updated_at
		)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		ON CONFLICT(trade_id)
		DO UPDATE SET
			plan_id = EXCLUDED.plan_id,
			exchange = EXCLUDED.exchange,
			symbol = EXCLUDED.symbol,
			native_symbol = EXCLUDED.native_symbol,
			status = EXCLUDED.status,
			scenario = EXCLUDED.scenario,
			side = EXCLUDED.side,
			entry_price = EXCLUDED.entry_price,
			qty = EXCLUDED.qty,
			funding_rate = EXCLUDED.funding_rate,
			funding_time = EXCLUDED.funding_time,
			position_usdt = EXCLUDED.position_usdt,
			unrealized_pnl = EXCLUDED.unrealized_pnl,
			payload = EXCLUDED.payload,
			opened_at = EXCLUDED.opened_at,
			updated_at = EXCLUDED.updated_at
	`,
		tradeID,
		t.PlanID,
		string(t.Exchange),
		t.Symbol,
		t.NativeSymbol,
		string(t.Status),
		string(t.Scenario),
		string(t.Side),
		firstNonZero(t.EntryAvgPrice, t.EntryPrice),
		t.Qty,
		t.FundingRate,
		nullableTime(t.FundingTime),
		t.PositionUSDT,
		t.UnrealizedPNL,
		string(raw),
		nullableTime(t.OpenedAt),
		t.UpdatedAt,
	)

	return err
}

func saveClosedTradeDB(db *sql.DB, t domain.ClosedTrade) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}

	tradeID := t.TradeID
	if tradeID == "" {
		tradeID = t.PlanID
	}
	if tradeID == "" {
		return fmt.Errorf("closed trade id is empty")
	}

	if t.ClosedAt.IsZero() {
		t.ClosedAt = time.Now().UTC()
	}

	_, err = db.Exec(`
		INSERT INTO closed_trades(
			trade_id,
			plan_id,
			exchange,
			symbol,
			native_symbol,
			status,
			scenario,
			side,
			entry_price,
			exit_price,
			qty,
			funding_rate,
			funding_time,
			funding_fee,
			commission,
			gross_pnl,
			net_pnl,
			close_reason,
			payload,
			opened_at,
			closed_at,
			created_at
		)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,
			$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,now())
		ON CONFLICT(trade_id)
		DO UPDATE SET
			plan_id = EXCLUDED.plan_id,
			exchange = EXCLUDED.exchange,
			symbol = EXCLUDED.symbol,
			native_symbol = EXCLUDED.native_symbol,
			status = EXCLUDED.status,
			scenario = EXCLUDED.scenario,
			side = EXCLUDED.side,
			entry_price = EXCLUDED.entry_price,
			exit_price = EXCLUDED.exit_price,
			qty = EXCLUDED.qty,
			funding_rate = EXCLUDED.funding_rate,
			funding_time = EXCLUDED.funding_time,
			funding_fee = EXCLUDED.funding_fee,
			commission = EXCLUDED.commission,
			gross_pnl = EXCLUDED.gross_pnl,
			net_pnl = EXCLUDED.net_pnl,
			close_reason = EXCLUDED.close_reason,
			payload = EXCLUDED.payload,
			opened_at = EXCLUDED.opened_at,
			closed_at = EXCLUDED.closed_at
	`,
		tradeID,
		t.PlanID,
		string(t.Exchange),
		t.Symbol,
		t.NativeSymbol,
		string(t.Status),
		string(t.Scenario),
		string(t.Side),
		firstNonZero(t.EntryAvgPrice, t.EntryPrice),
		firstNonZero(t.ExitAvgPrice, t.ExitPrice),
		t.Qty,
		t.FundingRate,
		nullableTime(t.FundingTime),
		t.FundingFee,
		t.Commission,
		t.GrossPNL,
		t.NetPNL,
		string(t.CloseReason),
		string(raw),
		nullableTime(t.OpenedAt),
		t.ClosedAt,
	)

	return err
}

func saveClosedTradeTx(tx *sql.Tx, t domain.ClosedTrade) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}

	tradeID := t.TradeID
	if tradeID == "" {
		tradeID = t.PlanID
	}
	if tradeID == "" {
		return fmt.Errorf("closed trade id is empty")
	}

	if t.ClosedAt.IsZero() {
		t.ClosedAt = time.Now().UTC()
	}

	_, err = tx.Exec(`
		INSERT INTO closed_trades(
			trade_id,
			plan_id,
			exchange,
			symbol,
			native_symbol,
			status,
			scenario,
			side,
			entry_price,
			exit_price,
			qty,
			funding_rate,
			funding_time,
			funding_fee,
			commission,
			gross_pnl,
			net_pnl,
			close_reason,
			payload,
			opened_at,
			closed_at,
			created_at
		)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,
			$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,now())
		ON CONFLICT(trade_id)
		DO UPDATE SET
			plan_id = EXCLUDED.plan_id,
			exchange = EXCLUDED.exchange,
			symbol = EXCLUDED.symbol,
			native_symbol = EXCLUDED.native_symbol,
			status = EXCLUDED.status,
			scenario = EXCLUDED.scenario,
			side = EXCLUDED.side,
			entry_price = EXCLUDED.entry_price,
			exit_price = EXCLUDED.exit_price,
			qty = EXCLUDED.qty,
			funding_rate = EXCLUDED.funding_rate,
			funding_time = EXCLUDED.funding_time,
			funding_fee = EXCLUDED.funding_fee,
			commission = EXCLUDED.commission,
			gross_pnl = EXCLUDED.gross_pnl,
			net_pnl = EXCLUDED.net_pnl,
			close_reason = EXCLUDED.close_reason,
			payload = EXCLUDED.payload,
			opened_at = EXCLUDED.opened_at,
			closed_at = EXCLUDED.closed_at
	`,
		tradeID,
		t.PlanID,
		string(t.Exchange),
		t.Symbol,
		t.NativeSymbol,
		string(t.Status),
		string(t.Scenario),
		string(t.Side),
		firstNonZero(t.EntryAvgPrice, t.EntryPrice),
		firstNonZero(t.ExitAvgPrice, t.ExitPrice),
		t.Qty,
		t.FundingRate,
		nullableTime(t.FundingTime),
		t.FundingFee,
		t.Commission,
		t.GrossPNL,
		t.NetPNL,
		string(t.CloseReason),
		string(raw),
		nullableTime(t.OpenedAt),
		t.ClosedAt,
	)

	return err
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

func firstNonZero(values ...float64) float64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}

	return 0
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

  can_trade_live BOOLEAN NOT NULL DEFAULT false,
  live_reject_reason TEXT NOT NULL DEFAULT '',
  min_order_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,
  estimated_order_qty DOUBLE PRECISION NOT NULL DEFAULT 0,
  estimated_order_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,

  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),

  PRIMARY KEY(exchange, symbol)
);

ALTER TABLE funding_candidates ADD COLUMN IF NOT EXISTS native_symbol TEXT NOT NULL DEFAULT '';
ALTER TABLE funding_candidates ADD COLUMN IF NOT EXISTS funding_interval_hours DOUBLE PRECISION NOT NULL DEFAULT 8;
ALTER TABLE funding_candidates ADD COLUMN IF NOT EXISTS next_funding_time TIMESTAMPTZ;
ALTER TABLE funding_candidates ADD COLUMN IF NOT EXISTS can_trade_live BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE funding_candidates ADD COLUMN IF NOT EXISTS live_reject_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE funding_candidates ADD COLUMN IF NOT EXISTS min_order_usdt DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE funding_candidates ADD COLUMN IF NOT EXISTS estimated_order_qty DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE funding_candidates ADD COLUMN IF NOT EXISTS estimated_order_usdt DOUBLE PRECISION NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_funding_candidates_exchange_rate
ON funding_candidates(exchange, funding_rate DESC);

CREATE INDEX IF NOT EXISTS idx_funding_candidates_updated_at
ON funding_candidates(updated_at DESC);

CREATE INDEX IF NOT EXISTS idx_funding_candidates_next_funding_time
ON funding_candidates(next_funding_time ASC);

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

CREATE INDEX IF NOT EXISTS idx_balance_snapshots_exchange_created
ON balance_snapshots(exchange, created_at DESC);

CREATE TABLE IF NOT EXISTS planned_trades (
  plan_id TEXT PRIMARY KEY,
  exchange TEXT NOT NULL,
  symbol TEXT NOT NULL,
  native_symbol TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'PLANNED',
  scenario TEXT NOT NULL DEFAULT 'UNKNOWN',
  funding_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
  funding_time TIMESTAMPTZ,
  position_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,
  payload JSONB NOT NULL,
  planned_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_planned_trades_funding_time
ON planned_trades(funding_time ASC);

CREATE INDEX IF NOT EXISTS idx_planned_trades_exchange_status
ON planned_trades(exchange, status);

CREATE TABLE IF NOT EXISTS active_trades (
  trade_id TEXT PRIMARY KEY,
  plan_id TEXT NOT NULL DEFAULT '',
  exchange TEXT NOT NULL,
  symbol TEXT NOT NULL,
  native_symbol TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'ACTIVE',
  scenario TEXT NOT NULL DEFAULT 'UNKNOWN',
  side TEXT NOT NULL DEFAULT '',
  entry_price DOUBLE PRECISION NOT NULL DEFAULT 0,
  qty DOUBLE PRECISION NOT NULL DEFAULT 0,
  funding_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
  funding_time TIMESTAMPTZ,
  position_usdt DOUBLE PRECISION NOT NULL DEFAULT 0,
  unrealized_pnl DOUBLE PRECISION NOT NULL DEFAULT 0,
  payload JSONB NOT NULL,
  opened_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_active_trades_exchange_status
ON active_trades(exchange, status);

CREATE INDEX IF NOT EXISTS idx_active_trades_funding_time
ON active_trades(funding_time ASC);

CREATE TABLE IF NOT EXISTS closed_trades (
  trade_id TEXT PRIMARY KEY,
  plan_id TEXT NOT NULL DEFAULT '',
  exchange TEXT NOT NULL,
  symbol TEXT NOT NULL,
  native_symbol TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'CLOSED',
  scenario TEXT NOT NULL DEFAULT 'UNKNOWN',
  side TEXT NOT NULL DEFAULT '',
  entry_price DOUBLE PRECISION NOT NULL DEFAULT 0,
  exit_price DOUBLE PRECISION NOT NULL DEFAULT 0,
  qty DOUBLE PRECISION NOT NULL DEFAULT 0,
  funding_rate DOUBLE PRECISION NOT NULL DEFAULT 0,
  funding_time TIMESTAMPTZ,
  funding_fee DOUBLE PRECISION NOT NULL DEFAULT 0,
  commission DOUBLE PRECISION NOT NULL DEFAULT 0,
  gross_pnl DOUBLE PRECISION NOT NULL DEFAULT 0,
  net_pnl DOUBLE PRECISION NOT NULL DEFAULT 0,
  close_reason TEXT NOT NULL DEFAULT '',
  payload JSONB NOT NULL,
  opened_at TIMESTAMPTZ,
  closed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_closed_trades_closed_at
ON closed_trades(closed_at DESC);

CREATE INDEX IF NOT EXISTS idx_closed_trades_exchange_closed_at
ON closed_trades(exchange, closed_at DESC);

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
  trade_key TEXT NOT NULL DEFAULT '',
  exchange_order_id TEXT NOT NULL DEFAULT '',
  client_order_id TEXT NOT NULL DEFAULT '',
  exchange TEXT NOT NULL,
  symbol TEXT NOT NULL,
  native_symbol TEXT NOT NULL DEFAULT '',
  side TEXT,
  type TEXT,
  price DOUBLE PRECISION,
  avg_price DOUBLE PRECISION NOT NULL DEFAULT 0,
  qty DOUBLE PRECISION,
  filled_qty DOUBLE PRECISION NOT NULL DEFAULT 0,
  filled_notional DOUBLE PRECISION NOT NULL DEFAULT 0,
  status TEXT,
  order_status TEXT NOT NULL DEFAULT '',
  reduce_only BOOLEAN DEFAULT FALSE,
  fee DOUBLE PRECISION NOT NULL DEFAULT 0,
  fee_asset TEXT NOT NULL DEFAULT '',
  raw_payload JSONB,
  created_at TIMESTAMPTZ DEFAULT now(),
  updated_at TIMESTAMPTZ DEFAULT now()
);

ALTER TABLE orders ADD COLUMN IF NOT EXISTS trade_key TEXT NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN IF NOT EXISTS avg_price DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS filled_qty DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS filled_notional DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS order_status TEXT NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN IF NOT EXISTS fee DOUBLE PRECISION NOT NULL DEFAULT 0;
ALTER TABLE orders ADD COLUMN IF NOT EXISTS fee_asset TEXT NOT NULL DEFAULT '';
ALTER TABLE orders ADD COLUMN IF NOT EXISTS raw_payload JSONB;

CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_exchange_order_id
ON orders(exchange, exchange_order_id)
WHERE exchange_order_id <> '';

CREATE INDEX IF NOT EXISTS idx_orders_trade_key
ON orders(trade_key);

CREATE TABLE IF NOT EXISTS telegram_events (
  id BIGSERIAL PRIMARY KEY,
  event_type TEXT,
  message TEXT,
  trade_id BIGINT,
  trade_key TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ DEFAULT now()
);

ALTER TABLE telegram_events ADD COLUMN IF NOT EXISTS trade_key TEXT NOT NULL DEFAULT '';

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

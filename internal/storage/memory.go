package storage

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"funding-bot/internal/domain"
)

type CandidateOverride struct {
	Exchange  domain.ExchangeName
	Symbol    string
	Field     string
	Value     float64
	UpdatedAt time.Time
}

type Store struct {
	mu sync.RWMutex

	pg *Postgres

	candidates []domain.Candidate
	balances   map[domain.ExchangeName]domain.Balance
	connected  map[domain.ExchangeName]bool
	lastErrors map[domain.ExchangeName]string

	planned []domain.PlannedTrade
	active  []domain.ActiveTrade
	closed  []domain.ClosedTrade

	nextActiveID int64

	logs      []string
	skipped   map[string]time.Time
	overrides map[string]CandidateOverride
}

func NewStore() *Store {
	return &Store{
		balances:     map[domain.ExchangeName]domain.Balance{},
		connected:    map[domain.ExchangeName]bool{},
		lastErrors:   map[domain.ExchangeName]string{},
		planned:      []domain.PlannedTrade{},
		active:       []domain.ActiveTrade{},
		closed:       []domain.ClosedTrade{},
		nextActiveID: 1,
		logs:         []string{},
		skipped:      map[string]time.Time{},
		overrides:    map[string]CandidateOverride{},
	}
}

func (s *Store) SetPostgres(pg *Postgres) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pg = pg
}

func overrideKey(exchange domain.ExchangeName, symbol, field string) string {
	return string(exchange) + ":" + symbol + ":" + field
}

func skippedKey(exchange domain.ExchangeName, symbol string, fundingTime int64) string {
	return string(exchange) + ":" + symbol + ":" + time.Unix(fundingTime, 0).UTC().Format(time.RFC3339)
}

func plannedKey(exchange domain.ExchangeName, symbol string, fundingTime time.Time) string {
	return string(exchange) + ":" + symbol + ":" + fundingTime.UTC().Format(time.RFC3339)
}

func (s *Store) applyOverridesLocked(c []domain.Candidate) {
	for i := range c {
		if o, ok := s.overrides[overrideKey(c[i].Exchange, c[i].Symbol, "planned_entry")]; ok {
			c[i].PlannedEntry = o.Value
		}

		if o, ok := s.overrides[overrideKey(c[i].Exchange, c[i].Symbol, "safe_tp_price")]; ok {
			c[i].SafeTPPrice = o.Value
		}
	}
}

func (s *Store) SetCandidates(c []domain.Candidate) {
	copyC := append([]domain.Candidate(nil), c...)

	s.AddLog(fmt.Sprintf("SetCandidates called: %d candidates", len(copyC)))

	s.mu.Lock()
	s.applyOverridesLocked(copyC)

	sort.Slice(copyC, func(i, j int) bool {
		if copyC[i].Exchange == copyC[j].Exchange {
			if copyC[i].FundingRate == copyC[j].FundingRate {
				return copyC[i].Volume24hUSDT > copyC[j].Volume24hUSDT
			}

			return copyC[i].FundingRate > copyC[j].FundingRate
		}

		return copyC[i].Exchange < copyC[j].Exchange
	})

	pg := s.pg
	s.candidates = copyC
	s.mu.Unlock()

	if pg != nil {
		if err := pg.SaveCandidates(copyC); err != nil {
			s.AddLog("postgres SaveCandidates error: " + err.Error())
		}
	}
}

func (s *Store) Candidates() []domain.Candidate {
	s.mu.RLock()
	pg := s.pg
	mem := append([]domain.Candidate(nil), s.candidates...)
	s.mu.RUnlock()

	if pg != nil {
		rows, err := pg.Candidates()
		if err == nil {
			return rows
		}

		s.AddLog("postgres Candidates error: " + err.Error())
	}

	return mem
}

func (s *Store) CandidatesByExchange(ex domain.ExchangeName) []domain.Candidate {
	out := []domain.Candidate{}

	for _, c := range s.Candidates() {
		if c.Exchange == ex {
			out = append(out, c)
		}
	}

	return out
}

func (s *Store) SetCandidateOverride(exchange domain.ExchangeName, symbol, field string, value float64) error {
	s.mu.Lock()

	s.overrides[overrideKey(exchange, symbol, field)] = CandidateOverride{
		Exchange:  exchange,
		Symbol:    symbol,
		Field:     field,
		Value:     value,
		UpdatedAt: time.Now(),
	}

	for i := range s.candidates {
		if s.candidates[i].Exchange == exchange && s.candidates[i].Symbol == symbol {
			switch field {
			case "planned_entry":
				s.candidates[i].PlannedEntry = value
			case "safe_tp_price":
				s.candidates[i].SafeTPPrice = value
			}
		}
	}

	pg := s.pg
	s.mu.Unlock()

	if pg != nil {
		if err := pg.SetCandidateOverride(exchange, symbol, field, value); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) SetBalance(b domain.Balance) {
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = time.Now()
	}

	s.mu.Lock()
	s.balances[b.Exchange] = b
	pg := s.pg
	s.mu.Unlock()

	if pg != nil {
		if err := pg.SaveBalance(b); err != nil {
			s.AddLog("postgres SaveBalance error: " + err.Error())
		}
	}
}

func (s *Store) Balances() map[domain.ExchangeName]domain.Balance {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := map[domain.ExchangeName]domain.Balance{}

	for k, v := range s.balances {
		out[k] = v
	}

	return out
}

func (s *Store) Balance(exchange domain.ExchangeName) (domain.Balance, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, ok := s.balances[exchange]

	return b, ok
}

func (s *Store) SetExchangeStatus(ex domain.ExchangeName, connected bool, errText string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.connected[ex] = connected

	if errText == "" {
		delete(s.lastErrors, ex)
	} else {
		s.lastErrors[ex] = errText
	}
}

func (s *Store) ExchangeStatuses() map[domain.ExchangeName]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := map[domain.ExchangeName]bool{}

	for k, v := range s.connected {
		out[k] = v
	}

	return out
}

func (s *Store) ExchangeErrors() map[domain.ExchangeName]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := map[domain.ExchangeName]string{}

	for k, v := range s.lastErrors {
		out[k] = v
	}

	return out
}

func (s *Store) SetPlannedTrades(plans []domain.PlannedTrade) {
	now := time.Now()

	copyPlans := append([]domain.PlannedTrade(nil), plans...)

	for i := range copyPlans {
		if copyPlans[i].Status == "" {
			copyPlans[i].Status = domain.TradePlanned
		}
		if copyPlans[i].PlannedAt.IsZero() {
			copyPlans[i].PlannedAt = now
		}
		if copyPlans[i].UpdatedAt.IsZero() {
			copyPlans[i].UpdatedAt = now
		}
	}

	s.mu.Lock()
	s.planned = copyPlans
	s.mu.Unlock()

	s.AddLog(fmt.Sprintf("planned trades updated: %d", len(copyPlans)))
}

func (s *Store) AddPlannedTrade(plan domain.PlannedTrade) domain.PlannedTrade {
	now := time.Now()

	if plan.Status == "" {
		plan.Status = domain.TradePlanned
	}
	if plan.PlannedAt.IsZero() {
		plan.PlannedAt = now
	}
	if plan.UpdatedAt.IsZero() {
		plan.UpdatedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := plannedKey(plan.Exchange, plan.Symbol, plan.FundingTime)

	for i := range s.planned {
		if plannedKey(s.planned[i].Exchange, s.planned[i].Symbol, s.planned[i].FundingTime) == key {
			s.planned[i] = plan
			return plan
		}
	}

	s.planned = append(s.planned, plan)

	return plan
}

func (s *Store) PlannedTrades() []domain.PlannedTrade {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := append([]domain.PlannedTrade(nil), s.planned...)

	sort.Slice(out, func(i, j int) bool {
		if out[i].FundingTime.Equal(out[j].FundingTime) {
			if out[i].Exchange == out[j].Exchange {
				return out[i].FundingRate > out[j].FundingRate
			}

			return out[i].Exchange < out[j].Exchange
		}

		return out[i].FundingTime.Before(out[j].FundingTime)
	})

	return out
}

func (s *Store) PlannedTradesByExchange(exchange domain.ExchangeName) []domain.PlannedTrade {
	out := []domain.PlannedTrade{}

	for _, p := range s.PlannedTrades() {
		if p.Exchange == exchange {
			out = append(out, p)
		}
	}

	return out
}

func (s *Store) FindPlannedTrade(planID string) (domain.PlannedTrade, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, p := range s.planned {
		if p.ID == planID {
			return p, true
		}
	}

	return domain.PlannedTrade{}, false
}

func (s *Store) HasPlannedOrActive(exchange domain.ExchangeName, symbol string, fundingTime time.Time) bool {
	key := plannedKey(exchange, symbol, fundingTime)

	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, p := range s.planned {
		if p.Status != domain.TradeClosed &&
			p.Status != domain.TradeCancelled &&
			p.Status != domain.TradeSkipped &&
			p.Status != domain.TradeError &&
			plannedKey(p.Exchange, p.Symbol, p.FundingTime) == key {
			return true
		}
	}

	for _, a := range s.active {
		if a.Status != domain.TradeClosed &&
			a.Status != domain.TradeFinish &&
			a.Status != domain.TradeCancelled &&
			a.Status != domain.TradeError &&
			plannedKey(a.Exchange, a.Symbol, a.FundingTime) == key {
			return true
		}
	}

	return false
}

func (s *Store) UpdatePlannedTradeStatus(planID string, status domain.TradeStatus, errText string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.planned {
		if s.planned[i].ID == planID {
			s.planned[i].Status = status
			s.planned[i].UpdatedAt = time.Now()

			if errText != "" {
				s.planned[i].LiveRejectReason = errText
			}

			return true
		}
	}

	return false
}

func (s *Store) RemovePlannedTrade(planID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.planned {
		if s.planned[i].ID == planID {
			s.planned = append(s.planned[:i], s.planned[i+1:]...)
			return true
		}
	}

	return false
}

func (s *Store) PromotePlannedToActive(plan domain.PlannedTrade, active domain.ActiveTrade) domain.ActiveTrade {
	now := time.Now()

	if active.ID == 0 {
		s.mu.Lock()
		active.ID = s.nextActiveID
		s.nextActiveID++
		s.mu.Unlock()
	}

	if active.PlanID == "" {
		active.PlanID = plan.ID
	}
	if active.TradeID == "" {
		active.TradeID = plan.ID
	}
	if active.Exchange == "" {
		active.Exchange = plan.Exchange
	}
	if active.Symbol == "" {
		active.Symbol = plan.Symbol
	}
	if active.NativeSymbol == "" {
		active.NativeSymbol = plan.NativeSymbol
	}
	if active.Scenario == "" {
		active.Scenario = plan.Scenario
	}
	if active.Status == "" {
		active.Status = domain.TradeActive
	}
	if active.FundingRate == 0 {
		active.FundingRate = plan.FundingRate
	}
	if active.FundingIntervalHours == 0 {
		active.FundingIntervalHours = plan.FundingIntervalHours
	}
	if active.FundingTime.IsZero() {
		active.FundingTime = plan.FundingTime
	}
	if active.PositionUSDT == 0 {
		active.PositionUSDT = plan.PositionUSDT
	}
	if active.Leverage == 0 {
		active.Leverage = plan.Leverage
	}
	if active.MarginMode == "" {
		active.MarginMode = plan.MarginMode
	}
	if active.PreFundingMarkPrice == 0 {
		active.PreFundingMarkPrice = plan.PreFundingMarkPrice
	}
	if active.FairPrice == 0 {
		active.FairPrice = plan.FairPrice
	}
	if active.Scenario2ExtraShift == 0 {
		active.Scenario2ExtraShift = plan.Scenario2ExtraShift
	}
	if active.Scenario2TPPrice == 0 {
		active.Scenario2TPPrice = plan.Scenario2TPPrice
	}
	if active.Scenario3Offset == 0 {
		active.Scenario3Offset = plan.Scenario3Offset
	}
	if active.Scenario3BuyPrice == 0 {
		active.Scenario3BuyPrice = plan.Scenario3BuyPrice
	}
	if active.Scenario3SellPrice == 0 {
		active.Scenario3SellPrice = plan.Scenario3SellPrice
	}
	if active.OpenedAt.IsZero() {
		active.OpenedAt = now
	}
	if active.UpdatedAt.IsZero() {
		active.UpdatedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.planned {
		if s.planned[i].ID == plan.ID {
			s.planned = append(s.planned[:i], s.planned[i+1:]...)
			break
		}
	}

	replaced := false

	for i := range s.active {
		if s.active[i].TradeID != "" && s.active[i].TradeID == active.TradeID {
			s.active[i] = active
			replaced = true
			break
		}
	}

	if !replaced {
		s.active = append(s.active, active)
	}

	return active
}

func (s *Store) AddActiveTrade(t domain.ActiveTrade) domain.ActiveTrade {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if t.ID == 0 {
		t.ID = s.nextActiveID
		s.nextActiveID++
	}

	if t.Status == "" {
		t.Status = domain.TradeActive
	}
	if t.OpenedAt.IsZero() {
		t.OpenedAt = now
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}
	if t.TradeID == "" {
		t.TradeID = fmt.Sprintf("%s:%s:%d", t.Exchange, t.Symbol, t.ID)
	}

	for i := range s.active {
		if s.active[i].TradeID == t.TradeID {
			s.active[i] = t
			return t
		}
	}

	s.active = append(s.active, t)

	return t
}

func (s *Store) ActiveTrades() []domain.ActiveTrade {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := append([]domain.ActiveTrade(nil), s.active...)

	sort.Slice(out, func(i, j int) bool {
		return out[i].OpenedAt.After(out[j].OpenedAt)
	})

	return out
}

func (s *Store) FindActiveTrade(tradeID string) (domain.ActiveTrade, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.active {
		if t.TradeID == tradeID || t.PlanID == tradeID {
			return t, true
		}
	}

	return domain.ActiveTrade{}, false
}

func (s *Store) FindActiveTradeByID(id int64) (domain.ActiveTrade, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, t := range s.active {
		if t.ID == id {
			return t, true
		}
	}

	return domain.ActiveTrade{}, false
}

func (s *Store) UpdateActiveTrade(t domain.ActiveTrade) bool {
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.active {
		if (t.TradeID != "" && s.active[i].TradeID == t.TradeID) || (t.ID != 0 && s.active[i].ID == t.ID) {
			s.active[i] = t
			return true
		}
	}

	return false
}

func (s *Store) UpdateActiveTradeStatus(tradeID string, status domain.TradeStatus, errText string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.active {
		if s.active[i].TradeID == tradeID || s.active[i].PlanID == tradeID {
			s.active[i].Status = status
			s.active[i].UpdatedAt = time.Now()

			if errText != "" {
				s.active[i].LastError = errText
			}

			return true
		}
	}

	return false
}

func (s *Store) UpdateActiveTradePNL(tradeID string, currentPrice, markPrice, unrealizedPNL float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.active {
		if s.active[i].TradeID == tradeID || s.active[i].PlanID == tradeID {
			s.active[i].CurrentPrice = currentPrice
			s.active[i].MarkPrice = markPrice
			s.active[i].UnrealizedPNL = unrealizedPNL
			s.active[i].UpdatedAt = time.Now()

			return true
		}
	}

	return false
}

func (s *Store) MarkManualAlertSent(tradeID string) bool {
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.active {
		if s.active[i].TradeID == tradeID || s.active[i].PlanID == tradeID {
			s.active[i].Status = domain.TradeManualAlert
			s.active[i].ManualAlertSentAt = &now
			s.active[i].UpdatedAt = now

			return true
		}
	}

	return false
}

func (s *Store) CloseActiveTrade(tradeID string, closed domain.ClosedTrade) bool {
	now := time.Now()

	if closed.ClosedAt.IsZero() {
		closed.ClosedAt = now
	}
	if closed.Status == "" {
		closed.Status = domain.TradeClosed
	}
	if closed.UpdatedAt.IsZero() {
		closed.UpdatedAt = now
	}
	if closed.TradeID == "" {
		closed.TradeID = tradeID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.active {
		if s.active[i].TradeID == tradeID || s.active[i].PlanID == tradeID {
			if closed.ActiveTrade.ID == 0 {
				closed.ActiveTrade = s.active[i]
			}

			closed.Status = domain.TradeClosed
			closed.ClosedAt = now
			closed.UpdatedAt = now

			s.active = append(s.active[:i], s.active[i+1:]...)
			s.closed = append([]domain.ClosedTrade{closed}, s.closed...)

			if len(s.closed) > 5000 {
				s.closed = s.closed[:5000]
			}

			return true
		}
	}

	return false
}

func (s *Store) AddClosedTrade(t domain.ClosedTrade) {
	now := time.Now()

	if t.ClosedAt.IsZero() {
		t.ClosedAt = now
	}
	if t.Status == "" {
		t.Status = domain.TradeClosed
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = append([]domain.ClosedTrade{t}, s.closed...)

	if len(s.closed) > 5000 {
		s.closed = s.closed[:5000]
	}
}

func (s *Store) ClosedTrades() []domain.ClosedTrade {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := append([]domain.ClosedTrade(nil), s.closed...)

	sort.Slice(out, func(i, j int) bool {
		return out[i].ClosedAt.After(out[j].ClosedAt)
	})

	return out
}

func (s *Store) TradeJournal() []domain.ClosedTrade {
	return s.ClosedTrades()
}

func (s *Store) AddLog(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.logs = append([]string{time.Now().Format(time.RFC3339) + " " + msg}, s.logs...)

	if len(s.logs) > 500 {
		s.logs = s.logs[:500]
	}
}

func (s *Store) Logs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return append([]string(nil), s.logs...)
}

func (s *Store) MarkSkipped(exchange domain.ExchangeName, symbol string, fundingTime int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.skipped[skippedKey(exchange, symbol, fundingTime)] = time.Now()
}

func (s *Store) IsSkipped(exchange domain.ExchangeName, symbol string, fundingUnix int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, ok := s.skipped[skippedKey(exchange, symbol, fundingUnix)]

	return ok
}

func (s *Store) CleanupOldPlans(before time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	kept := s.planned[:0]
	removed := 0

	for _, p := range s.planned {
		if !p.FundingTime.IsZero() && p.FundingTime.Before(before) {
			removed++
			continue
		}

		kept = append(kept, p)
	}

	s.planned = kept

	return removed
}

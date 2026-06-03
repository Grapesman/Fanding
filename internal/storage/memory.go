package storage

import (
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
	mu         sync.RWMutex
	candidates []domain.Candidate
	balances   map[domain.ExchangeName]domain.Balance
	connected  map[domain.ExchangeName]bool
	lastErrors map[domain.ExchangeName]string
	active     []domain.ActiveTrade
	closed     []domain.ClosedTrade
	logs       []string
	skipped    map[string]time.Time
	overrides  map[string]CandidateOverride
}

func NewStore() *Store {
	return &Store{
		balances:   map[domain.ExchangeName]domain.Balance{},
		connected:  map[domain.ExchangeName]bool{},
		lastErrors: map[domain.ExchangeName]string{},
		logs:       []string{},
		skipped:    map[string]time.Time{},
		overrides:  map[string]CandidateOverride{},
	}
}

func overrideKey(exchange domain.ExchangeName, symbol, field string) string {
	return string(exchange) + ":" + symbol + ":" + field
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
	s.mu.Lock()
	defer s.mu.Unlock()
	copyC := append([]domain.Candidate(nil), c...)
	s.applyOverridesLocked(copyC)
	s.candidates = copyC
	sort.Slice(s.candidates, func(i, j int) bool {
		if s.candidates[i].Exchange == s.candidates[j].Exchange {
			return s.candidates[i].FundingRate > s.candidates[j].FundingRate
		}
		return s.candidates[i].Exchange < s.candidates[j].Exchange
	})
}
func (s *Store) Candidates() []domain.Candidate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.Candidate(nil), s.candidates...)
}
func (s *Store) CandidatesByExchange(ex domain.ExchangeName) []domain.Candidate {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []domain.Candidate{}
	for _, c := range s.candidates {
		if c.Exchange == ex {
			out = append(out, c)
		}
	}
	return out
}
func (s *Store) SetCandidateOverride(exchange domain.ExchangeName, symbol, field string, value float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overrides[overrideKey(exchange, symbol, field)] = CandidateOverride{Exchange: exchange, Symbol: symbol, Field: field, Value: value, UpdatedAt: time.Now()}
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
	return nil
}
func (s *Store) SetBalance(b domain.Balance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balances[b.Exchange] = b
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
func (s *Store) ActiveTrades() []domain.ActiveTrade {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.ActiveTrade(nil), s.active...)
}
func (s *Store) ClosedTrades() []domain.ClosedTrade {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]domain.ClosedTrade(nil), s.closed...)
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
	s.skipped[string(exchange)+":"+symbol+":"+time.Unix(fundingTime, 0).Format(time.RFC3339)] = time.Now()
}
func (s *Store) IsSkipped(exchange domain.ExchangeName, symbol string, fundingUnix int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.skipped[string(exchange)+":"+symbol+":"+time.Unix(fundingUnix, 0).Format(time.RFC3339)]
	return ok
}

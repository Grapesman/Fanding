package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"funding-bot/internal/config"
	"funding-bot/internal/domain"
	"funding-bot/internal/storage"
)

type Server struct {
	cfg   config.Config
	store *storage.Store
	mux   *http.ServeMux
}

type stateResponse struct {
	Mode       string                 `json:"mode"`
	ServerTime time.Time              `json:"server_time"`
	Exchanges  []domain.ExchangeState `json:"exchanges"`
	Active     []domain.ActiveTrade   `json:"active"`
	Closed     []domain.ClosedTrade   `json:"closed"`
	Settings   map[string]any         `json:"settings"`
	Logs       []string               `json:"logs"`
}

func New(cfg config.Config, store *storage.Store) *Server {
	s := &Server{cfg: cfg, store: store, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/candidate/override", s.handleCandidateOverride)
	s.mux.HandleFunc("/api/close-all", s.handleCloseAll)
}

func (s *Server) Addr() string { return s.cfg.DashboardHost + ":" + s.cfg.DashboardPort }
func (s *Server) Run() error   { return http.ListenAndServe(s.Addr(), s.mux) }

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join("web", "index.html")
	http.ServeFile(w, r, p)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	candidates := s.store.Candidates()
	balances := s.store.Balances()
	statuses := s.store.ExchangeStatuses()
	errors := s.store.ExchangeErrors()

	byExchange := map[domain.ExchangeName][]domain.Candidate{}
	for _, c := range candidates {
		byExchange[c.Exchange] = append(byExchange[c.Exchange], c)
	}

	exchanges := make([]domain.ExchangeState, 0, len(domain.AllExchangeNames))
	for _, name := range domain.AllExchangeNames {
		bal := balances[name]
		if bal.Exchange == "" {
			bal = domain.Balance{Exchange: name, UpdatedAt: time.Now()}
		}
		errText := errors[name]
		exchanges = append(exchanges, domain.ExchangeState{
			Name:       name,
			Enabled:    s.exchangeEnabled(name),
			Connected:  statuses[name],
			PublicOK:   statuses[name] && errText == "",
			PrivateOK:  bal.PrivateOK,
			Balance:    bal,
			Error:      errText,
			Candidates: byExchange[name],
			UpdatedAt:  time.Now(),
		})
	}

	resp := stateResponse{
		Mode:       string(s.cfg.BotMode),
		ServerTime: time.Now().UTC(),
		Exchanges:  exchanges,
		Active:     s.store.ActiveTrades(),
		Closed:     s.store.ClosedTrades(),
		Settings: map[string]any{
			"min_funding_rate_percent": s.cfg.MinFundingRate * 100,
			"min_24h_volume_usdt":      s.cfg.Min24hVolumeUSDT,
			"use_spread_filter":        s.cfg.UseSpreadFilter,
			"max_spread_percent":       s.cfg.MaxSpread * 100,
			"usdt_per_trade":           s.cfg.USDTPerTrade,
			"leverage":                 s.cfg.Leverage,
			"margin_mode":              s.cfg.MarginMode,
			"dashboard_url":            s.cfg.DashboardPublicURL,
			"allow_mainnet_live":       s.cfg.AllowMainnetLive,
		},
		Logs: s.store.Logs(),
	}
	writeJSON(w, resp)
}

func (s *Server) exchangeEnabled(name domain.ExchangeName) bool {
	switch name {
	case domain.ExchangeBinance:
		return s.cfg.Binance.Enabled
	case domain.ExchangeBybit:
		return s.cfg.Bybit.Enabled
	case domain.ExchangeBingX:
		return s.cfg.BingX.Enabled
	case domain.ExchangeBitget:
		return s.cfg.Bitget.Enabled
	case domain.ExchangeOKX:
		return s.cfg.OKX.Enabled
	case domain.ExchangeGate:
		return s.cfg.Gate.Enabled
	case domain.ExchangeMEXC:
		return s.cfg.MEXC.Enabled
	case domain.ExchangeKuCoin:
		return s.cfg.KuCoin.Enabled
	case domain.ExchangeHTX:
		return s.cfg.HTX.Enabled
	default:
		return false
	}
}

func (s *Server) handleCandidateOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Exchange string  `json:"exchange"`
		Symbol   string  `json:"symbol"`
		Field    string  `json:"field"`
		Value    float64 `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Exchange == "" || req.Symbol == "" || req.Value <= 0 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Field != "planned_entry" && req.Field != "safe_tp_price" {
		http.Error(w, "unsupported field", http.StatusBadRequest)
		return
	}
	if err := s.store.SetCandidateOverride(domain.ExchangeName(req.Exchange), req.Symbol, req.Field, req.Value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.store.AddLog(fmt.Sprintf("candidate override %s %s %s=%.8f", req.Exchange, req.Symbol, req.Field, req.Value))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCloseAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	msg := "Close-all endpoint accepted. Live exchange close implementation is added per exchange in later milestones."
	s.store.AddLog(msg)
	_, _ = w.Write([]byte(msg))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

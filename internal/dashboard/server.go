package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
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

	Planned []domain.PlannedTrade `json:"planned"`
	Active  []domain.ActiveTrade  `json:"active"`
	Closed  []domain.ClosedTrade  `json:"closed"`
	Journal []domain.ClosedTrade  `json:"journal"`

	Settings map[string]any `json:"settings"`
	Logs     []string       `json:"logs"`
}

func New(cfg config.Config, store *storage.Store) *Server {
	s := &Server{
		cfg:   cfg,
		store: store,
		mux:   http.NewServeMux(),
	}

	s.routes()

	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)

	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/planned", s.handlePlanned)
	s.mux.HandleFunc("/api/active", s.handleActive)
	s.mux.HandleFunc("/api/journal", s.handleJournal)

	s.mux.HandleFunc("/api/candidate/override", s.handleCandidateOverride)
	s.mux.HandleFunc("/api/close-all", s.handleCloseAll)
	s.mux.HandleFunc("/api/manual-close", s.handleManualClose)
}

func (s *Server) Addr() string {
	return s.cfg.DashboardHost + ":" + s.cfg.DashboardPort
}

func (s *Server) Run() error {
	return http.ListenAndServe(s.Addr(), s.mux)
}

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
			bal = domain.Balance{
				Exchange:  name,
				UpdatedAt: time.Now().UTC(),
			}
		}

		errText := errors[name]

		exchanges = append(exchanges, domain.ExchangeState{
			Name:       name,
			Enabled:    s.exchangeEnabled(name),
			Connected:  statuses[name],
			PublicOK:   statuses[name],
			PrivateOK:  bal.PrivateOK,
			Balance:    bal,
			Error:      errText,
			Candidates: byExchange[name],
			UpdatedAt:  time.Now().UTC(),
		})
	}

	planned := s.store.PlannedTrades()
	active := s.store.ActiveTrades()
	closed := s.store.ClosedTrades()
	journal := s.store.TradeJournal()

	resp := stateResponse{
		Mode:       string(s.cfg.BotMode),
		ServerTime: time.Now().UTC(),
		Exchanges:  exchanges,

		Planned: planned,
		Active:  active,
		Closed:  closed,
		Journal: journal,

		Settings: s.settings(),
		Logs:     s.store.Logs(),
	}

	writeJSON(w, resp)
}

func (s *Server) handlePlanned(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, map[string]any{
		"planned": s.store.PlannedTrades(),
	})
}

func (s *Server) handleActive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, map[string]any{
		"active": s.store.ActiveTrades(),
	})
}

func (s *Server) handleJournal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, map[string]any{
		"journal": s.store.TradeJournal(),
		"closed":  s.store.ClosedTrades(),
	})
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

	req.Exchange = strings.TrimSpace(req.Exchange)
	req.Symbol = strings.TrimSpace(req.Symbol)
	req.Field = strings.TrimSpace(req.Field)

	if req.Exchange == "" {
		http.Error(w, "exchange is required", http.StatusBadRequest)
		return
	}

	if req.Symbol == "" {
		http.Error(w, "symbol is required", http.StatusBadRequest)
		return
	}

	switch req.Field {
	case "planned_entry", "safe_tp_price":
	default:
		http.Error(w, "unsupported field", http.StatusBadRequest)
		return
	}

	if req.Value <= 0 {
		http.Error(w, "value must be positive", http.StatusBadRequest)
		return
	}

	if err := s.store.SetCandidateOverride(
		domain.ExchangeName(req.Exchange),
		req.Symbol,
		req.Field,
		req.Value,
	); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"ok":       true,
		"exchange": req.Exchange,
		"symbol":   req.Symbol,
		"field":    req.Field,
		"value":    req.Value,
	})
}

func (s *Server) handleCloseAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	active := s.store.ActiveTrades()

	s.store.AddLog(fmt.Sprintf("dashboard close-all requested: active=%d", len(active)))

	// Реальное закрытие всех позиций будет подключено в engine после реализации
	// live-методов по всем 9 биржам.
	writeJSON(w, map[string]any{
		"ok":      true,
		"message": "close-all request accepted; live market close will be connected in engine",
		"active":  len(active),
	})
}

func (s *Server) handleManualClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TradeID string `json:"trade_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	req.TradeID = strings.TrimSpace(req.TradeID)

	if req.TradeID == "" {
		http.Error(w, "trade_id is required", http.StatusBadRequest)
		return
	}

	t, ok := s.store.FindActiveTrade(req.TradeID)
	if !ok {
		http.Error(w, "active trade not found", http.StatusNotFound)
		return
	}

	s.store.AddLog("dashboard manual close requested: " + req.TradeID)

	// Реальное ручное закрытие будет подключено в engine:
	// cancel limit orders -> close market -> save closed trade -> telegram result.
	writeJSON(w, map[string]any{
		"ok":       true,
		"message":  "manual close request accepted; live market close will be connected in engine",
		"trade_id": req.TradeID,
		"trade":    t,
	})
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

func (s *Server) settings() map[string]any {
	return map[string]any{
		"bot_mode":           string(s.cfg.BotMode),
		"allow_mainnet_live": s.cfg.AllowMainnetLive,
		"dashboard_url":      s.cfg.DashboardPublicURL,
		"dashboard_host":     s.cfg.DashboardHost,
		"dashboard_port":     s.cfg.DashboardPort,
		"log_level":          s.cfg.LogLevel,

		"strategy": "funding live-ready execution",

		"min_funding_rate_percent": s.cfg.MinFundingRate * 100,
		"only_positive_funding":    s.cfg.OnlyPositiveFunding,

		"min_24h_volume_usdt":   s.cfg.Min24hVolumeUSDT,
		"use_max_volume_filter": s.cfg.UseMaxVolumeFilter,
		"max_24h_volume_usdt":   s.cfg.Max24hVolumeUSDT,

		"use_spread_filter":  s.cfg.UseSpreadFilter,
		"max_spread_percent": s.cfg.MaxSpread * 100,

		"usdt_per_trade":                 s.cfg.USDTPerTrade,
		"position_size_mode":             s.cfg.PositionSizeMode,
		"position_percent_of_balance":    s.cfg.PositionPercentOfBalance * 100,
		"auto_scale_trades_by_balance":   s.cfg.AutoScaleTradesByBalance,
		"max_active_trades_per_exchange": s.cfg.MaxActiveTradesPerExchange,

		"leverage":    s.cfg.Leverage,
		"margin_mode": s.cfg.MarginMode,

		"plan_minute_utc3":               s.cfg.PlanMinuteUTC3,
		"pre_funding_mark_capture_sec":   int(s.cfg.PreFundingMarkCapture.Seconds()),
		"entry_confirm_timeout_ms":       int(s.cfg.EntryConfirmTimeout.Milliseconds()),
		"manual_alert_after_funding_sec": int(s.cfg.ManualAlertAfterFunding.Seconds()),
		"auto_close_after_funding_sec":   int(s.cfg.AutoCloseAfterFunding.Seconds()),

		"scan_interval_sec":          int(s.cfg.ScanInterval.Seconds()),
		"time_sync_interval_sec":     int(s.cfg.TimeSyncInterval.Seconds()),
		"max_allowed_time_offset_ms": int(s.cfg.MaxAllowedTimeOffset.Milliseconds()),

		"scenario_1": "market short confirmed quickly -> limit buy at fair_price",
		"scenario_2": "funding fee detected or late short confirmation -> limit buy at fair_price * (1 - funding_rate/2)",
		"scenario_3": "market short not confirmed in time -> buy below fair and sell above fair using funding_rate/3 offset",
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

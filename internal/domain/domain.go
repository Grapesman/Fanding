package domain

import "time"

type ExchangeName string

const (
	ExchangeBinance ExchangeName = "Binance"
	ExchangeBybit   ExchangeName = "Bybit"
	ExchangeBingX   ExchangeName = "BingX"
	ExchangeBitget  ExchangeName = "Bitget"
	ExchangeOKX     ExchangeName = "OKX"
	ExchangeGate    ExchangeName = "Gate"
	ExchangeMEXC    ExchangeName = "MEXC"
	ExchangeKuCoin  ExchangeName = "KuCoin"
	ExchangeHTX     ExchangeName = "HTX"
)

var AllExchangeNames = []ExchangeName{
	ExchangeBinance,
	ExchangeBybit,
	ExchangeBingX,
	ExchangeBitget,
	ExchangeOKX,
	ExchangeGate,
	ExchangeMEXC,
	ExchangeKuCoin,
	ExchangeHTX,
}

type Candidate struct {
	Exchange             ExchangeName `json:"exchange"`
	Symbol               string       `json:"symbol"`
	NativeSymbol         string       `json:"native_symbol,omitempty"`
	Price                float64      `json:"price"`
	MarkPrice            float64      `json:"mark_price"`
	FundingRate          float64      `json:"funding_rate"`
	FundingIntervalHours float64      `json:"funding_interval_hours"`
	NextFundingTime      time.Time    `json:"next_funding_time"`
	Volume24hUSDT        float64      `json:"volume_24h_usdt"`
	Bid                  float64      `json:"bid"`
	Ask                  float64      `json:"ask"`
	Spread               float64      `json:"spread"`
	RawFairPrice         float64      `json:"raw_fair_price"`
	SafeTPPrice          float64      `json:"safe_tp_price"`
	PlannedEntry         float64      `json:"planned_entry"`
	PassFunding          bool         `json:"pass_funding"`
	PassVolume           bool         `json:"pass_volume"`
	PassSpread           bool         `json:"pass_spread"`
	Selected             bool         `json:"selected"`
	UpdatedAt            time.Time    `json:"updated_at"`
}

type Balance struct {
	Exchange      ExchangeName `json:"exchange"`
	WalletUSDT    float64      `json:"wallet_usdt"`
	AvailableUSDT float64      `json:"available_usdt"`
	PrivateOK     bool         `json:"private_ok"`
	Error         string       `json:"error,omitempty"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

type ExchangeState struct {
	Name       ExchangeName `json:"name"`
	Enabled    bool         `json:"enabled"`
	Connected  bool         `json:"connected"`
	PublicOK   bool         `json:"public_ok"`
	PrivateOK  bool         `json:"private_ok"`
	Balance    Balance      `json:"balance"`
	Error      string       `json:"error,omitempty"`
	Candidates []Candidate  `json:"candidates"`
	UpdatedAt  time.Time    `json:"updated_at"`
}

type TradeStatus string

const (
	TradePlanned   TradeStatus = "PLANNED"
	TradeActive    TradeStatus = "ACTIVE"
	TradeWaitingTP TradeStatus = "WAITING_TP"
	TradeStuck     TradeStatus = "STUCK"
	TradeFinish    TradeStatus = "FINISH"
	TradeSkipped   TradeStatus = "SKIPPED"
)

type Scenario string

const (
	ScenarioUnknown Scenario = "UNKNOWN"
	ScenarioOne     Scenario = "SCENARIO_1"
	ScenarioTwo     Scenario = "SCENARIO_2"
	ScenarioThree   Scenario = "SCENARIO_3"
)

type Side string

const (
	SideLong  Side = "LONG"
	SideShort Side = "SHORT"
)

type ActiveTrade struct {
	ID              int64        `json:"id"`
	Exchange        ExchangeName `json:"exchange"`
	Symbol          string       `json:"symbol"`
	NativeSymbol    string       `json:"native_symbol,omitempty"`
	Scenario        Scenario     `json:"scenario"`
	Side            Side         `json:"side"`
	Status          TradeStatus  `json:"status"`
	EntryPrice      float64      `json:"entry_price"`
	CurrentPrice    float64      `json:"current_price"`
	TakeProfitPrice float64      `json:"take_profit_price"`
	FundingRate     float64      `json:"funding_rate"`
	FundingFee      float64      `json:"funding_fee"`
	Qty             float64      `json:"qty"`
	UnrealizedPNL   float64      `json:"unrealized_pnl"`
	RealizedPNL     float64      `json:"realized_pnl"`
	NetPNL          float64      `json:"net_pnl"`
	OpenedAt        time.Time    `json:"opened_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

type ClosedTrade struct {
	ActiveTrade
	ExitPrice    float64   `json:"exit_price"`
	ClosedAt     time.Time `json:"closed_at"`
	CloseReason  string    `json:"close_reason"`
	BalanceAfter float64   `json:"balance_after"`
}

type OrderRequest struct {
	Exchange      ExchangeName `json:"exchange"`
	Symbol        string       `json:"symbol"`
	NativeSymbol  string       `json:"native_symbol"`
	Side          string       `json:"side"` // BUY/SELL
	PositionSide  string       `json:"position_side,omitempty"`
	Type          string       `json:"type"` // LIMIT/MARKET
	Price         float64      `json:"price"`
	Qty           float64      `json:"qty"`
	MarginUSDT    float64      `json:"margin_usdt"`
	Leverage      int          `json:"leverage"`
	ReduceOnly    bool         `json:"reduce_only"`
	ClientOrderID string       `json:"client_order_id,omitempty"`
}

type OrderResult struct {
	ExchangeOrderID string    `json:"exchange_order_id"`
	ClientOrderID   string    `json:"client_order_id,omitempty"`
	Symbol          string    `json:"symbol"`
	Side            string    `json:"side"`
	Type            string    `json:"type"`
	Price           float64   `json:"price"`
	Qty             float64   `json:"qty"`
	Status          string    `json:"status"`
	AvgPrice        float64   `json:"avg_price"`
	CreatedAt       time.Time `json:"created_at"`
}

type Position struct {
	Exchange      ExchangeName `json:"exchange"`
	Symbol        string       `json:"symbol"`
	NativeSymbol  string       `json:"native_symbol,omitempty"`
	Side          Side         `json:"side"`
	Qty           float64      `json:"qty"`
	EntryPrice    float64      `json:"entry_price"`
	UnrealizedPNL float64      `json:"unrealized_pnl"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

type Exchange interface {
	Name() ExchangeName
	Connected() bool
	ServerTime() (time.Time, error)
	Balance() (Balance, error)
	FundingCandidates() ([]Candidate, error)
	SetMarginAndLeverage(symbol string, leverage int, marginMode string) error
	PlaceOrder(req OrderRequest) (OrderResult, error)
	CancelOrder(symbol, orderID string) error
	CancelAll(symbol string) error
	ClosePositionMarket(symbol string) error
}

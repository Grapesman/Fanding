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

	// RawFairPrice is the old fair-price field kept for compatibility.
	// For the live strategy we use FairPrice on PlannedTrade / ActiveTrade.
	RawFairPrice float64 `json:"raw_fair_price"`

	// SafeTPPrice is kept for the dashboard candidate table.
	SafeTPPrice float64 `json:"safe_tp_price"`

	// PlannedEntry is kept for compatibility with current dashboard overrides.
	PlannedEntry float64 `json:"planned_entry"`

	PassFunding bool `json:"pass_funding"`
	PassVolume  bool `json:"pass_volume"`
	PassSpread  bool `json:"pass_spread"`
	Selected    bool `json:"selected"`

	// Live eligibility fields. They will be filled after symbol-rule checks.
	CanTradeLive       bool    `json:"can_trade_live"`
	LiveRejectReason   string  `json:"live_reject_reason,omitempty"`
	MinOrderUSDT       float64 `json:"min_order_usdt,omitempty"`
	EstimatedOrderQty  float64 `json:"estimated_order_qty,omitempty"`
	EstimatedOrderUSDT float64 `json:"estimated_order_usdt,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
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
	TradePlanned TradeStatus = "PLANNED"

	// Entry lifecycle.
	TradeWaitingEntryMark TradeStatus = "WAITING_ENTRY_MARK"
	TradeEntrySending     TradeStatus = "ENTRY_SENDING"
	TradeEntrySent        TradeStatus = "ENTRY_SENT"
	TradeEntryConfirmed   TradeStatus = "ENTRY_CONFIRMED"
	TradeEntryLateConfirm TradeStatus = "ENTRY_LATE_CONFIRMED"
	TradeEntryFailed      TradeStatus = "ENTRY_FAILED"

	// Main position lifecycle.
	TradeActive         TradeStatus = "ACTIVE"
	TradeWaitingTP      TradeStatus = "WAITING_TP"
	TradeWaitingFunding TradeStatus = "WAITING_FUNDING_CHECK"
	TradeManualAlert    TradeStatus = "MANUAL_CLOSE_ALERT_SENT"
	TradeStuck          TradeStatus = "STUCK"

	// Scenario 3 lifecycle.
	TradeScenario3BracketPlaced TradeStatus = "SCENARIO_3_BRACKET_PLACED"
	TradeScenario3BuyFilled     TradeStatus = "SCENARIO_3_BUY_FILLED"
	TradeScenario3SellFilled    TradeStatus = "SCENARIO_3_SELL_FILLED"
	TradeScenario3Conflict      TradeStatus = "SCENARIO_3_CONFLICT"

	// Terminal statuses.
	TradeFinish    TradeStatus = "FINISH"
	TradeClosed    TradeStatus = "CLOSED"
	TradeCancelled TradeStatus = "CANCELLED"
	TradeSkipped   TradeStatus = "SKIPPED"
	TradeError     TradeStatus = "ERROR"
)

type Scenario string

const (
	ScenarioUnknown Scenario = "UNKNOWN"

	// SCENARIO_1:
	// Funding-time market short is confirmed quickly.
	// Place reduce-only limit buy at fair_price.
	ScenarioOne Scenario = "SCENARIO_1"

	// SCENARIO_2:
	// Funding fee is detected, or market short confirms late after scenario 3 was started.
	// Place/re-place reduce-only limit buy at fair_price * (1 - funding_rate/2).
	ScenarioTwo Scenario = "SCENARIO_2"

	// SCENARIO_3:
	// Funding-time market short is not confirmed within ENTRY_CONFIRM_TIMEOUT_MS.
	// Place two limit orders around fair_price using funding_rate/3 offset.
	ScenarioThree Scenario = "SCENARIO_3"
)

type Side string

const (
	SideLong  Side = "LONG"
	SideShort Side = "SHORT"
)

type OrderSide string

const (
	OrderSideBuy  OrderSide = "BUY"
	OrderSideSell OrderSide = "SELL"
)

type OrderType string

const (
	OrderTypeMarket OrderType = "MARKET"
	OrderTypeLimit  OrderType = "LIMIT"
)

type OrderStatus string

const (
	OrderStatusUnknown        OrderStatus = "UNKNOWN"
	OrderStatusNew            OrderStatus = "NEW"
	OrderStatusPartiallyFill  OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled         OrderStatus = "FILLED"
	OrderStatusCanceled       OrderStatus = "CANCELED"
	OrderStatusRejected       OrderStatus = "REJECTED"
	OrderStatusExpired        OrderStatus = "EXPIRED"
	OrderStatusPending        OrderStatus = "PENDING"
	OrderStatusPendingCancel  OrderStatus = "PENDING_CANCEL"
	OrderStatusPendingReplace OrderStatus = "PENDING_REPLACE"
)

type CloseReason string

const (
	CloseReasonUnknown                 CloseReason = "UNKNOWN"
	CloseReasonScenario1LimitFilled    CloseReason = "SCENARIO_1_LIMIT_FILLED"
	CloseReasonScenario2LimitFilled    CloseReason = "SCENARIO_2_LIMIT_FILLED"
	CloseReasonScenario3ClosedAtFair   CloseReason = "SCENARIO_3_CLOSED_AT_FAIR"
	CloseReasonScenario3ConflictMarket CloseReason = "SCENARIO_3_CONFLICT_MARKET_CLOSE"
	CloseReasonManualMarketClose       CloseReason = "MANUAL_MARKET_CLOSE"
	CloseReasonAutoMarketClose         CloseReason = "AUTO_MARKET_CLOSE_AFTER_TIMEOUT"
	CloseReasonEmergencyMarketClose    CloseReason = "EMERGENCY_MARKET_CLOSE"
	CloseReasonEntryFailed             CloseReason = "ENTRY_FAILED"
	CloseReasonCancelled               CloseReason = "CANCELLED"
	CloseReasonSkipped                 CloseReason = "SKIPPED"
)

type PlannedTrade struct {
	ID string `json:"id"`

	Exchange     ExchangeName `json:"exchange"`
	Symbol       string       `json:"symbol"`
	NativeSymbol string       `json:"native_symbol,omitempty"`

	Scenario Scenario    `json:"scenario"`
	Status   TradeStatus `json:"status"`

	FundingRate          float64   `json:"funding_rate"`
	FundingIntervalHours float64   `json:"funding_interval_hours"`
	FundingTime          time.Time `json:"funding_time"`

	PositionUSDT float64 `json:"position_usdt"`
	Leverage     int     `json:"leverage"`
	MarginMode   string  `json:"margin_mode"`

	// Captured at T-1 sec before funding.
	PreFundingMarkPrice float64 `json:"pre_funding_mark_price"`
	FairPrice           float64 `json:"fair_price"`

	// Fixed formulas:
	// Scenario2ExtraShift = funding_rate / 2.
	// Scenario3Offset = funding_rate / 3.
	Scenario2ExtraShift float64 `json:"scenario_2_extra_shift"`
	Scenario2TPPrice    float64 `json:"scenario_2_tp_price"`
	Scenario3Offset     float64 `json:"scenario_3_offset"`
	Scenario3BuyPrice   float64 `json:"scenario_3_buy_price"`
	Scenario3SellPrice  float64 `json:"scenario_3_sell_price"`

	EstimatedQty      float64   `json:"estimated_qty"`
	EstimatedNotional float64   `json:"estimated_notional"`
	MinOrderUSDT      float64   `json:"min_order_usdt,omitempty"`
	LiveRejectReason  string    `json:"live_reject_reason,omitempty"`
	CanTradeLive      bool      `json:"can_trade_live"`
	PlanningCycleKey  string    `json:"planning_cycle_key,omitempty"`
	IdempotencyKey    string    `json:"idempotency_key,omitempty"`
	CreatedFromScanAt time.Time `json:"created_from_scan_at"`
	PlannedAt         time.Time `json:"planned_at"`
	EntryAt           time.Time `json:"entry_at"`
	ManualAlertAt     time.Time `json:"manual_alert_at"`
	AutoCloseAt       time.Time `json:"auto_close_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type ActiveTrade struct {
	ID      int64  `json:"id"`
	PlanID  string `json:"plan_id,omitempty"`
	TradeID string `json:"trade_id,omitempty"`

	Exchange     ExchangeName `json:"exchange"`
	Symbol       string       `json:"symbol"`
	NativeSymbol string       `json:"native_symbol,omitempty"`

	Scenario Scenario    `json:"scenario"`
	Side     Side        `json:"side"`
	Status   TradeStatus `json:"status"`

	FundingRate          float64   `json:"funding_rate"`
	FundingIntervalHours float64   `json:"funding_interval_hours"`
	FundingTime          time.Time `json:"funding_time"`

	PositionUSDT float64 `json:"position_usdt"`
	Leverage     int     `json:"leverage"`
	MarginMode   string  `json:"margin_mode"`

	PreFundingMarkPrice float64 `json:"pre_funding_mark_price"`
	FairPrice           float64 `json:"fair_price"`

	Scenario2ExtraShift float64 `json:"scenario_2_extra_shift"`
	Scenario2TPPrice    float64 `json:"scenario_2_tp_price"`
	Scenario3Offset     float64 `json:"scenario_3_offset"`
	Scenario3BuyPrice   float64 `json:"scenario_3_buy_price"`
	Scenario3SellPrice  float64 `json:"scenario_3_sell_price"`

	EntryOrderID       string      `json:"entry_order_id,omitempty"`
	EntryClientOrderID string      `json:"entry_client_order_id,omitempty"`
	EntryOrderStatus   OrderStatus `json:"entry_order_status,omitempty"`
	EntryPrice         float64     `json:"entry_price"`
	EntryAvgPrice      float64     `json:"entry_avg_price"`
	EntryRequestedQty  float64     `json:"entry_requested_qty"`
	EntryFilledQty     float64     `json:"entry_filled_qty"`
	EntryFilledUSDT    float64     `json:"entry_filled_usdt"`
	EntrySentAt        time.Time   `json:"entry_sent_at"`
	EntryConfirmedAt   time.Time   `json:"entry_confirmed_at"`

	CurrentPrice    float64 `json:"current_price"`
	MarkPrice       float64 `json:"mark_price"`
	TakeProfitPrice float64 `json:"take_profit_price"`

	TPOrderID       string      `json:"tp_order_id,omitempty"`
	TPClientOrderID string      `json:"tp_client_order_id,omitempty"`
	TPOrderStatus   OrderStatus `json:"tp_order_status,omitempty"`

	Scenario3BuyOrderID  string      `json:"scenario_3_buy_order_id,omitempty"`
	Scenario3SellOrderID string      `json:"scenario_3_sell_order_id,omitempty"`
	Scenario3BuyStatus   OrderStatus `json:"scenario_3_buy_status,omitempty"`
	Scenario3SellStatus  OrderStatus `json:"scenario_3_sell_status,omitempty"`

	FundingFeeDetected bool       `json:"funding_fee_detected"`
	FundingFeeChecked  bool       `json:"funding_fee_checked"`
	FundingFee         float64    `json:"funding_fee"`
	FundingFeeAsset    string     `json:"funding_fee_asset,omitempty"`
	FundingFeeAt       *time.Time `json:"funding_fee_at,omitempty"`

	Qty             float64 `json:"qty"`
	UnrealizedPNL   float64 `json:"unrealized_pnl"`
	RealizedPNL     float64 `json:"realized_pnl"`
	GrossPNL        float64 `json:"gross_pnl"`
	Commission      float64 `json:"commission"`
	CommissionAsset string  `json:"commission_asset,omitempty"`
	NetPNL          float64 `json:"net_pnl"`

	ManualAlertSentAt *time.Time `json:"manual_alert_sent_at,omitempty"`
	LastError         string     `json:"last_error,omitempty"`

	OpenedAt  time.Time `json:"opened_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ClosedTrade struct {
	ActiveTrade

	ExitOrderID     string      `json:"exit_order_id,omitempty"`
	ExitOrderStatus OrderStatus `json:"exit_order_status,omitempty"`
	ExitPrice       float64     `json:"exit_price"`
	ExitAvgPrice    float64     `json:"exit_avg_price"`
	ExitQty         float64     `json:"exit_qty"`
	ExitUSDT        float64     `json:"exit_usdt"`

	ClosedAt     time.Time   `json:"closed_at"`
	CloseReason  CloseReason `json:"close_reason"`
	BalanceAfter float64     `json:"balance_after"`
	FinalMessage string      `json:"final_message,omitempty"`
}

type SymbolRules struct {
	Exchange     ExchangeName `json:"exchange"`
	Symbol       string       `json:"symbol"`
	NativeSymbol string       `json:"native_symbol,omitempty"`

	// If the exchange accepts USDT/notional orders directly, SupportsNotionalMarketOrder is true.
	// Otherwise the engine/adaptor must convert PositionUSDT into Qty.
	SupportsNotionalMarketOrder bool `json:"supports_notional_market_order"`

	MinQty       float64   `json:"min_qty"`
	MaxQty       float64   `json:"max_qty,omitempty"`
	QtyStep      float64   `json:"qty_step"`
	MinNotional  float64   `json:"min_notional"`
	PriceStep    float64   `json:"price_step"`
	TickSize     float64   `json:"tick_size"`
	ContractSize float64   `json:"contract_size"`
	BaseAsset    string    `json:"base_asset,omitempty"`
	QuoteAsset   string    `json:"quote_asset,omitempty"`
	MarginAsset  string    `json:"margin_asset,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type OrderRequest struct {
	Exchange ExchangeName `json:"exchange"`
	Symbol   string       `json:"symbol"`

	// NativeSymbol is the symbol format required by the exchange API.
	NativeSymbol string `json:"native_symbol"`

	Side         string `json:"side"` // BUY/SELL; kept as string for current client compatibility.
	PositionSide string `json:"position_side,omitempty"`
	Type         string `json:"type"` // LIMIT/MARKET; kept as string for current client compatibility.

	Price float64 `json:"price"`

	// Qty is used when the exchange requires quantity/contract amount.
	Qty float64 `json:"qty"`

	// MarginUSDT/NotionalUSDT is the intended order size in USDT.
	// MarginUSDT is kept for compatibility with current code.
	MarginUSDT   float64 `json:"margin_usdt"`
	NotionalUSDT float64 `json:"notional_usdt"`

	Leverage   int    `json:"leverage"`
	MarginMode string `json:"margin_mode,omitempty"`

	ReduceOnly bool `json:"reduce_only"`
	PostOnly   bool `json:"post_only,omitempty"`

	ClientOrderID string `json:"client_order_id,omitempty"`
	TradeID       string `json:"trade_id,omitempty"`
	PlanID        string `json:"plan_id,omitempty"`

	Scenario Scenario `json:"scenario,omitempty"`

	// TimeInForce is exchange-normalized in clients: GTC/IOC/FOK.
	TimeInForce string `json:"time_in_force,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`
}

type OrderResult struct {
	Exchange        ExchangeName `json:"exchange,omitempty"`
	ExchangeOrderID string       `json:"exchange_order_id"`
	ClientOrderID   string       `json:"client_order_id,omitempty"`

	Symbol       string `json:"symbol"`
	NativeSymbol string `json:"native_symbol,omitempty"`

	Side         string      `json:"side"`
	PositionSide string      `json:"position_side,omitempty"`
	Type         string      `json:"type"`
	Status       string      `json:"status"`
	OrderStatus  OrderStatus `json:"order_status"`

	Price    float64 `json:"price"`
	AvgPrice float64 `json:"avg_price"`
	Qty      float64 `json:"qty"`

	FilledQty      float64 `json:"filled_qty"`
	FilledNotional float64 `json:"filled_notional"`
	RemainingQty   float64 `json:"remaining_qty"`

	Fee      float64 `json:"fee"`
	FeeAsset string  `json:"fee_asset,omitempty"`

	ReduceOnly bool `json:"reduce_only,omitempty"`

	RawMessage string    `json:"raw_message,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

type Position struct {
	Exchange     ExchangeName `json:"exchange"`
	Symbol       string       `json:"symbol"`
	NativeSymbol string       `json:"native_symbol,omitempty"`

	Side Side `json:"side"`

	Qty          float64 `json:"qty"`
	NotionalUSDT float64 `json:"notional_usdt"`

	EntryPrice    float64 `json:"entry_price"`
	MarkPrice     float64 `json:"mark_price"`
	UnrealizedPNL float64 `json:"unrealized_pnl"`
	RealizedPNL   float64 `json:"realized_pnl"`

	LiquidationPrice float64 `json:"liquidation_price,omitempty"`
	Leverage         int     `json:"leverage,omitempty"`
	MarginMode       string  `json:"margin_mode,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

type PositionInfo = Position

type UnrealizedPNLInfo struct {
	Exchange ExchangeName `json:"exchange"`
	Symbol   string       `json:"symbol"`

	NativeSymbol string `json:"native_symbol,omitempty"`

	Side          Side      `json:"side"`
	Qty           float64   `json:"qty"`
	EntryPrice    float64   `json:"entry_price"`
	MarkPrice     float64   `json:"mark_price"`
	UnrealizedPNL float64   `json:"unrealized_pnl"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type FundingFeeInfo struct {
	Exchange ExchangeName `json:"exchange"`
	Symbol   string       `json:"symbol"`

	NativeSymbol string `json:"native_symbol,omitempty"`

	Amount     float64   `json:"amount"`
	Asset      string    `json:"asset"`
	IncomeType string    `json:"income_type,omitempty"`
	FeeTime    time.Time `json:"fee_time"`
	Raw        string    `json:"raw,omitempty"`
}

type TradePlanSummary struct {
	CreatedAt time.Time `json:"created_at"`
	CycleKey  string    `json:"cycle_key"`

	TotalPlanned int `json:"total_planned"`

	ByExchange map[ExchangeName][]PlannedTrade `json:"by_exchange"`
	Balances   map[ExchangeName]Balance        `json:"balances"`
}

type Exchange interface {
	Name() ExchangeName
	Connected() bool
	ServerTime() (time.Time, error)
	Balance() (Balance, error)
	FundingCandidates() ([]Candidate, error)

	// Legacy trading methods kept so current exchange clients keep compiling.
	SetMarginAndLeverage(symbol string, leverage int, marginMode string) error
	PlaceOrder(req OrderRequest) (OrderResult, error)
	CancelOrder(symbol, orderID string) error
	CancelAll(symbol string) error
	ClosePositionMarket(symbol string) error
}

// LiveExchange is the new live-trading interface.
// We keep it separate from Exchange while migrating all 9 exchange clients.
type LiveExchange interface {
	Exchange

	// SymbolRules returns min qty, qty step, tick size and whether the exchange
	// supports direct USDT/notional market orders.
	SymbolRules(symbol string) (SymbolRules, error)

	// PlaceMarketShort opens a short position at market.
	// The engine always requests PositionUSDT=5; exchange client may either send
	// notional directly or convert to Qty using SymbolRules and current price.
	PlaceMarketShort(req OrderRequest) (OrderResult, error)

	// PlaceLimitReduceOnly places a reduce-only limit order.
	// For an opened short this is LIMIT BUY reduce-only.
	PlaceLimitReduceOnly(req OrderRequest) (OrderResult, error)

	GetOrderStatus(symbol, orderID string) (OrderResult, error)
	GetPosition(symbol string) (PositionInfo, error)
	GetUnrealizedPNL(symbol string) (UnrealizedPNLInfo, error)
	FundingFees(symbol string, from, to time.Time) ([]FundingFeeInfo, error)
}

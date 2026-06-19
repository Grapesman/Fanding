package gate

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"funding-bot/internal/domain"
)

type Client struct {
	baseURL   string
	apiKey    string
	apiSecret string
	http      *http.Client
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		apiSecret: strings.TrimSpace(apiSecret),
		http:      &http.Client{Timeout: 12 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeGate
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out []struct {
		Name     string `json:"name"`
		Contract string `json:"contract"`
	}

	if err := c.getJSON("/api/v4/futures/usdt/contracts", nil, &out); err != nil {
		return time.Time{}, err
	}

	if len(out) == 0 {
		return time.Time{}, fmt.Errorf("Gate contracts response is empty")
	}

	return time.Now().UTC(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Gate API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	var out struct {
		Total                 flexibleFloat `json:"total"`
		Available             flexibleFloat `json:"available"`
		Currency              string        `json:"currency"`
		UnrealisedPNL         flexibleFloat `json:"unrealised_pnl"`
		UnrealizedPNL         flexibleFloat `json:"unrealized_pnl"`
		PositionMargin        flexibleFloat `json:"position_margin"`
		OrderMargin           flexibleFloat `json:"order_margin"`
		Point                 flexibleFloat `json:"point"`
		Bonus                 flexibleFloat `json:"bonus"`
		InDualMode            bool          `json:"in_dual_mode"`
		EnableCredit          bool          `json:"enable_credit"`
		PositionInitialMargin flexibleFloat `json:"position_initial_margin"`
	}

	if err := c.signedGET("/api/v4/futures/usdt/accounts", nil, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Gate futures balance error: " + err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	return domain.Balance{
		Exchange:      c.Name(),
		WalletUSDT:    float64(out.Total),
		AvailableUSDT: float64(out.Available),
		PrivateOK:     true,
		Error:         "",
		UpdatedAt:     time.Now().UTC(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	contracts, err := c.contracts()
	if err != nil {
		return nil, err
	}

	tickers := c.tickers()

	now := time.Now().UTC()
	out := make([]domain.Candidate, 0, len(contracts))

	for _, ct := range contracts {
		contract := normalizeContract(firstNonEmpty(ct.Name, ct.Contract))
		if contract == "" || !strings.HasSuffix(contract, "_USDT") {
			continue
		}

		t := tickers[contract]

		price := firstNonZero(
			float64(t.Last),
			float64(ct.LastPrice),
			float64(ct.MarkPrice),
			float64(ct.IndexPrice),
		)

		mark := firstNonZero(float64(t.MarkPrice), float64(ct.MarkPrice), price)

		fundingRate := firstNonZero(float64(t.FundingRate), float64(t.FundingRateIndicative), float64(ct.FundingRate))

		intervalHours := extractIntervalHours(ct.FundingInterval)
		if intervalHours <= 0 {
			intervalHours = extractIntervalHours(t.FundingInterval)
		}
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := time.Time{}
		if ct.FundingNextApply > 0 {
			nextFunding = parseMillisOrSeconds(int64(ct.FundingNextApply))
		}
		if nextFunding.IsZero() && t.FundingNextApply > 0 {
			nextFunding = parseMillisOrSeconds(int64(t.FundingNextApply))
		}
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		bid := float64(t.HighestBid)
		ask := float64(t.LowestAsk)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		volumeUSDT := firstNonZero(
			float64(t.Volume24hQuote),
			float64(t.Volume24hBase)*price,
			float64(ct.Volume24hQuote),
			float64(ct.Volume24hBase)*price,
		)

		symbol := gateUnifiedSymbol(contract)

		out = append(out, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         contract,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          fundingRate,
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        volumeUSDT,
			Bid:                  bid,
			Ask:                  ask,
			Spread:               spread,
			UpdatedAt:            now,
		})
	}

	return out, nil
}

type gateContract struct {
	Name              string        `json:"name"`
	Contract          string        `json:"contract"`
	Type              string        `json:"type"`
	QuantoMultiplier  flexibleFloat `json:"quanto_multiplier"`
	RefDiscountRate   flexibleFloat `json:"ref_discount_rate"`
	OrderPriceDeviate flexibleFloat `json:"order_price_deviate"`
	MaintenanceRate   flexibleFloat `json:"maintenance_rate"`
	MarkType          string        `json:"mark_type"`
	LastPrice         flexibleFloat `json:"last_price"`
	MarkPrice         flexibleFloat `json:"mark_price"`
	IndexPrice        flexibleFloat `json:"index_price"`
	FundingRate       flexibleFloat `json:"funding_rate"`
	FundingInterval   flexibleFloat `json:"funding_interval"`
	FundingNextApply  flexibleInt   `json:"funding_next_apply"`
	OrderPriceRound   flexibleFloat `json:"order_price_round"`
	MarkPriceRound    flexibleFloat `json:"mark_price_round"`
	OrderSizeMin      flexibleFloat `json:"order_size_min"`
	OrderSizeMax      flexibleFloat `json:"order_size_max"`
	RiskLimitBase     flexibleFloat `json:"risk_limit_base"`
	RiskLimitStep     flexibleFloat `json:"risk_limit_step"`
	RiskLimitMax      flexibleFloat `json:"risk_limit_max"`
	MakerFeeRate      flexibleFloat `json:"maker_fee_rate"`
	TakerFeeRate      flexibleFloat `json:"taker_fee_rate"`
	InDelisting       bool          `json:"in_delisting"`
	EnableBonus       bool          `json:"enable_bonus"`
	EnableCredit      bool          `json:"enable_credit"`
	CreateTime        flexibleInt   `json:"create_time"`
	FundingOffset     flexibleInt   `json:"funding_offset"`
	InPreDelivery     bool          `json:"in_pre_delivery"`
	Status            string        `json:"status"`
	BaseCurrency      string        `json:"base_currency"`
	QuoteCurrency     string        `json:"quote_currency"`
	SettleCurrency    string        `json:"settle_currency"`
	Volume24hBase     flexibleFloat `json:"volume_24h_base"`
	Volume24hQuote    flexibleFloat `json:"volume_24h_quote"`
}

func (c *Client) contracts() ([]gateContract, error) {
	var out []gateContract

	if err := c.getJSON("/api/v4/futures/usdt/contracts", nil, &out); err != nil {
		return nil, err
	}

	return out, nil
}

type gateTicker struct {
	Contract              string        `json:"contract"`
	Last                  flexibleFloat `json:"last"`
	ChangePercentage      flexibleFloat `json:"change_percentage"`
	TotalSize             flexibleFloat `json:"total_size"`
	Volume24h             flexibleFloat `json:"volume_24h"`
	Volume24hBase         flexibleFloat `json:"volume_24h_base"`
	Volume24hQuote        flexibleFloat `json:"volume_24h_quote"`
	Volume24hSettle       flexibleFloat `json:"volume_24h_settle"`
	MarkPrice             flexibleFloat `json:"mark_price"`
	IndexPrice            flexibleFloat `json:"index_price"`
	FundingRate           flexibleFloat `json:"funding_rate"`
	FundingRateIndicative flexibleFloat `json:"funding_rate_indicative"`
	FundingInterval       flexibleFloat `json:"funding_interval"`
	FundingNextApply      flexibleInt   `json:"funding_next_apply"`
	HighestBid            flexibleFloat `json:"highest_bid"`
	LowestAsk             flexibleFloat `json:"lowest_ask"`
}

func (c *Client) tickers() map[string]gateTicker {
	out := map[string]gateTicker{}

	var rows []gateTicker
	if err := c.getJSON("/api/v4/futures/usdt/tickers", nil, &rows); err != nil {
		return out
	}

	for _, r := range rows {
		contract := normalizeContract(r.Contract)
		if contract == "" {
			continue
		}

		out[contract] = r
	}

	return out
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	contract := normalizeGateSymbol(symbol)

	rows, err := c.contracts()
	if err != nil {
		return domain.SymbolRules{}, err
	}

	for _, item := range rows {
		name := normalizeContract(firstNonEmpty(item.Name, item.Contract))
		if name != contract {
			continue
		}

		qtyStep := 1.0
		priceStep := firstNonZero(float64(item.OrderPriceRound), float64(item.MarkPriceRound))

		base := item.BaseCurrency
		quote := item.QuoteCurrency
		settle := item.SettleCurrency

		if base == "" || quote == "" {
			parts := strings.Split(name, "_")
			if len(parts) == 2 {
				if base == "" {
					base = parts[0]
				}
				if quote == "" {
					quote = parts[1]
				}
			}
		}

		return domain.SymbolRules{
			Exchange:                    c.Name(),
			Symbol:                      gateUnifiedSymbol(name),
			NativeSymbol:                name,
			SupportsNotionalMarketOrder: false,
			MinQty:                      firstNonZero(float64(item.OrderSizeMin), 1),
			MaxQty:                      float64(item.OrderSizeMax),
			QtyStep:                     qtyStep,
			MinNotional:                 0,
			PriceStep:                   priceStep,
			TickSize:                    priceStep,
			ContractSize:                firstNonZero(float64(item.QuantoMultiplier), 1),
			BaseAsset:                   base,
			QuoteAsset:                  firstNonEmpty(quote, "USDT"),
			MarginAsset:                 firstNonEmpty(settle, "USDT"),
			UpdatedAt:                   time.Now().UTC(),
		}, nil
	}

	return domain.SymbolRules{}, fmt.Errorf("Gate symbol rules not found: %s", contract)
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	contract := normalizeGateSymbol(symbol)

	if leverage <= 0 {
		leverage = 1
	}

	body := map[string]string{
		"leverage": strconv.Itoa(leverage),
	}

	var out any
	err := c.signedRequest(http.MethodPost, "/api/v4/futures/usdt/positions/"+url.PathEscape(contract)+"/leverage", nil, body, &out)
	if err == nil {
		return nil
	}

	// Некоторые аккаунты/режимы Gate возвращают ошибку, если плечо уже установлено
	// или если для позиции пока нет данных. На планирование это не должно падать.
	if isIgnorableGateMsg(err.Error()) {
		return nil
	}

	return err
}

func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	if strings.EqualFold(req.Type, string(domain.OrderTypeMarket)) &&
		strings.EqualFold(req.Side, string(domain.OrderSideSell)) &&
		!req.ReduceOnly {
		return c.PlaceMarketShort(req)
	}

	if strings.EqualFold(req.Type, string(domain.OrderTypeLimit)) {
		return c.PlaceLimitReduceOnly(req)
	}

	return domain.OrderResult{}, fmt.Errorf("Gate unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
}

func (c *Client) PlaceMarketShort(req domain.OrderRequest) (domain.OrderResult, error) {
	contract := normalizeGateSymbol(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(contract)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		price = c.markPrice(contract)
	}
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("Gate %s mark price is zero", contract)
	}

	contractsQty := req.Qty
	if contractsQty <= 0 {
		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		contractsQty = notional / price / firstNonZero(rules.ContractSize, 1)
	}

	contractsQty = floorToStep(contractsQty, rules.QtyStep)
	if contractsQty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("Gate %s calculated contracts qty is zero", contract)
	}

	if rules.MinQty > 0 && contractsQty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("Gate %s qty %.12f < min qty %.12f", contract, contractsQty, rules.MinQty)
	}

	body := gateOrderRequest{
		Contract: contract,
		Size:     -int64(math.Abs(contractsQty)),
		Price:    "0",
		TIF:      "ioc",
		Text:     gateClientText(req.ClientOrderID),
	}

	var out gateOrderData
	if err := c.signedPOST("/api/v4/futures/usdt/orders", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	res := out.toDomain(c.Name(), contract, rules.ContractSize)
	if res.Price <= 0 {
		res.Price = price
	}
	if res.Qty <= 0 {
		res.Qty = math.Abs(contractsQty)
	}

	if res.ExchangeOrderID != "" {
		if fresh, err := c.GetOrderStatus(contract, res.ExchangeOrderID); err == nil {
			res = fresh
		}
	}

	return res, nil
}

func (c *Client) PlaceLimitReduceOnly(req domain.OrderRequest) (domain.OrderResult, error) {
	contract := normalizeGateSymbol(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(contract)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("Gate %s limit price is zero", contract)
	}

	price = floorToStep(price, rules.TickSize)

	contractsQty := req.Qty
	if contractsQty <= 0 {
		ref := c.markPrice(contract)
		if ref <= 0 {
			ref = price
		}

		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		contractsQty = notional / ref / firstNonZero(rules.ContractSize, 1)
	}

	contractsQty = floorToStep(contractsQty, rules.QtyStep)
	if contractsQty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("Gate %s calculated contracts qty is zero", contract)
	}

	if rules.MinQty > 0 && contractsQty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("Gate %s qty %.12f < min qty %.12f", contract, contractsQty, rules.MinQty)
	}

	size := int64(math.Abs(contractsQty))
	if strings.EqualFold(req.Side, string(domain.OrderSideSell)) || strings.EqualFold(req.Side, "sell") {
		size = -size
	}

	body := gateOrderRequest{
		Contract:   contract,
		Size:       size,
		Price:      formatFloat(price),
		TIF:        gateTIF(req.TimeInForce),
		Text:       gateClientText(req.ClientOrderID),
		ReduceOnly: req.ReduceOnly,
	}

	var out gateOrderData
	if err := c.signedPOST("/api/v4/futures/usdt/orders", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	res := out.toDomain(c.Name(), contract, rules.ContractSize)
	if res.Price <= 0 {
		res.Price = price
	}
	if res.Qty <= 0 {
		res.Qty = math.Abs(contractsQty)
	}
	res.ReduceOnly = req.ReduceOnly

	return res, nil
}

func (c *Client) GetOrderStatus(symbol, orderID string) (domain.OrderResult, error) {
	contract := normalizeGateSymbol(symbol)

	var out gateOrderData
	if err := c.signedGET("/api/v4/futures/usdt/orders/"+url.PathEscape(orderID), nil, &out); err != nil {
		return domain.OrderResult{}, err
	}

	rules, _ := c.SymbolRules(contract)

	return out.toDomain(c.Name(), contract, rules.ContractSize), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	err := c.signedDELETE("/api/v4/futures/usdt/orders/"+url.PathEscape(orderID), nil, nil)
	if err == nil {
		return nil
	}

	if isIgnorableGateMsg(err.Error()) {
		return nil
	}

	return err
}

func (c *Client) CancelAll(symbol string) error {
	contract := normalizeGateSymbol(symbol)

	q := url.Values{}
	q.Set("contract", contract)

	err := c.signedDELETE("/api/v4/futures/usdt/orders", q, nil)
	if err == nil {
		return nil
	}

	if isIgnorableGateMsg(err.Error()) {
		return nil
	}

	return err
}

func (c *Client) ClosePositionMarket(symbol string) error {
	contract := normalizeGateSymbol(symbol)

	pos, err := c.GetPosition(contract)
	if err != nil {
		return err
	}

	if pos.Qty <= 0 {
		return nil
	}

	size := int64(math.Abs(pos.Qty))
	if size <= 0 {
		return nil
	}

	// Закрытие short = buy, положительный size.
	// Закрытие long = sell, отрицательный size.
	if pos.Side == domain.SideLong {
		size = -size
	}

	body := gateOrderRequest{
		Contract:   contract,
		Size:       size,
		Price:      "0",
		TIF:        "ioc",
		ReduceOnly: true,
		Text:       gateClientText("close-" + contract + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)),
	}

	var out gateOrderData
	if err := c.signedPOST("/api/v4/futures/usdt/orders", body, &out); err != nil {
		return err
	}

	return nil
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	contract := normalizeGateSymbol(symbol)

	var out gatePositionData
	if err := c.signedGET("/api/v4/futures/usdt/positions/"+url.PathEscape(contract), nil, &out); err != nil {
		if isIgnorableGateMsg(err.Error()) {
			return domain.PositionInfo{
				Exchange:     c.Name(),
				Symbol:       gateUnifiedSymbol(contract),
				NativeSymbol: contract,
				UpdatedAt:    time.Now().UTC(),
			}, nil
		}

		return domain.PositionInfo{}, err
	}

	size := float64(out.Size)
	if size == 0 {
		return domain.PositionInfo{
			Exchange:     c.Name(),
			Symbol:       gateUnifiedSymbol(contract),
			NativeSymbol: contract,
			UpdatedAt:    time.Now().UTC(),
		}, nil
	}

	side := domain.SideLong
	if size < 0 {
		side = domain.SideShort
	}

	return domain.PositionInfo{
		Exchange:         c.Name(),
		Symbol:           gateUnifiedSymbol(contract),
		NativeSymbol:     contract,
		Side:             side,
		Qty:              math.Abs(size),
		NotionalUSDT:     math.Abs(float64(out.Value)),
		EntryPrice:       float64(out.EntryPrice),
		MarkPrice:        float64(out.MarkPrice),
		UnrealizedPNL:    float64(out.UnrealisedPNL),
		RealizedPNL:      float64(out.RealisedPNL),
		LiquidationPrice: float64(out.LiqPrice),
		Leverage:         int(float64(out.Leverage)),
		MarginMode:       "isolated",
		UpdatedAt:        time.Now().UTC(),
	}, nil
}

func (c *Client) GetUnrealizedPNL(symbol string) (domain.UnrealizedPNLInfo, error) {
	pos, err := c.GetPosition(symbol)
	if err != nil {
		return domain.UnrealizedPNLInfo{}, err
	}

	return domain.UnrealizedPNLInfo{
		Exchange:      c.Name(),
		Symbol:        pos.Symbol,
		NativeSymbol:  pos.NativeSymbol,
		Side:          pos.Side,
		Qty:           pos.Qty,
		EntryPrice:    pos.EntryPrice,
		MarkPrice:     pos.MarkPrice,
		UnrealizedPNL: pos.UnrealizedPNL,
		UpdatedAt:     time.Now().UTC(),
	}, nil
}

func (c *Client) FundingFees(symbol string, from, to time.Time) ([]domain.FundingFeeInfo, error) {
	contract := normalizeGateSymbol(symbol)

	q := url.Values{}
	q.Set("contract", contract)
	q.Set("type", "fund")
	q.Set("limit", "100")

	if !from.IsZero() {
		q.Set("from", strconv.FormatInt(from.UTC().Unix(), 10))
	}
	if !to.IsZero() {
		q.Set("to", strconv.FormatInt(to.UTC().Unix(), 10))
	}

	var rows []gateAccountBookItem
	if err := c.signedGET("/api/v4/futures/usdt/account_book", q, &rows); err != nil {
		return nil, err
	}

	res := make([]domain.FundingFeeInfo, 0, len(rows))

	for _, r := range rows {
		if r.Contract != "" && normalizeContract(r.Contract) != contract {
			continue
		}

		res = append(res, domain.FundingFeeInfo{
			Exchange:     c.Name(),
			Symbol:       gateUnifiedSymbol(contract),
			NativeSymbol: contract,
			Amount:       float64(r.Change),
			Asset:        "USDT",
			IncomeType:   firstNonEmpty(r.Type, "FUNDING_FEE"),
			FeeTime:      parseMillisOrSeconds(int64(r.Time)),
			Raw:          r.Text,
		})
	}

	return res, nil
}

type gateOrderRequest struct {
	Contract   string `json:"contract"`
	Size       int64  `json:"size"`
	Price      string `json:"price"`
	TIF        string `json:"tif,omitempty"`
	Text       string `json:"text,omitempty"`
	ReduceOnly bool   `json:"reduce_only,omitempty"`
}

type gateOrderData struct {
	ID           flexibleID    `json:"id"`
	Text         string        `json:"text"`
	Contract     string        `json:"contract"`
	Size         flexibleFloat `json:"size"`
	Left         flexibleFloat `json:"left"`
	Price        flexibleFloat `json:"price"`
	FillPrice    flexibleFloat `json:"fill_price"`
	AvgDealPrice flexibleFloat `json:"avg_deal_price"`
	Status       string        `json:"status"`
	TIF          string        `json:"tif"`
	Fee          flexibleFloat `json:"fee"`
	FeeAsset     string        `json:"fee_asset"`
	ReduceOnly   bool          `json:"reduce_only"`
	CreateTime   flexibleFloat `json:"create_time"`
	CreateTimeMs flexibleInt   `json:"create_time_ms"`
	FinishTime   flexibleFloat `json:"finish_time"`
	FinishTimeMs flexibleInt   `json:"finish_time_ms"`
}

func (o gateOrderData) toDomain(exchange domain.ExchangeName, fallbackContract string, contractSize float64) domain.OrderResult {
	contract := normalizeContract(firstNonEmpty(o.Contract, fallbackContract))

	size := float64(o.Size)
	left := float64(o.Left)
	qty := math.Abs(size)
	filled := math.Max(0, math.Abs(size)-math.Abs(left))

	createdAt := time.Now().UTC()
	if o.CreateTimeMs > 0 {
		createdAt = parseMillisOrSeconds(int64(o.CreateTimeMs))
	} else if o.CreateTime > 0 {
		createdAt = parseMillisOrSeconds(int64(float64(o.CreateTime)))
	}

	updatedAt := time.Now().UTC()
	if o.FinishTimeMs > 0 {
		updatedAt = parseMillisOrSeconds(int64(o.FinishTimeMs))
	} else if o.FinishTime > 0 {
		updatedAt = parseMillisOrSeconds(int64(float64(o.FinishTime)))
	}

	side := "BUY"
	if size < 0 {
		side = "SELL"
	}

	avgPrice := firstNonZero(float64(o.AvgDealPrice), float64(o.FillPrice))
	price := float64(o.Price)
	if price <= 0 {
		price = avgPrice
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: string(o.ID),
		ClientOrderID:   strings.TrimPrefix(o.Text, "t-"),
		Symbol:          gateUnifiedSymbol(contract),
		NativeSymbol:    contract,
		Side:            side,
		PositionSide:    "",
		Type:            gateOrderType(o),
		Status:          o.Status,
		OrderStatus:     mapGateOrderStatus(o.Status, left),
		Price:           price,
		AvgPrice:        avgPrice,
		Qty:             qty,
		FilledQty:       filled,
		FilledNotional:  filled * firstNonZero(avgPrice, price) * firstNonZero(contractSize, 1),
		RemainingQty:    math.Abs(left),
		Fee:             float64(o.Fee),
		FeeAsset:        firstNonEmpty(o.FeeAsset, "USDT"),
		ReduceOnly:      o.ReduceOnly,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

type gatePositionData struct {
	Contract        string        `json:"contract"`
	Size            flexibleFloat `json:"size"`
	Leverage        flexibleFloat `json:"leverage"`
	RiskLimit       flexibleFloat `json:"risk_limit"`
	LeverageMax     flexibleFloat `json:"leverage_max"`
	MaintenanceRate flexibleFloat `json:"maintenance_rate"`
	Value           flexibleFloat `json:"value"`
	Margin          flexibleFloat `json:"margin"`
	EntryPrice      flexibleFloat `json:"entry_price"`
	MarkPrice       flexibleFloat `json:"mark_price"`
	UnrealisedPNL   flexibleFloat `json:"unrealised_pnl"`
	RealisedPNL     flexibleFloat `json:"realised_pnl"`
	HistoryPNL      flexibleFloat `json:"history_pnl"`
	LiqPrice        flexibleFloat `json:"liq_price"`
	UpdateTime      flexibleFloat `json:"update_time"`
	UpdateTimeMs    flexibleInt   `json:"update_time_ms"`
}

type gateAccountBookItem struct {
	Time     flexibleInt   `json:"time"`
	Change   flexibleFloat `json:"change"`
	Balance  flexibleFloat `json:"balance"`
	Type     string        `json:"type"`
	Text     string        `json:"text"`
	Contract string        `json:"contract"`
}

func (c *Client) markPrice(symbol string) float64 {
	contract := normalizeGateSymbol(symbol)

	q := url.Values{}
	q.Set("contract", contract)

	var rows []gateTicker
	if err := c.getJSON("/api/v4/futures/usdt/tickers", q, &rows); err != nil {
		return 0
	}

	for _, row := range rows {
		if normalizeContract(row.Contract) != contract {
			continue
		}

		return firstNonZero(float64(row.MarkPrice), float64(row.IndexPrice), float64(row.Last))
	}

	return 0
}

func (c *Client) getJSON(path string, q url.Values, dst any) error {
	u := c.baseURL + path
	if q != nil && len(q) > 0 {
		u += "?" + q.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("Gate GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("Gate GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	return c.signedRequest(http.MethodGet, path, q, nil, dst)
}

func (c *Client) signedPOST(path string, body any, dst any) error {
	return c.signedRequest(http.MethodPost, path, nil, body, dst)
}

func (c *Client) signedDELETE(path string, q url.Values, dst any) error {
	return c.signedRequest(http.MethodDelete, path, q, nil, dst)
}

func (c *Client) signedRequest(method, path string, q url.Values, body any, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("Gate API key or secret is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	query := q.Encode()

	bodyBytes := []byte{}
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	bodyHash := sha512Sum(string(bodyBytes))
	timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)

	signPayload := strings.Join([]string{
		method,
		path,
		query,
		bodyHash,
		timestamp,
	}, "\n")

	signature := c.sign(signPayload)

	requestPath := path
	if query != "" {
		requestPath += "?" + query
	}

	u := c.baseURL + requestPath

	var reader io.Reader
	if len(bodyBytes) > 0 {
		reader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("KEY", c.apiKey)
	req.Header.Set("Timestamp", timestamp)
	req.Header.Set("SIGN", signature)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("Gate signed %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("Gate signed %s %s decode error: %w; body=%s", method, path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha512.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return hex.EncodeToString(mac.Sum(nil))
}

func sha512Sum(s string) string {
	h := sha512.New()
	h.Write([]byte(s))

	return hex.EncodeToString(h.Sum(nil))
}

type flexibleFloat float64

func (f *flexibleFloat) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*f = 0
		return nil
	}

	var num float64
	if err := json.Unmarshal(b, &num); err == nil {
		*f = flexibleFloat(num)
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	if s == "" {
		*f = 0
		return nil
	}

	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		*f = 0
		return nil
	}

	*f = flexibleFloat(v)

	return nil
}

type flexibleInt int64

func (i *flexibleInt) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*i = 0
		return nil
	}

	var num int64
	if err := json.Unmarshal(b, &num); err == nil {
		*i = flexibleInt(num)
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	if s == "" {
		*i = 0
		return nil
	}

	v, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if err != nil {
		*i = 0
		return nil
	}

	*i = flexibleInt(v)

	return nil
}

type flexibleID string

func (id *flexibleID) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*id = ""
		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*id = flexibleID(s)
		return nil
	}

	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*id = flexibleID(strconv.FormatInt(n, 10))
		return nil
	}

	return nil
}

func normalizeContract(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

func normalizeGateSymbol(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))

	if strings.Contains(s, "_") {
		return s
	}

	if strings.HasSuffix(s, "USDT") {
		return strings.TrimSuffix(s, "USDT") + "_USDT"
	}

	return s
}

func gateUnifiedSymbol(contract string) string {
	return strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(contract)), "_", "")
}

func parseMillisOrSeconds(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}

	if v > 1_000_000_000_000 {
		return time.UnixMilli(v).UTC()
	}

	if v > 1_000_000_000 {
		return time.Unix(v, 0).UTC()
	}

	return time.Time{}
}

func extractIntervalHours(values ...flexibleFloat) float64 {
	for _, raw := range values {
		v := float64(raw)
		if v <= 0 {
			continue
		}

		if v >= 3600000 && int64(v)%3600000 == 0 {
			return v / 3600000
		}

		if v >= 3600 && int64(v)%3600 == 0 {
			return v / 3600
		}

		if v >= 60 && v <= 1440 && int64(v)%60 == 0 {
			return v / 60
		}

		if v > 0 && v <= 24 {
			return v
		}
	}

	return 0
}

func nextFundingByInterval(now time.Time, hours int) time.Time {
	if hours <= 0 {
		hours = 8
	}

	now = now.UTC()
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	step := time.Duration(hours) * time.Hour

	for t := base; ; t = t.Add(step) {
		if t.After(now) {
			return t
		}
	}
}

func floorToStep(v, step float64) float64 {
	if v <= 0 {
		return 0
	}

	if step <= 0 {
		return v
	}

	return math.Floor(v/step) * step
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func gateTIF(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "gtc"
	}

	switch v {
	case "gtc", "ioc", "poc", "fok":
		return v
	default:
		return "gtc"
	}
}

func gateClientText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "fg-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	}

	replacer := strings.NewReplacer(
		":", "-",
		"/", "-",
		" ", "-",
		"_", "-",
	)

	s = replacer.Replace(s)

	if !strings.HasPrefix(s, "t-") {
		s = "t-" + s
	}

	if len(s) > 28 {
		s = s[:2] + s[len(s)-26:]
	}

	return s
}

func gateOrderType(o gateOrderData) string {
	if strings.EqualFold(o.TIF, "ioc") && float64(o.Price) == 0 {
		return string(domain.OrderTypeMarket)
	}

	return string(domain.OrderTypeLimit)
}

func mapGateOrderStatus(status string, left float64) domain.OrderStatus {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "open":
		if left > 0 {
			return domain.OrderStatusNew
		}

		return domain.OrderStatusNew
	case "finished":
		if left > 0 {
			return domain.OrderStatusPartiallyFill
		}

		return domain.OrderStatusFilled
	case "cancelled", "canceled":
		return domain.OrderStatusCanceled
	case "failed", "rejected":
		return domain.OrderStatusRejected
	default:
		return domain.OrderStatusUnknown
	}
}

func isIgnorableGateMsg(msg string) bool {
	msg = strings.ToLower(msg)

	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "not exist") ||
		strings.Contains(msg, "no need") ||
		strings.Contains(msg, "same") ||
		strings.Contains(msg, "already") ||
		strings.Contains(msg, "finished") ||
		strings.Contains(msg, "cancelled") ||
		strings.Contains(msg, "canceled")
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}

	return ""
}

func firstNonZero(values ...float64) float64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}

	return 0
}

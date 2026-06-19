package htx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
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

type htxFundingInfo struct {
	ContractCode  string
	Rate          float64
	NextFunding   time.Time
	IntervalHours float64
	OK            bool
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		apiSecret: strings.TrimSpace(apiSecret),
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeHTX
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Status string      `json:"status"`
		TS     flexibleInt `json:"ts"`
	}

	if err := c.getJSON("/linear-swap-api/v1/swap_contract_info", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.Status != "" && out.Status != "ok" {
		return time.Time{}, fmt.Errorf("HTX server time check status=%s", out.Status)
	}

	if out.TS > 0 {
		return time.UnixMilli(int64(out.TS)).UTC(), nil
	}

	return time.Now().UTC(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "HTX API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	endpoints := []string{
		"/linear-swap-api/v3/unified_account_info",
		"/linear-swap-api/v1/swap_cross_account_info",
		"/linear-swap-api/v1/swap_account_info",
	}

	var lastErr error

	for _, endpoint := range endpoints {
		balance, err := c.balanceFromEndpoint(endpoint)
		if err == nil && balance.PrivateOK {
			return balance, nil
		}

		if err != nil {
			lastErr = err
		}
	}

	if lastErr != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "HTX futures balance error: " + lastErr.Error(),
			UpdatedAt: time.Now().UTC(),
		}, lastErr
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "HTX USDT futures balance not found",
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (c *Client) balanceFromEndpoint(endpoint string) (domain.Balance, error) {
	var out struct {
		Status string      `json:"status"`
		ErrMsg string      `json:"err_msg"`
		TS     flexibleInt `json:"ts"`
		Data   []struct {
			MarginAsset       string        `json:"margin_asset"`
			Symbol            string        `json:"symbol"`
			ContractCode      string        `json:"contract_code"`
			MarginBalance     flexibleFloat `json:"margin_balance"`
			MarginStatic      flexibleFloat `json:"margin_static"`
			MarginAvailable   flexibleFloat `json:"margin_available"`
			WithdrawAvailable flexibleFloat `json:"withdraw_available"`
			ProfitReal        flexibleFloat `json:"profit_real"`
			ProfitUnreal      flexibleFloat `json:"profit_unreal"`
			RiskRate          flexibleFloat `json:"risk_rate"`
			LiquidationPrice  flexibleFloat `json:"liquidation_price"`
			LeverRate         flexibleFloat `json:"lever_rate"`
			AdjustFactor      flexibleFloat `json:"adjust_factor"`
			MarginFrozen      flexibleFloat `json:"margin_frozen"`
			MarginPosition    flexibleFloat `json:"margin_position"`
			MarginOrder       flexibleFloat `json:"margin_order"`
			SubAccountName    string        `json:"sub_account_name"`
			TradePartition    string        `json:"trade_partition"`
		} `json:"data"`
	}

	body := map[string]string{
		"margin_asset": "USDT",
	}

	if err := c.signedPOST(endpoint, body, &out); err != nil {
		return domain.Balance{}, err
	}

	if out.Status != "" && out.Status != "ok" {
		return domain.Balance{}, fmt.Errorf("HTX balance endpoint=%s status=%s err=%s", endpoint, out.Status, out.ErrMsg)
	}

	for _, row := range out.Data {
		if row.MarginAsset != "" && !strings.EqualFold(row.MarginAsset, "USDT") {
			continue
		}

		wallet := firstNonZero(
			float64(row.MarginBalance),
			float64(row.MarginStatic),
		)

		available := firstNonZero(
			float64(row.MarginAvailable),
			float64(row.WithdrawAvailable),
		)

		if wallet == 0 && available == 0 {
			continue
		}

		return domain.Balance{
			Exchange:      c.Name(),
			WalletUSDT:    wallet,
			AvailableUSDT: available,
			PrivateOK:     true,
			Error:         "",
			UpdatedAt:     time.Now().UTC(),
		}, nil
	}

	return domain.Balance{}, fmt.Errorf("HTX USDT balance not found in endpoint %s", endpoint)
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
		code := normalizeHTXContract(firstNonEmpty(ct.ContractCode, ct.Symbol))
		if code == "" || !strings.Contains(code, "USDT") {
			continue
		}

		t := tickers[code]
		funding := c.fetchFundingInfo(code, now)

		price := firstNonZero(
			float64(t.Close),
			float64(t.LastPrice),
			float64(t.MarkPrice),
		)

		mark := firstNonZero(
			float64(t.MarkPrice),
			price,
		)

		intervalHours := funding.IntervalHours
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := funding.NextFunding
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		bid := float64(t.Bid)
		ask := float64(t.Ask)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		volumeUSDT := firstNonZero(
			float64(t.Amount),
			float64(t.Vol)*price,
			float64(t.Count)*price,
		)

		out = append(out, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               htxUnifiedSymbol(code),
			NativeSymbol:         code,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          funding.Rate,
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        volumeUSDT,
			Bid:                  bid,
			Ask:                  ask,
			Spread:               spread,
			UpdatedAt:            now,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].FundingRate == out[j].FundingRate {
			return out[i].Volume24hUSDT > out[j].Volume24hUSDT
		}

		return out[i].FundingRate > out[j].FundingRate
	})

	return out, nil
}

type htxContract struct {
	Symbol            string        `json:"symbol"`
	ContractCode      string        `json:"contract_code"`
	ContractSize      flexibleFloat `json:"contract_size"`
	PriceTick         flexibleFloat `json:"price_tick"`
	DeliveryDate      string        `json:"delivery_date"`
	CreateDate        string        `json:"create_date"`
	ContractStatus    flexibleInt   `json:"contract_status"`
	SupportMarginMode string        `json:"support_margin_mode"`
	BusinessType      string        `json:"business_type"`
	Pair              string        `json:"pair"`
	TradePartition    string        `json:"trade_partition"`
	MinOrderValue     flexibleFloat `json:"min_order_value"`
	OrderPriceType    string        `json:"order_price_type"`
	ContractType      string        `json:"contract_type"`
}

func (c *Client) contracts() ([]htxContract, error) {
	var out struct {
		Status string        `json:"status"`
		ErrMsg string        `json:"err_msg"`
		TS     flexibleInt   `json:"ts"`
		Data   []htxContract `json:"data"`
	}

	if err := c.getJSON("/linear-swap-api/v1/swap_contract_info", nil, &out); err != nil {
		return nil, err
	}

	if out.Status != "" && out.Status != "ok" {
		return nil, fmt.Errorf("HTX contract info status=%s err=%s", out.Status, out.ErrMsg)
	}

	return out.Data, nil
}

type htxTicker struct {
	ContractCode string        `json:"contract_code"`
	Close        flexibleFloat `json:"close"`
	LastPrice    flexibleFloat `json:"last_price"`
	MarkPrice    flexibleFloat `json:"mark_price"`
	Ask          flexibleFloat `json:"ask"`
	Bid          flexibleFloat `json:"bid"`
	Amount       flexibleFloat `json:"amount"`
	Vol          flexibleFloat `json:"vol"`
	Count        flexibleFloat `json:"count"`
	High         flexibleFloat `json:"high"`
	Low          flexibleFloat `json:"low"`
	Open         flexibleFloat `json:"open"`
}

func (c *Client) tickers() map[string]htxTicker {
	result := map[string]htxTicker{}

	var out struct {
		Status string `json:"status"`
		ErrMsg string `json:"err_msg"`
		Tick   []struct {
			ContractCode string          `json:"contract_code"`
			Close        flexibleFloat   `json:"close"`
			LastPrice    flexibleFloat   `json:"last_price"`
			MarkPrice    flexibleFloat   `json:"mark_price"`
			Ask          []flexibleFloat `json:"ask"`
			Bid          []flexibleFloat `json:"bid"`
			Amount       flexibleFloat   `json:"amount"`
			Vol          flexibleFloat   `json:"vol"`
			Count        flexibleFloat   `json:"count"`
			High         flexibleFloat   `json:"high"`
			Low          flexibleFloat   `json:"low"`
			Open         flexibleFloat   `json:"open"`
		} `json:"ticks"`
	}

	if err := c.getJSON("/linear-swap-ex/market/detail/batch_merged", nil, &out); err != nil {
		return result
	}

	if out.Status != "" && out.Status != "ok" {
		return result
	}

	for _, t := range out.Tick {
		code := normalizeHTXContract(t.ContractCode)
		if code == "" {
			continue
		}

		row := htxTicker{
			ContractCode: code,
			Close:        t.Close,
			LastPrice:    t.LastPrice,
			MarkPrice:    t.MarkPrice,
			Amount:       t.Amount,
			Vol:          t.Vol,
			Count:        t.Count,
			High:         t.High,
			Low:          t.Low,
			Open:         t.Open,
		}

		if len(t.Ask) > 0 {
			row.Ask = t.Ask[0]
		}
		if len(t.Bid) > 0 {
			row.Bid = t.Bid[0]
		}

		result[code] = row
	}

	return result
}

func (c *Client) fetchFundingInfo(contractCode string, now time.Time) htxFundingInfo {
	q := url.Values{}
	q.Set("contract_code", contractCode)

	var out struct {
		Status string `json:"status"`
		ErrMsg string `json:"err_msg"`
		Data   []struct {
			ContractCode        string        `json:"contract_code"`
			FundingRate         flexibleFloat `json:"funding_rate"`
			EstimatedRate       flexibleFloat `json:"estimated_rate"`
			FundingTime         flexibleInt   `json:"funding_time"`
			NextFundingTime     flexibleInt   `json:"next_funding_time"`
			FundingInterval     flexibleFloat `json:"funding_interval"`
			FundingRateInterval flexibleFloat `json:"funding_rate_interval"`
		} `json:"data"`
	}

	if err := c.getJSON("/linear-swap-api/v1/swap_funding_rate", q, &out); err != nil {
		return htxFundingInfo{
			ContractCode:  contractCode,
			NextFunding:   nextFundingByInterval(now, 8),
			IntervalHours: 8,
			OK:            false,
		}
	}

	if out.Status != "" && out.Status != "ok" {
		return htxFundingInfo{
			ContractCode:  contractCode,
			NextFunding:   nextFundingByInterval(now, 8),
			IntervalHours: 8,
			OK:            false,
		}
	}

	if len(out.Data) == 0 {
		return htxFundingInfo{
			ContractCode:  contractCode,
			NextFunding:   nextFundingByInterval(now, 8),
			IntervalHours: 8,
			OK:            false,
		}
	}

	d := out.Data[0]

	rate := firstNonZero(float64(d.FundingRate), float64(d.EstimatedRate))

	nextFunding := parseMillisOrSeconds(int64(d.NextFundingTime))
	if nextFunding.IsZero() {
		nextFunding = parseMillisOrSeconds(int64(d.FundingTime))
	}

	intervalHours := extractIntervalHours(d.FundingInterval, d.FundingRateInterval)
	if intervalHours <= 0 {
		intervalHours = inferIntervalFromNextFunding(now, nextFunding)
	}
	if intervalHours <= 0 {
		intervalHours = 8
	}

	if nextFunding.IsZero() {
		nextFunding = nextFundingByInterval(now, int(intervalHours))
	}

	return htxFundingInfo{
		ContractCode:  contractCode,
		Rate:          rate,
		NextFunding:   nextFunding,
		IntervalHours: intervalHours,
		OK:            true,
	}
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	contractCode := normalizeHTXContract(symbol)

	contracts, err := c.contracts()
	if err != nil {
		return domain.SymbolRules{}, err
	}

	for _, ct := range contracts {
		code := normalizeHTXContract(firstNonEmpty(ct.ContractCode, ct.Symbol))
		if code != contractCode {
			continue
		}

		base, quote := splitHTXContract(code)

		tickSize := float64(ct.PriceTick)

		return domain.SymbolRules{
			Exchange:                    c.Name(),
			Symbol:                      htxUnifiedSymbol(code),
			NativeSymbol:                code,
			SupportsNotionalMarketOrder: false,
			MinQty:                      1,
			MaxQty:                      0,
			QtyStep:                     1,
			MinNotional:                 float64(ct.MinOrderValue),
			PriceStep:                   tickSize,
			TickSize:                    tickSize,
			ContractSize:                firstNonZero(float64(ct.ContractSize), 1),
			BaseAsset:                   base,
			QuoteAsset:                  quote,
			MarginAsset:                 "USDT",
			UpdatedAt:                   time.Now().UTC(),
		}, nil
	}

	return domain.SymbolRules{}, fmt.Errorf("HTX symbol rules not found: %s", contractCode)
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	contractCode := normalizeHTXContract(symbol)

	if leverage <= 0 {
		leverage = 1
	}

	body := map[string]any{
		"contract_code": contractCode,
		"lever_rate":    leverage,
	}

	var out htxResponse[any]
	if err := c.signedPOST("/linear-swap-api/v1/swap_switch_lever_rate", body, &out); err != nil {
		return err
	}

	if out.Status != "" && out.Status != "ok" && !isIgnorableHTXMsg(firstNonEmpty(out.ErrMsg, out.Message)) {
		return fmt.Errorf("HTX set leverage status=%s err=%s", out.Status, firstNonEmpty(out.ErrMsg, out.Message))
	}

	return nil
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

	return domain.OrderResult{}, fmt.Errorf("HTX unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
}

func (c *Client) PlaceMarketShort(req domain.OrderRequest) (domain.OrderResult, error) {
	contractCode := normalizeHTXContract(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(contractCode)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		price = c.markPrice(contractCode)
	}
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("HTX %s mark price is zero", contractCode)
	}

	contractsQty := req.Qty
	if contractsQty <= 0 {
		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		contractsQty = notional / price / firstNonZero(rules.ContractSize, 1)
	}

	contractsQty = floorToStep(contractsQty, rules.QtyStep)
	if contractsQty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("HTX %s calculated contracts qty is zero", contractCode)
	}

	if rules.MinQty > 0 && contractsQty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("HTX %s qty %.12f < min qty %.12f", contractCode, contractsQty, rules.MinQty)
	}

	clientOID := htxClientOID(req.ClientOrderID)

	body := map[string]any{
		"contract_code":    contractCode,
		"volume":           int64(math.Abs(contractsQty)),
		"direction":        "sell",
		"offset":           "open",
		"lever_rate":       firstNonZeroInt(req.Leverage, 1),
		"order_price_type": "opponent",
		"client_order_id":  clientOID,
	}

	var out htxResponse[htxOrderSubmitData]
	if err := c.signedPOST("/linear-swap-api/v1/swap_order", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Status != "" && out.Status != "ok" {
		return domain.OrderResult{}, fmt.Errorf("HTX market short status=%s err=%s", out.Status, firstNonEmpty(out.ErrMsg, out.Message))
	}

	orderID := firstNonEmpty(out.Data.OrderIDStr, strconv.FormatInt(int64(out.Data.OrderID), 10))

	res := domain.OrderResult{
		Exchange:        c.Name(),
		ExchangeOrderID: orderID,
		ClientOrderID:   strconv.FormatInt(clientOID, 10),
		Symbol:          htxUnifiedSymbol(contractCode),
		NativeSymbol:    contractCode,
		Side:            "SELL",
		Type:            string(domain.OrderTypeMarket),
		Status:          "submitted",
		OrderStatus:     domain.OrderStatusNew,
		Price:           price,
		Qty:             math.Abs(contractsQty),
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}

	if orderID != "" {
		if fresh, err := c.GetOrderStatus(contractCode, orderID); err == nil {
			res = fresh
		}
	}

	return res, nil
}

func (c *Client) PlaceLimitReduceOnly(req domain.OrderRequest) (domain.OrderResult, error) {
	contractCode := normalizeHTXContract(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(contractCode)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("HTX %s limit price is zero", contractCode)
	}

	price = floorToStep(price, rules.TickSize)

	contractsQty := req.Qty
	if contractsQty <= 0 {
		ref := c.markPrice(contractCode)
		if ref <= 0 {
			ref = price
		}

		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		contractsQty = notional / ref / firstNonZero(rules.ContractSize, 1)
	}

	contractsQty = floorToStep(contractsQty, rules.QtyStep)
	if contractsQty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("HTX %s calculated contracts qty is zero", contractCode)
	}

	if rules.MinQty > 0 && contractsQty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("HTX %s qty %.12f < min qty %.12f", contractCode, contractsQty, rules.MinQty)
	}

	direction := "buy"
	offset := "close"

	if strings.EqualFold(req.Side, string(domain.OrderSideSell)) || strings.EqualFold(req.Side, "sell") {
		direction = "sell"
		offset = "close"
	}

	if !req.ReduceOnly {
		if direction == "buy" {
			offset = "open"
		} else {
			offset = "open"
		}
	}

	clientOID := htxClientOID(req.ClientOrderID)

	body := map[string]any{
		"contract_code":    contractCode,
		"volume":           int64(math.Abs(contractsQty)),
		"direction":        direction,
		"offset":           offset,
		"lever_rate":       firstNonZeroInt(req.Leverage, 1),
		"price":            formatFloat(price),
		"order_price_type": "limit",
		"client_order_id":  clientOID,
	}

	var out htxResponse[htxOrderSubmitData]
	if err := c.signedPOST("/linear-swap-api/v1/swap_order", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Status != "" && out.Status != "ok" {
		return domain.OrderResult{}, fmt.Errorf("HTX limit order status=%s err=%s", out.Status, firstNonEmpty(out.ErrMsg, out.Message))
	}

	orderID := firstNonEmpty(out.Data.OrderIDStr, strconv.FormatInt(int64(out.Data.OrderID), 10))

	return domain.OrderResult{
		Exchange:        c.Name(),
		ExchangeOrderID: orderID,
		ClientOrderID:   strconv.FormatInt(clientOID, 10),
		Symbol:          htxUnifiedSymbol(contractCode),
		NativeSymbol:    contractCode,
		Side:            strings.ToUpper(req.Side),
		Type:            string(domain.OrderTypeLimit),
		Status:          "submitted",
		OrderStatus:     domain.OrderStatusNew,
		Price:           price,
		Qty:             math.Abs(contractsQty),
		ReduceOnly:      req.ReduceOnly,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}, nil
}

func (c *Client) GetOrderStatus(symbol, orderID string) (domain.OrderResult, error) {
	contractCode := normalizeHTXContract(symbol)

	body := map[string]any{
		"contract_code": contractCode,
	}

	if isNumeric(orderID) {
		body["order_id"] = orderID
	} else {
		body["client_order_id"] = orderID
	}

	var out htxResponse[[]htxOrderData]
	if err := c.signedPOST("/linear-swap-api/v1/swap_order_info", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Status != "" && out.Status != "ok" {
		return domain.OrderResult{}, fmt.Errorf("HTX order info status=%s err=%s", out.Status, firstNonEmpty(out.ErrMsg, out.Message))
	}

	if len(out.Data) == 0 {
		return domain.OrderResult{}, fmt.Errorf("HTX order info empty: %s %s", contractCode, orderID)
	}

	rules, _ := c.SymbolRules(contractCode)

	return out.Data[0].toDomain(c.Name(), contractCode, rules.ContractSize), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	contractCode := normalizeHTXContract(symbol)

	body := map[string]any{
		"contract_code": contractCode,
		"order_id":      orderID,
	}

	var out htxResponse[any]
	if err := c.signedPOST("/linear-swap-api/v1/swap_cancel", body, &out); err != nil {
		return err
	}

	if out.Status != "" && out.Status != "ok" && !isIgnorableCancel(firstNonEmpty(out.ErrMsg, out.Message)) {
		return fmt.Errorf("HTX cancel order status=%s err=%s", out.Status, firstNonEmpty(out.ErrMsg, out.Message))
	}

	return nil
}

func (c *Client) CancelAll(symbol string) error {
	contractCode := normalizeHTXContract(symbol)

	body := map[string]any{
		"contract_code": contractCode,
	}

	var out htxResponse[any]
	if err := c.signedPOST("/linear-swap-api/v1/swap_cancelall", body, &out); err != nil {
		return err
	}

	if out.Status != "" && out.Status != "ok" && !isIgnorableCancel(firstNonEmpty(out.ErrMsg, out.Message)) {
		return fmt.Errorf("HTX cancel all status=%s err=%s", out.Status, firstNonEmpty(out.ErrMsg, out.Message))
	}

	return nil
}

func (c *Client) ClosePositionMarket(symbol string) error {
	contractCode := normalizeHTXContract(symbol)

	pos, err := c.GetPosition(contractCode)
	if err != nil {
		return err
	}

	if pos.Qty <= 0 {
		return nil
	}

	direction := "buy"
	if pos.Side == domain.SideLong {
		direction = "sell"
	}

	body := map[string]any{
		"contract_code":    contractCode,
		"volume":           int64(math.Abs(pos.Qty)),
		"direction":        direction,
		"offset":           "close",
		"lever_rate":       firstNonZeroInt(pos.Leverage, 1),
		"order_price_type": "opponent",
		"client_order_id":  htxClientOID("close-" + contractCode + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)),
	}

	var out htxResponse[htxOrderSubmitData]
	if err := c.signedPOST("/linear-swap-api/v1/swap_order", body, &out); err != nil {
		return err
	}

	if out.Status != "" && out.Status != "ok" {
		return fmt.Errorf("HTX close market status=%s err=%s", out.Status, firstNonEmpty(out.ErrMsg, out.Message))
	}

	return nil
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	contractCode := normalizeHTXContract(symbol)

	body := map[string]any{
		"contract_code": contractCode,
	}

	var out htxResponse[[]htxPositionData]
	if err := c.signedPOST("/linear-swap-api/v1/swap_position_info", body, &out); err != nil {
		return domain.PositionInfo{}, err
	}

	if out.Status != "" && out.Status != "ok" {
		if isIgnorableHTXMsg(firstNonEmpty(out.ErrMsg, out.Message)) {
			return domain.PositionInfo{
				Exchange:     c.Name(),
				Symbol:       htxUnifiedSymbol(contractCode),
				NativeSymbol: contractCode,
				UpdatedAt:    time.Now().UTC(),
			}, nil
		}

		return domain.PositionInfo{}, fmt.Errorf("HTX position status=%s err=%s", out.Status, firstNonEmpty(out.ErrMsg, out.Message))
	}

	for _, p := range out.Data {
		if normalizeHTXContract(p.ContractCode) != contractCode {
			continue
		}

		qty := math.Abs(float64(p.Volume))
		if qty <= 0 {
			continue
		}

		side := domain.SideShort
		if strings.EqualFold(p.Direction, "buy") {
			side = domain.SideLong
		}

		return domain.PositionInfo{
			Exchange:         c.Name(),
			Symbol:           htxUnifiedSymbol(contractCode),
			NativeSymbol:     contractCode,
			Side:             side,
			Qty:              qty,
			NotionalUSDT:     math.Abs(float64(p.PositionValue)),
			EntryPrice:       float64(p.CostOpen),
			MarkPrice:        float64(p.LastPrice),
			UnrealizedPNL:    float64(p.ProfitUnreal),
			RealizedPNL:      float64(p.Profit),
			LiquidationPrice: float64(p.LiquidationPrice),
			Leverage:         int(float64(p.LeverRate)),
			MarginMode:       "isolated",
			UpdatedAt:        time.Now().UTC(),
		}, nil
	}

	return domain.PositionInfo{
		Exchange:     c.Name(),
		Symbol:       htxUnifiedSymbol(contractCode),
		NativeSymbol: contractCode,
		UpdatedAt:    time.Now().UTC(),
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
	return nil, nil
}

type htxResponse[T any] struct {
	Status  string      `json:"status"`
	ErrMsg  string      `json:"err_msg"`
	Message string      `json:"message"`
	TS      flexibleInt `json:"ts"`
	Data    T           `json:"data"`
}

type htxOrderSubmitData struct {
	OrderID       flexibleInt `json:"order_id"`
	OrderIDStr    string      `json:"order_id_str"`
	ClientOrderID flexibleInt `json:"client_order_id"`
}

type htxOrderData struct {
	OrderID        flexibleInt   `json:"order_id"`
	OrderIDStr     string        `json:"order_id_str"`
	ClientOrderID  flexibleInt   `json:"client_order_id"`
	ContractCode   string        `json:"contract_code"`
	Symbol         string        `json:"symbol"`
	Volume         flexibleFloat `json:"volume"`
	Price          flexibleFloat `json:"price"`
	OrderPriceType string        `json:"order_price_type"`
	Direction      string        `json:"direction"`
	Offset         string        `json:"offset"`
	LeverRate      flexibleFloat `json:"lever_rate"`
	OrderStatus    flexibleInt   `json:"order_status"`
	TradeVolume    flexibleFloat `json:"trade_volume"`
	TradeTurnover  flexibleFloat `json:"trade_turnover"`
	Fee            flexibleFloat `json:"fee"`
	FeeAsset       string        `json:"fee_asset"`
	CreatedAt      flexibleInt   `json:"created_at"`
	CanceledAt     flexibleInt   `json:"canceled_at"`
	UpdateTime     flexibleInt   `json:"update_time"`
}

func (o htxOrderData) toDomain(exchange domain.ExchangeName, fallbackContract string, contractSize float64) domain.OrderResult {
	contract := normalizeHTXContract(firstNonEmpty(o.ContractCode, fallbackContract))

	qty := float64(o.Volume)
	filled := float64(o.TradeVolume)

	createdAt := time.Now().UTC()
	if o.CreatedAt > 0 {
		createdAt = parseMillisOrSeconds(int64(o.CreatedAt))
	}

	updatedAt := time.Now().UTC()
	if o.UpdateTime > 0 {
		updatedAt = parseMillisOrSeconds(int64(o.UpdateTime))
	} else if o.CanceledAt > 0 {
		updatedAt = parseMillisOrSeconds(int64(o.CanceledAt))
	}

	orderID := firstNonEmpty(o.OrderIDStr, strconv.FormatInt(int64(o.OrderID), 10))

	avgPrice := 0.0
	if filled > 0 && o.TradeTurnover > 0 {
		avgPrice = float64(o.TradeTurnover) / filled / firstNonZero(contractSize, 1)
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: orderID,
		ClientOrderID:   strconv.FormatInt(int64(o.ClientOrderID), 10),
		Symbol:          htxUnifiedSymbol(contract),
		NativeSymbol:    contract,
		Side:            strings.ToUpper(o.Direction),
		PositionSide:    strings.ToUpper(o.Offset),
		Type:            htxOrderType(o.OrderPriceType),
		Status:          strconv.Itoa(int(o.OrderStatus)),
		OrderStatus:     mapHTXOrderStatus(int(o.OrderStatus), qty, filled),
		Price:           float64(o.Price),
		AvgPrice:        avgPrice,
		Qty:             qty,
		FilledQty:       filled,
		FilledNotional:  float64(o.TradeTurnover),
		RemainingQty:    math.Max(0, qty-filled),
		Fee:             float64(o.Fee),
		FeeAsset:        firstNonEmpty(o.FeeAsset, "USDT"),
		ReduceOnly:      strings.EqualFold(o.Offset, "close"),
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

type htxPositionData struct {
	Symbol           string        `json:"symbol"`
	ContractCode     string        `json:"contract_code"`
	Direction        string        `json:"direction"`
	Volume           flexibleFloat `json:"volume"`
	Available        flexibleFloat `json:"available"`
	Frozen           flexibleFloat `json:"frozen"`
	CostOpen         flexibleFloat `json:"cost_open"`
	CostHold         flexibleFloat `json:"cost_hold"`
	ProfitUnreal     flexibleFloat `json:"profit_unreal"`
	Profit           flexibleFloat `json:"profit"`
	PositionMargin   flexibleFloat `json:"position_margin"`
	LeverRate        flexibleFloat `json:"lever_rate"`
	LastPrice        flexibleFloat `json:"last_price"`
	PositionValue    flexibleFloat `json:"position_value"`
	LiquidationPrice flexibleFloat `json:"liquidation_price"`
	MarginAsset      string        `json:"margin_asset"`
}

func (c *Client) markPrice(symbol string) float64 {
	contractCode := normalizeHTXContract(symbol)

	q := url.Values{}
	q.Set("contract_code", contractCode)

	var out struct {
		Status string `json:"status"`
		ErrMsg string `json:"err_msg"`
		Tick   struct {
			ContractCode string        `json:"contract_code"`
			Close        flexibleFloat `json:"close"`
			MarkPrice    flexibleFloat `json:"mark_price"`
			IndexPrice   flexibleFloat `json:"index_price"`
		} `json:"tick"`
	}

	if err := c.getJSON("/linear-swap-ex/market/detail/merged", q, &out); err != nil {
		return 0
	}

	if out.Status != "" && out.Status != "ok" {
		return 0
	}

	return firstNonZero(float64(out.Tick.MarkPrice), float64(out.Tick.IndexPrice), float64(out.Tick.Close))
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
		return fmt.Errorf("HTX GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("HTX GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedPOST(path string, body any, dst any) error {
	return c.signedRequest(http.MethodPost, path, nil, body, dst)
}

func (c *Client) signedRequest(method, path string, q url.Values, body any, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("HTX API key or secret is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	base, err := url.Parse(c.baseURL)
	if err != nil {
		return err
	}

	q.Set("AccessKeyId", c.apiKey)
	q.Set("SignatureMethod", "HmacSHA256")
	q.Set("SignatureVersion", "2")
	q.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05"))

	canonicalQuery := q.Encode()

	payload := strings.Join([]string{
		method,
		base.Host,
		path,
		canonicalQuery,
	}, "\n")

	signature := c.sign(payload)
	q.Set("Signature", signature)

	requestPath := path + "?" + q.Encode()
	u := c.baseURL + requestPath

	bodyBytes := []byte{}
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

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

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTX signed %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("HTX signed %s %s decode error: %w; body=%s", method, path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
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

func normalizeHTXContract(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))

	if strings.Contains(s, "-") {
		return s
	}

	if strings.HasSuffix(s, "USDT") {
		return strings.TrimSuffix(s, "USDT") + "-USDT"
	}

	return s
}

func htxUnifiedSymbol(contractCode string) string {
	return strings.ReplaceAll(strings.ToUpper(strings.TrimSpace(contractCode)), "-", "")
}

func splitHTXContract(contractCode string) (string, string) {
	parts := strings.Split(strings.ToUpper(strings.TrimSpace(contractCode)), "-")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}

	if strings.HasSuffix(contractCode, "USDT") {
		return strings.TrimSuffix(contractCode, "USDT"), "USDT"
	}

	return "", "USDT"
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

func inferIntervalFromNextFunding(now time.Time, next time.Time) float64 {
	if next.IsZero() {
		return 0
	}

	next = next.UTC()

	if next.Minute() != 0 || next.Second() != 0 {
		return 0
	}

	h := next.Hour()

	if h%4 != 0 {
		return 1
	}

	if h%8 != 0 {
		return 4
	}

	return 8
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

func htxClientOID(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Now().UnixMilli()
	}

	var digits strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}

	out := digits.String()
	if out == "" {
		return time.Now().UnixMilli()
	}

	if len(out) > 18 {
		out = out[len(out)-18:]
	}

	v, err := strconv.ParseInt(out, 10, 64)
	if err != nil || v <= 0 {
		return time.Now().UnixMilli()
	}

	return v
}

func htxOrderType(orderPriceType string) string {
	orderPriceType = strings.ToLower(strings.TrimSpace(orderPriceType))
	if strings.Contains(orderPriceType, "opponent") || strings.Contains(orderPriceType, "market") {
		return string(domain.OrderTypeMarket)
	}

	return string(domain.OrderTypeLimit)
}

func mapHTXOrderStatus(status int, qty float64, filled float64) domain.OrderStatus {
	switch status {
	case 1, 2, 3:
		if filled > 0 && filled < qty {
			return domain.OrderStatusPartiallyFill
		}

		return domain.OrderStatusNew
	case 4:
		return domain.OrderStatusFilled
	case 5, 6, 7:
		if filled > 0 && filled < qty {
			return domain.OrderStatusPartiallyFill
		}

		return domain.OrderStatusCanceled
	default:
		return domain.OrderStatusUnknown
	}
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}

	_, err := strconv.ParseInt(s, 10, 64)

	return err == nil
}

func firstNonZeroInt(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}

	return 0
}

func isIgnorableHTXMsg(msg string) bool {
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

func isIgnorableCancel(msg string) bool {
	msg = strings.ToLower(msg)

	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "not exist") ||
		strings.Contains(msg, "already") ||
		strings.Contains(msg, "finished") ||
		strings.Contains(msg, "canceled") ||
		strings.Contains(msg, "cancelled")
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

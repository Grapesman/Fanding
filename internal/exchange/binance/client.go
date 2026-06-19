package binance

import (
	"crypto/hmac"
	"crypto/sha256"
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
	return domain.ExchangeBinance
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		ServerTime int64 `json:"serverTime"`
	}

	if err := c.getJSON("/fapi/v1/time", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.ServerTime <= 0 {
		return time.Time{}, fmt.Errorf("binance server time is empty")
	}

	return time.UnixMilli(out.ServerTime), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Binance API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	params := url.Values{}

	var rows []struct {
		AccountAlias       string `json:"accountAlias"`
		Asset              string `json:"asset"`
		Balance            string `json:"balance"`
		CrossWalletBalance string `json:"crossWalletBalance"`
		CrossUnPnl         string `json:"crossUnPnl"`
		AvailableBalance   string `json:"availableBalance"`
		MaxWithdrawAmount  string `json:"maxWithdrawAmount"`
		MarginAvailable    bool   `json:"marginAvailable"`
		UpdateTime         int64  `json:"updateTime"`
	}

	if err := c.signedGET("/fapi/v2/balance", params, &rows); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Binance futures balance error: " + err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	for _, r := range rows {
		if strings.EqualFold(r.Asset, "USDT") {
			wallet := parseFloat(r.Balance)
			available := parseFloat(r.AvailableBalance)

			return domain.Balance{
				Exchange:      c.Name(),
				WalletUSDT:    wallet,
				AvailableUSDT: available,
				PrivateOK:     true,
				Error:         "",
				UpdatedAt:     time.Now().UTC(),
			}, nil
		}
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "USDT futures balance not found",
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	intervalBySymbol := c.fundingIntervals()

	var premium []struct {
		Symbol          string `json:"symbol"`
		MarkPrice       string `json:"markPrice"`
		IndexPrice      string `json:"indexPrice"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
	}

	if err := c.getJSON("/fapi/v1/premiumIndex", nil, &premium); err != nil {
		return nil, err
	}

	var tickers []struct {
		Symbol      string `json:"symbol"`
		LastPrice   string `json:"lastPrice"`
		QuoteVolume string `json:"quoteVolume"`
	}

	_ = c.getJSON("/fapi/v1/ticker/24hr", nil, &tickers)

	volumeBySymbol := map[string]float64{}
	lastBySymbol := map[string]float64{}

	for _, t := range tickers {
		volumeBySymbol[t.Symbol] = parseFloat(t.QuoteVolume)
		lastBySymbol[t.Symbol] = parseFloat(t.LastPrice)
	}

	bidBySymbol := map[string]float64{}
	askBySymbol := map[string]float64{}

	var books []struct {
		Symbol   string `json:"symbol"`
		BidPrice string `json:"bidPrice"`
		AskPrice string `json:"askPrice"`
	}

	_ = c.getJSON("/fapi/v1/ticker/bookTicker", nil, &books)

	for _, b := range books {
		bidBySymbol[b.Symbol] = parseFloat(b.BidPrice)
		askBySymbol[b.Symbol] = parseFloat(b.AskPrice)
	}

	out := make([]domain.Candidate, 0, len(premium))
	now := time.Now().UTC()

	for _, p := range premium {
		if !strings.HasSuffix(p.Symbol, "USDT") {
			continue
		}

		mark := parseFloat(p.MarkPrice)
		fr := parseFloat(p.LastFundingRate)

		price := lastBySymbol[p.Symbol]
		if price <= 0 {
			price = mark
		}

		bid := bidBySymbol[p.Symbol]
		ask := askBySymbol[p.Symbol]

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		intervalHours := intervalBySymbol[p.Symbol]
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := time.Time{}
		if p.NextFundingTime > 0 {
			nextFunding = time.UnixMilli(p.NextFundingTime).UTC()
		} else {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		out = append(out, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               p.Symbol,
			NativeSymbol:         p.Symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          fr,
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        volumeBySymbol[p.Symbol],
			Bid:                  bid,
			Ask:                  ask,
			Spread:               spread,
			UpdatedAt:            now,
		})
	}

	return out, nil
}

func (c *Client) fundingIntervals() map[string]float64 {
	out := map[string]float64{}

	var rows []struct {
		Symbol                   string  `json:"symbol"`
		AdjustedFundingRateCap   string  `json:"adjustedFundingRateCap"`
		AdjustedFundingRateFloor string  `json:"adjustedFundingRateFloor"`
		FundingIntervalHours     float64 `json:"fundingIntervalHours"`
		Disclaimer               bool    `json:"disclaimer"`
	}

	if err := c.getJSON("/fapi/v1/fundingInfo", nil, &rows); err != nil {
		return out
	}

	for _, r := range rows {
		if r.Symbol == "" || r.FundingIntervalHours <= 0 {
			continue
		}

		out[r.Symbol] = r.FundingIntervalHours
	}

	return out
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	symbol = normalizeSymbol(symbol)

	var out struct {
		Symbols []struct {
			Symbol       string `json:"symbol"`
			Status       string `json:"status"`
			ContractType string `json:"contractType"`
			BaseAsset    string `json:"baseAsset"`
			QuoteAsset   string `json:"quoteAsset"`
			MarginAsset  string `json:"marginAsset"`
			Filters      []struct {
				FilterType  string `json:"filterType"`
				MinPrice    string `json:"minPrice"`
				MaxPrice    string `json:"maxPrice"`
				TickSize    string `json:"tickSize"`
				MinQty      string `json:"minQty"`
				MaxQty      string `json:"maxQty"`
				StepSize    string `json:"stepSize"`
				Notional    string `json:"notional"`
				MinNotional string `json:"minNotional"`
			} `json:"filters"`
		} `json:"symbols"`
	}

	if err := c.getJSON("/fapi/v1/exchangeInfo", nil, &out); err != nil {
		return domain.SymbolRules{}, err
	}

	for _, s := range out.Symbols {
		if s.Symbol != symbol {
			continue
		}

		rules := domain.SymbolRules{
			Exchange:                    c.Name(),
			Symbol:                      symbol,
			NativeSymbol:                symbol,
			SupportsNotionalMarketOrder: false,
			BaseAsset:                   s.BaseAsset,
			QuoteAsset:                  s.QuoteAsset,
			MarginAsset:                 s.MarginAsset,
			ContractSize:                1,
			UpdatedAt:                   time.Now().UTC(),
		}

		for _, f := range s.Filters {
			switch f.FilterType {
			case "PRICE_FILTER":
				rules.PriceStep = parseFloat(f.TickSize)
				rules.TickSize = parseFloat(f.TickSize)

			case "LOT_SIZE":
				rules.MinQty = parseFloat(f.MinQty)
				rules.MaxQty = parseFloat(f.MaxQty)
				rules.QtyStep = parseFloat(f.StepSize)

			case "MARKET_LOT_SIZE":
				marketMinQty := parseFloat(f.MinQty)
				marketMaxQty := parseFloat(f.MaxQty)
				marketStep := parseFloat(f.StepSize)

				if marketMinQty > rules.MinQty {
					rules.MinQty = marketMinQty
				}
				if marketMaxQty > 0 {
					rules.MaxQty = marketMaxQty
				}
				if marketStep > 0 {
					rules.QtyStep = marketStep
				}

			case "MIN_NOTIONAL":
				rules.MinNotional = firstNonZero(parseFloat(f.Notional), parseFloat(f.MinNotional))
			}
		}

		if rules.QtyStep <= 0 {
			rules.QtyStep = 0.001
		}
		if rules.TickSize <= 0 {
			rules.TickSize = rules.PriceStep
		}

		return rules, nil
	}

	return domain.SymbolRules{}, fmt.Errorf("binance symbol rules not found: %s", symbol)
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	symbol = normalizeSymbol(symbol)

	if leverage <= 0 {
		leverage = 1
	}

	marginMode = strings.ToUpper(strings.TrimSpace(marginMode))
	if marginMode == "" {
		marginMode = "ISOLATED"
	}

	marginParams := url.Values{}
	marginParams.Set("symbol", symbol)
	marginParams.Set("marginType", marginMode)

	var marginOut any
	if err := c.signedPOST("/fapi/v1/marginType", marginParams, &marginOut); err != nil {
		errText := strings.ToLower(err.Error())

		// Binance code -4046: No need to change margin type.
		if !strings.Contains(errText, "-4046") && !strings.Contains(errText, "no need to change margin type") {
			return err
		}
	}

	levParams := url.Values{}
	levParams.Set("symbol", symbol)
	levParams.Set("leverage", strconv.Itoa(leverage))

	var levOut any
	if err := c.signedPOST("/fapi/v1/leverage", levParams, &levOut); err != nil {
		return err
	}

	return nil
}

func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	if strings.EqualFold(req.Type, string(domain.OrderTypeMarket)) && strings.EqualFold(req.Side, string(domain.OrderSideSell)) && !req.ReduceOnly {
		return c.PlaceMarketShort(req)
	}

	if strings.EqualFold(req.Type, string(domain.OrderTypeLimit)) {
		return c.PlaceLimitReduceOnly(req)
	}

	return domain.OrderResult{}, fmt.Errorf("binance unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
}

func (c *Client) PlaceMarketShort(req domain.OrderRequest) (domain.OrderResult, error) {
	symbol := normalizeSymbol(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(symbol)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		price = c.markPrice(symbol)
	}

	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("binance %s mark price is zero", symbol)
	}

	qty := req.Qty
	if qty <= 0 {
		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		qty = notional / price
	}

	qty = floorToStep(qty, rules.QtyStep)

	if qty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("binance %s calculated qty is zero", symbol)
	}

	notional := qty * price

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("binance %s qty %.12f < min qty %.12f", symbol, qty, rules.MinQty)
	}

	if rules.MinNotional > 0 && notional < rules.MinNotional {
		return domain.OrderResult{}, fmt.Errorf("binance %s notional %.8f < min notional %.8f", symbol, notional, rules.MinNotional)
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", "SELL")
	params.Set("type", "MARKET")
	params.Set("quantity", formatFloat(qty))
	params.Set("newOrderRespType", "RESULT")

	if req.ClientOrderID != "" {
		params.Set("newClientOrderId", req.ClientOrderID)
	}

	var raw binanceOrder

	if err := c.signedPOST("/fapi/v1/order", params, &raw); err != nil {
		return domain.OrderResult{}, err
	}

	res := raw.toDomain(c.Name())

	if !orderFilled(res) && res.ExchangeOrderID != "" {
		if fresh, err := c.GetOrderStatus(symbol, res.ExchangeOrderID); err == nil {
			res = fresh
		}
	}

	return res, nil
}

func (c *Client) PlaceLimitReduceOnly(req domain.OrderRequest) (domain.OrderResult, error) {
	symbol := normalizeSymbol(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(symbol)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("binance %s limit price is zero", symbol)
	}

	if rules.TickSize > 0 {
		price = floorToStep(price, rules.TickSize)
	}

	qty := req.Qty
	if qty <= 0 {
		ref := c.markPrice(symbol)
		if ref <= 0 {
			ref = price
		}
		qty = firstNonZero(req.NotionalUSDT, req.MarginUSDT) / ref
	}

	qty = floorToStep(qty, rules.QtyStep)

	if qty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("binance %s calculated limit qty is zero", symbol)
	}

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("binance %s qty %.12f < min qty %.12f", symbol, qty, rules.MinQty)
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", strings.ToUpper(req.Side))
	params.Set("type", "LIMIT")
	params.Set("timeInForce", firstNonEmpty(req.TimeInForce, "GTC"))
	params.Set("quantity", formatFloat(qty))
	params.Set("price", formatFloat(price))
	params.Set("reduceOnly", boolString(req.ReduceOnly))
	params.Set("newOrderRespType", "RESULT")

	if req.ClientOrderID != "" {
		params.Set("newClientOrderId", req.ClientOrderID)
	}

	var raw binanceOrder

	if err := c.signedPOST("/fapi/v1/order", params, &raw); err != nil {
		return domain.OrderResult{}, err
	}

	return raw.toDomain(c.Name()), nil
}

func (c *Client) GetOrderStatus(symbol, orderID string) (domain.OrderResult, error) {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	if isInt(orderID) {
		params.Set("orderId", orderID)
	} else {
		params.Set("origClientOrderId", orderID)
	}

	var raw binanceOrder

	if err := c.signedGET("/fapi/v1/order", params, &raw); err != nil {
		return domain.OrderResult{}, err
	}

	return raw.toDomain(c.Name()), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	if isInt(orderID) {
		params.Set("orderId", orderID)
	} else {
		params.Set("origClientOrderId", orderID)
	}

	var out any

	return c.signedDELETE("/fapi/v1/order", params, &out)
}

func (c *Client) CancelAll(symbol string) error {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	var out any

	return c.signedDELETE("/fapi/v1/allOpenOrders", params, &out)
}

func (c *Client) ClosePositionMarket(symbol string) error {
	symbol = normalizeSymbol(symbol)

	pos, err := c.GetPosition(symbol)
	if err != nil {
		return err
	}

	if math.Abs(pos.Qty) <= 0 {
		return nil
	}

	side := "BUY"
	if pos.Side == domain.SideLong {
		side = "SELL"
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", side)
	params.Set("type", "MARKET")
	params.Set("quantity", formatFloat(math.Abs(pos.Qty)))
	params.Set("reduceOnly", "true")
	params.Set("newOrderRespType", "RESULT")

	var out any

	return c.signedPOST("/fapi/v1/order", params, &out)
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	var rows []struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		EntryPrice       string `json:"entryPrice"`
		BreakEvenPrice   string `json:"breakEvenPrice"`
		MarkPrice        string `json:"markPrice"`
		UnRealizedProfit string `json:"unRealizedProfit"`
		LiquidationPrice string `json:"liquidationPrice"`
		Leverage         string `json:"leverage"`
		MarginType       string `json:"marginType"`
		PositionSide     string `json:"positionSide"`
		UpdateTime       int64  `json:"updateTime"`
	}

	if err := c.signedGET("/fapi/v2/positionRisk", params, &rows); err != nil {
		return domain.PositionInfo{}, err
	}

	for _, r := range rows {
		if r.Symbol != symbol {
			continue
		}

		qty := parseFloat(r.PositionAmt)

		side := domain.SideShort
		if qty > 0 {
			side = domain.SideLong
		}

		lev, _ := strconv.Atoi(r.Leverage)

		return domain.PositionInfo{
			Exchange:         c.Name(),
			Symbol:           symbol,
			NativeSymbol:     symbol,
			Side:             side,
			Qty:              math.Abs(qty),
			NotionalUSDT:     math.Abs(qty) * parseFloat(r.MarkPrice),
			EntryPrice:       parseFloat(r.EntryPrice),
			MarkPrice:        parseFloat(r.MarkPrice),
			UnrealizedPNL:    parseFloat(r.UnRealizedProfit),
			LiquidationPrice: parseFloat(r.LiquidationPrice),
			Leverage:         lev,
			MarginMode:       strings.ToLower(r.MarginType),
			UpdatedAt:        time.Now().UTC(),
		}, nil
	}

	return domain.PositionInfo{
		Exchange:     c.Name(),
		Symbol:       symbol,
		NativeSymbol: symbol,
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
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("incomeType", "FUNDING_FEE")
	params.Set("limit", "1000")

	if !from.IsZero() {
		params.Set("startTime", strconv.FormatInt(from.UTC().UnixMilli(), 10))
	}
	if !to.IsZero() {
		params.Set("endTime", strconv.FormatInt(to.UTC().UnixMilli(), 10))
	}

	var rows []struct {
		Symbol     string `json:"symbol"`
		Income     string `json:"income"`
		IncomeType string `json:"incomeType"`
		Asset      string `json:"asset"`
		Info       string `json:"info"`
		Time       int64  `json:"time"`
		TranID     string `json:"tranId"`
		TradeID    string `json:"tradeId"`
	}

	if err := c.signedGET("/fapi/v1/income", params, &rows); err != nil {
		return nil, err
	}

	out := make([]domain.FundingFeeInfo, 0, len(rows))

	for _, r := range rows {
		if r.Symbol != "" && r.Symbol != symbol {
			continue
		}

		out = append(out, domain.FundingFeeInfo{
			Exchange:     c.Name(),
			Symbol:       symbol,
			NativeSymbol: symbol,
			Amount:       parseFloat(r.Income),
			Asset:        firstNonEmpty(r.Asset, "USDT"),
			IncomeType:   r.IncomeType,
			FeeTime:      time.UnixMilli(r.Time).UTC(),
			Raw:          r.Info,
		})
	}

	return out, nil
}

func (c *Client) markPrice(symbol string) float64 {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	var out struct {
		Symbol    string `json:"symbol"`
		MarkPrice string `json:"markPrice"`
	}

	if err := c.getJSON("/fapi/v1/premiumIndex", params, &out); err != nil {
		return 0
	}

	return parseFloat(out.MarkPrice)
}

func (c *Client) getJSON(path string, params url.Values, dst any) error {
	fullURL := c.baseURL + path

	if params != nil && len(params) > 0 {
		fullURL += "?" + params.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("binance GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	return json.Unmarshal(body, dst)
}

func (c *Client) signedGET(path string, params url.Values, dst any) error {
	return c.signedRequest(http.MethodGet, path, params, dst)
}

func (c *Client) signedPOST(path string, params url.Values, dst any) error {
	return c.signedRequest(http.MethodPost, path, params, dst)
}

func (c *Client) signedDELETE(path string, params url.Values, dst any) error {
	return c.signedRequest(http.MethodDelete, path, params, dst)
}

func (c *Client) signedRequest(method, path string, params url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("binance api key or secret is empty")
	}

	if params == nil {
		params = url.Values{}
	}

	serverTime, err := c.ServerTime()
	if err != nil {
		return err
	}

	params.Set("timestamp", strconv.FormatInt(serverTime.UnixMilli(), 10))
	params.Set("recvWindow", "5000")

	query := params.Encode()
	signature := c.sign(query)
	query += "&signature=" + signature

	var body io.Reader
	fullURL := c.baseURL + path

	if method == http.MethodGet || method == http.MethodDelete {
		fullURL += "?" + query
	} else {
		body = strings.NewReader(query)
	}

	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return err
	}

	req.Header.Set("X-MBX-APIKEY", c.apiKey)

	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("binance %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil || len(raw) == 0 {
		return nil
	}

	return json.Unmarshal(raw, dst)
}

func (c *Client) sign(query string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

type binanceOrder struct {
	OrderID       int64  `json:"orderId"`
	Symbol        string `json:"symbol"`
	Status        string `json:"status"`
	ClientOrderID string `json:"clientOrderId"`
	Price         string `json:"price"`
	AvgPrice      string `json:"avgPrice"`
	OrigQty       string `json:"origQty"`
	ExecutedQty   string `json:"executedQty"`
	CumQuote      string `json:"cumQuote"`
	Side          string `json:"side"`
	Type          string `json:"type"`
	ReduceOnly    bool   `json:"reduceOnly"`
	UpdateTime    int64  `json:"updateTime"`
	Time          int64  `json:"time"`
}

func (o binanceOrder) toDomain(exchange domain.ExchangeName) domain.OrderResult {
	status := mapOrderStatus(o.Status)

	createdAt := time.Now().UTC()
	if o.Time > 0 {
		createdAt = time.UnixMilli(o.Time).UTC()
	} else if o.UpdateTime > 0 {
		createdAt = time.UnixMilli(o.UpdateTime).UTC()
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: strconv.FormatInt(o.OrderID, 10),
		ClientOrderID:   o.ClientOrderID,
		Symbol:          o.Symbol,
		NativeSymbol:    o.Symbol,
		Side:            o.Side,
		Type:            o.Type,
		Status:          o.Status,
		OrderStatus:     status,
		Price:           parseFloat(o.Price),
		AvgPrice:        parseFloat(o.AvgPrice),
		Qty:             parseFloat(o.OrigQty),
		FilledQty:       parseFloat(o.ExecutedQty),
		FilledNotional:  parseFloat(o.CumQuote),
		RemainingQty:    math.Max(0, parseFloat(o.OrigQty)-parseFloat(o.ExecutedQty)),
		ReduceOnly:      o.ReduceOnly,
		CreatedAt:       createdAt,
		UpdatedAt:       time.Now().UTC(),
	}
}

func mapOrderStatus(s string) domain.OrderStatus {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "NEW":
		return domain.OrderStatusNew
	case "PARTIALLY_FILLED":
		return domain.OrderStatusPartiallyFill
	case "FILLED":
		return domain.OrderStatusFilled
	case "CANCELED", "CANCELLED":
		return domain.OrderStatusCanceled
	case "REJECTED":
		return domain.OrderStatusRejected
	case "EXPIRED":
		return domain.OrderStatusExpired
	default:
		if s == "" {
			return domain.OrderStatusUnknown
		}

		return domain.OrderStatusUnknown
	}
}

func orderFilled(r domain.OrderResult) bool {
	return r.OrderStatus == domain.OrderStatusFilled ||
		strings.EqualFold(r.Status, "FILLED")
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

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func normalizeSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
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
	if v == 0 {
		return "0"
	}

	return strconv.FormatFloat(v, 'f', -1, 64)
}

func boolString(v bool) string {
	if v {
		return "true"
	}

	return "false"
}

func isInt(s string) bool {
	if s == "" {
		return false
	}

	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

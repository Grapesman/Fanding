package bybit

import (
	"bytes"
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

const (
	categoryLinear = "linear"
	recvWindow     = "10000"
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
	return domain.ExchangeBybit
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out bybitResponse[struct {
		TimeSecond flexibleInt `json:"timeSecond"`
		TimeNano   string      `json:"timeNano"`
	}]

	if err := c.getJSON("/v5/market/time", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.RetCode != 0 {
		return time.Time{}, fmt.Errorf("bybit server time retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	if out.Time > 0 {
		return time.UnixMilli(int64(out.Time)), nil
	}

	if out.Result.TimeSecond > 0 {
		return time.Unix(int64(out.Result.TimeSecond), 0), nil
	}

	return time.Now().UTC(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bybit API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	b, err := c.balanceByAccountType("UNIFIED")
	if err == nil && b.PrivateOK {
		return b, nil
	}

	contractBalance, contractErr := c.balanceByAccountType("CONTRACT")
	if contractErr == nil && contractBalance.PrivateOK {
		return contractBalance, nil
	}

	if err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bybit futures balance error: " + err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	if contractErr != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bybit futures balance error: " + contractErr.Error(),
			UpdatedAt: time.Now().UTC(),
		}, contractErr
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "Bybit USDT futures balance not found",
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (c *Client) balanceByAccountType(accountType string) (domain.Balance, error) {
	q := url.Values{}
	q.Set("accountType", accountType)
	q.Set("coin", "USDT")

	var out bybitResponse[struct {
		List []struct {
			AccountType           string        `json:"accountType"`
			TotalEquity           flexibleFloat `json:"totalEquity"`
			TotalWalletBalance    flexibleFloat `json:"totalWalletBalance"`
			TotalMarginBalance    flexibleFloat `json:"totalMarginBalance"`
			TotalAvailableBalance flexibleFloat `json:"totalAvailableBalance"`
			Coin                  []struct {
				Coin                string        `json:"coin"`
				Equity              flexibleFloat `json:"equity"`
				WalletBalance       flexibleFloat `json:"walletBalance"`
				AvailableToWithdraw flexibleFloat `json:"availableToWithdraw"`
				AvailableToBorrow   flexibleFloat `json:"availableToBorrow"`
				UsdValue            flexibleFloat `json:"usdValue"`
			} `json:"coin"`
		} `json:"list"`
	}]

	if err := c.signedGET("/v5/account/wallet-balance", q, &out); err != nil {
		return domain.Balance{}, err
	}

	if out.RetCode != 0 {
		return domain.Balance{}, fmt.Errorf("Bybit balance accountType=%s retCode=%d retMsg=%s", accountType, out.RetCode, out.RetMsg)
	}

	for _, account := range out.Result.List {
		wallet := float64(account.TotalWalletBalance)
		available := float64(account.TotalAvailableBalance)

		for _, coin := range account.Coin {
			if strings.EqualFold(coin.Coin, "USDT") {
				if wallet <= 0 {
					wallet = float64(coin.WalletBalance)
				}

				coinAvailable := float64(coin.AvailableToWithdraw)
				if coinAvailable > 0 {
					available = coinAvailable
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
		}

		if wallet > 0 || available > 0 {
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

	return domain.Balance{}, fmt.Errorf("Bybit USDT balance not found for accountType=%s", accountType)
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	intervalBySymbol := c.fundingIntervals()

	q := url.Values{}
	q.Set("category", categoryLinear)

	var out bybitResponse[struct {
		Category string `json:"category"`
		List     []struct {
			Symbol                 string        `json:"symbol"`
			LastPrice              flexibleFloat `json:"lastPrice"`
			IndexPrice             flexibleFloat `json:"indexPrice"`
			MarkPrice              flexibleFloat `json:"markPrice"`
			PrevPrice24h           flexibleFloat `json:"prevPrice24h"`
			Price24hPcnt           flexibleFloat `json:"price24hPcnt"`
			HighPrice24h           flexibleFloat `json:"highPrice24h"`
			LowPrice24h            flexibleFloat `json:"lowPrice24h"`
			PrevPrice1h            flexibleFloat `json:"prevPrice1h"`
			OpenInterest           flexibleFloat `json:"openInterest"`
			OpenInterestValue      flexibleFloat `json:"openInterestValue"`
			Turnover24h            flexibleFloat `json:"turnover24h"`
			Volume24h              flexibleFloat `json:"volume24h"`
			FundingRate            flexibleFloat `json:"fundingRate"`
			NextFundingTime        flexibleInt   `json:"nextFundingTime"`
			PredictedDeliveryPrice flexibleFloat `json:"predictedDeliveryPrice"`
			BasisRate              flexibleFloat `json:"basisRate"`
			Bid1Price              flexibleFloat `json:"bid1Price"`
			Bid1Size               flexibleFloat `json:"bid1Size"`
			Ask1Price              flexibleFloat `json:"ask1Price"`
			Ask1Size               flexibleFloat `json:"ask1Size"`
		} `json:"list"`
	}]

	if err := c.getJSON("/v5/market/tickers", q, &out); err != nil {
		return nil, err
	}

	if out.RetCode != 0 {
		return nil, fmt.Errorf("Bybit tickers retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	now := time.Now().UTC()
	rows := make([]domain.Candidate, 0, len(out.Result.List))

	for _, t := range out.Result.List {
		if !strings.HasSuffix(t.Symbol, "USDT") {
			continue
		}

		mark := float64(t.MarkPrice)
		price := float64(t.LastPrice)

		if price <= 0 {
			price = mark
		}

		bid := float64(t.Bid1Price)
		ask := float64(t.Ask1Price)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		intervalHours := intervalBySymbol[t.Symbol]
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := time.Time{}
		if t.NextFundingTime > 0 {
			nextFunding = time.UnixMilli(int64(t.NextFundingTime)).UTC()
		} else {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		rows = append(rows, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               t.Symbol,
			NativeSymbol:         t.Symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          float64(t.FundingRate),
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        float64(t.Turnover24h),
			Bid:                  bid,
			Ask:                  ask,
			Spread:               spread,
			UpdatedAt:            now,
		})
	}

	return rows, nil
}

func (c *Client) fundingIntervals() map[string]float64 {
	out := map[string]float64{}
	cursor := ""

	for {
		q := url.Values{}
		q.Set("category", categoryLinear)
		q.Set("limit", "1000")

		if cursor != "" {
			q.Set("cursor", cursor)
		}

		var resp bybitResponse[struct {
			Category       string `json:"category"`
			NextPageCursor string `json:"nextPageCursor"`
			List           []struct {
				Symbol          string      `json:"symbol"`
				ContractType    string      `json:"contractType"`
				Status          string      `json:"status"`
				QuoteCoin       string      `json:"quoteCoin"`
				SettleCoin      string      `json:"settleCoin"`
				FundingInterval flexibleInt `json:"fundingInterval"`
			} `json:"list"`
		}]

		if err := c.getJSON("/v5/market/instruments-info", q, &resp); err != nil {
			return out
		}

		if resp.RetCode != 0 {
			return out
		}

		for _, s := range resp.Result.List {
			if s.Symbol == "" || !strings.EqualFold(s.QuoteCoin, "USDT") {
				continue
			}

			minutes := int64(s.FundingInterval)
			if minutes <= 0 {
				continue
			}

			out[s.Symbol] = float64(minutes) / 60.0
		}

		if resp.Result.NextPageCursor == "" || resp.Result.NextPageCursor == cursor {
			break
		}

		cursor = resp.Result.NextPageCursor
	}

	return out
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	symbol = normalizeSymbol(symbol)

	q := url.Values{}
	q.Set("category", categoryLinear)
	q.Set("symbol", symbol)

	var out bybitResponse[struct {
		Category string `json:"category"`
		List     []struct {
			Symbol       string `json:"symbol"`
			ContractType string `json:"contractType"`
			Status       string `json:"status"`
			BaseCoin     string `json:"baseCoin"`
			QuoteCoin    string `json:"quoteCoin"`
			SettleCoin   string `json:"settleCoin"`
			PriceFilter  struct {
				MinPrice flexibleFloat `json:"minPrice"`
				MaxPrice flexibleFloat `json:"maxPrice"`
				TickSize flexibleFloat `json:"tickSize"`
			} `json:"priceFilter"`
			LotSizeFilter struct {
				MaxOrderQty         flexibleFloat `json:"maxOrderQty"`
				MinOrderQty         flexibleFloat `json:"minOrderQty"`
				QtyStep             flexibleFloat `json:"qtyStep"`
				MinNotionalValue    flexibleFloat `json:"minNotionalValue"`
				MaxMktOrderQty      flexibleFloat `json:"maxMktOrderQty"`
				PostOnlyMaxOrderQty flexibleFloat `json:"postOnlyMaxOrderQty"`
			} `json:"lotSizeFilter"`
		} `json:"list"`
	}]

	if err := c.getJSON("/v5/market/instruments-info", q, &out); err != nil {
		return domain.SymbolRules{}, err
	}

	if out.RetCode != 0 {
		return domain.SymbolRules{}, fmt.Errorf("Bybit instruments-info retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	for _, s := range out.Result.List {
		if s.Symbol != symbol {
			continue
		}

		rules := domain.SymbolRules{
			Exchange:                    c.Name(),
			Symbol:                      symbol,
			NativeSymbol:                symbol,
			SupportsNotionalMarketOrder: false,
			MinQty:                      float64(s.LotSizeFilter.MinOrderQty),
			MaxQty:                      firstNonZero(float64(s.LotSizeFilter.MaxMktOrderQty), float64(s.LotSizeFilter.MaxOrderQty)),
			QtyStep:                     float64(s.LotSizeFilter.QtyStep),
			MinNotional:                 float64(s.LotSizeFilter.MinNotionalValue),
			PriceStep:                   float64(s.PriceFilter.TickSize),
			TickSize:                    float64(s.PriceFilter.TickSize),
			ContractSize:                1,
			BaseAsset:                   s.BaseCoin,
			QuoteAsset:                  s.QuoteCoin,
			MarginAsset:                 s.SettleCoin,
			UpdatedAt:                   time.Now().UTC(),
		}

		if rules.QtyStep <= 0 {
			rules.QtyStep = 0.001
		}

		if rules.TickSize <= 0 {
			rules.TickSize = rules.PriceStep
		}

		return rules, nil
	}

	return domain.SymbolRules{}, fmt.Errorf("Bybit symbol rules not found: %s", symbol)
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	symbol = normalizeSymbol(symbol)

	if leverage <= 0 {
		leverage = 1
	}

	marginMode = strings.ToLower(strings.TrimSpace(marginMode))
	if marginMode == "" {
		marginMode = "isolated"
	}

	if marginMode == "isolated" {
		body := map[string]string{
			"category":     categoryLinear,
			"symbol":       symbol,
			"tradeMode":    "1",
			"buyLeverage":  strconv.Itoa(leverage),
			"sellLeverage": strconv.Itoa(leverage),
		}

		var out bybitResponse[any]
		err := c.signedPOST("/v5/position/switch-isolated", body, &out)
		if err != nil && !isIgnorablePositionConfigError(err) {
			return err
		}

		if out.RetCode != 0 && !isIgnorableBybitRet(out.RetCode, out.RetMsg) {
			return fmt.Errorf("Bybit switch isolated retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
		}
	}

	body := map[string]string{
		"category":     categoryLinear,
		"symbol":       symbol,
		"buyLeverage":  strconv.Itoa(leverage),
		"sellLeverage": strconv.Itoa(leverage),
	}

	var out bybitResponse[any]
	err := c.signedPOST("/v5/position/set-leverage", body, &out)
	if err != nil && !isIgnorablePositionConfigError(err) {
		return err
	}

	if out.RetCode != 0 && !isIgnorableBybitRet(out.RetCode, out.RetMsg) {
		return fmt.Errorf("Bybit set leverage retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
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

	return domain.OrderResult{}, fmt.Errorf("Bybit unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
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
		return domain.OrderResult{}, fmt.Errorf("Bybit %s mark price is zero", symbol)
	}

	qty := req.Qty
	if qty <= 0 {
		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		qty = notional / price
	}

	qty = floorToStep(qty, rules.QtyStep)

	if qty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("Bybit %s calculated qty is zero", symbol)
	}

	notional := qty * price

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("Bybit %s qty %.12f < min qty %.12f", symbol, qty, rules.MinQty)
	}

	if rules.MinNotional > 0 && notional < rules.MinNotional {
		return domain.OrderResult{}, fmt.Errorf("Bybit %s notional %.8f < min notional %.8f", symbol, notional, rules.MinNotional)
	}

	body := map[string]string{
		"category":    categoryLinear,
		"symbol":      symbol,
		"side":        "Sell",
		"orderType":   "Market",
		"qty":         formatFloat(qty),
		"timeInForce": "IOC",
		"reduceOnly":  "false",
		"positionIdx": "0",
	}

	if req.ClientOrderID != "" {
		body["orderLinkId"] = bybitOrderLinkID(req.ClientOrderID)
	}

	var out bybitResponse[struct {
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
	}]

	if err := c.signedPOST("/v5/order/create", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.RetCode != 0 {
		return domain.OrderResult{}, fmt.Errorf("Bybit market short retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	if out.Result.OrderID != "" {
		if fresh, err := c.GetOrderStatus(symbol, out.Result.OrderID); err == nil {
			return fresh, nil
		}
	}

	return domain.OrderResult{
		Exchange:        c.Name(),
		ExchangeOrderID: out.Result.OrderID,
		ClientOrderID:   out.Result.OrderLinkID,
		Symbol:          symbol,
		NativeSymbol:    symbol,
		Side:            "Sell",
		Type:            "Market",
		Status:          "Created",
		OrderStatus:     domain.OrderStatusPending,
		Price:           price,
		Qty:             qty,
		FilledQty:       0,
		FilledNotional:  0,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}, nil
}

func (c *Client) PlaceLimitReduceOnly(req domain.OrderRequest) (domain.OrderResult, error) {
	symbol := normalizeSymbol(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(symbol)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("Bybit %s limit price is zero", symbol)
	}

	price = floorToStep(price, rules.TickSize)

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
		return domain.OrderResult{}, fmt.Errorf("Bybit %s calculated limit qty is zero", symbol)
	}

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("Bybit %s qty %.12f < min qty %.12f", symbol, qty, rules.MinQty)
	}

	side := normalizeBybitSide(req.Side)
	if side == "" {
		return domain.OrderResult{}, fmt.Errorf("Bybit side is empty")
	}

	body := map[string]string{
		"category":    categoryLinear,
		"symbol":      symbol,
		"side":        side,
		"orderType":   "Limit",
		"qty":         formatFloat(qty),
		"price":       formatFloat(price),
		"timeInForce": firstNonEmpty(req.TimeInForce, "GTC"),
		"reduceOnly":  boolString(req.ReduceOnly),
		"positionIdx": "0",
	}

	if req.ClientOrderID != "" {
		body["orderLinkId"] = bybitOrderLinkID(req.ClientOrderID)
	}

	var out bybitResponse[struct {
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
	}]

	if err := c.signedPOST("/v5/order/create", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.RetCode != 0 {
		return domain.OrderResult{}, fmt.Errorf("Bybit limit order retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	return domain.OrderResult{
		Exchange:        c.Name(),
		ExchangeOrderID: out.Result.OrderID,
		ClientOrderID:   out.Result.OrderLinkID,
		Symbol:          symbol,
		NativeSymbol:    symbol,
		Side:            side,
		Type:            "Limit",
		Status:          "Created",
		OrderStatus:     domain.OrderStatusPending,
		Price:           price,
		Qty:             qty,
		ReduceOnly:      req.ReduceOnly,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}, nil
}

func (c *Client) GetOrderStatus(symbol, orderID string) (domain.OrderResult, error) {
	symbol = normalizeSymbol(symbol)

	q := url.Values{}
	q.Set("category", categoryLinear)
	q.Set("symbol", symbol)

	if looksLikeBybitOrderID(orderID) {
		q.Set("orderId", orderID)
	} else {
		q.Set("orderLinkId", bybitOrderLinkID(orderID))
	}

	var out bybitResponse[struct {
		List []bybitOrder `json:"list"`
	}]

	if err := c.signedGET("/v5/order/realtime", q, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.RetCode != 0 {
		return domain.OrderResult{}, fmt.Errorf("Bybit order status retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	if len(out.Result.List) == 0 {
		return domain.OrderResult{}, fmt.Errorf("Bybit order not found: %s %s", symbol, orderID)
	}

	return out.Result.List[0].toDomain(c.Name()), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	symbol = normalizeSymbol(symbol)

	body := map[string]string{
		"category": categoryLinear,
		"symbol":   symbol,
	}

	if looksLikeBybitOrderID(orderID) {
		body["orderId"] = orderID
	} else {
		body["orderLinkId"] = bybitOrderLinkID(orderID)
	}

	var out bybitResponse[any]

	if err := c.signedPOST("/v5/order/cancel", body, &out); err != nil {
		return err
	}

	if out.RetCode != 0 && !isIgnorableCancelRet(out.RetCode, out.RetMsg) {
		return fmt.Errorf("Bybit cancel order retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	return nil
}

func (c *Client) CancelAll(symbol string) error {
	symbol = normalizeSymbol(symbol)

	body := map[string]string{
		"category": categoryLinear,
		"symbol":   symbol,
	}

	var out bybitResponse[any]

	if err := c.signedPOST("/v5/order/cancel-all", body, &out); err != nil {
		return err
	}

	if out.RetCode != 0 && !isIgnorableCancelRet(out.RetCode, out.RetMsg) {
		return fmt.Errorf("Bybit cancel all retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	return nil
}

func (c *Client) ClosePositionMarket(symbol string) error {
	symbol = normalizeSymbol(symbol)

	pos, err := c.GetPosition(symbol)
	if err != nil {
		return err
	}

	if pos.Qty <= 0 {
		return nil
	}

	side := "Buy"
	if pos.Side == domain.SideLong {
		side = "Sell"
	}

	body := map[string]string{
		"category":    categoryLinear,
		"symbol":      symbol,
		"side":        side,
		"orderType":   "Market",
		"qty":         formatFloat(pos.Qty),
		"timeInForce": "IOC",
		"reduceOnly":  "true",
		"positionIdx": "0",
	}

	var out bybitResponse[any]

	if err := c.signedPOST("/v5/order/create", body, &out); err != nil {
		return err
	}

	if out.RetCode != 0 {
		return fmt.Errorf("Bybit close position market retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	return nil
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	symbol = normalizeSymbol(symbol)

	q := url.Values{}
	q.Set("category", categoryLinear)
	q.Set("symbol", symbol)

	var out bybitResponse[struct {
		List []struct {
			Symbol           string        `json:"symbol"`
			Side             string        `json:"side"`
			Size             flexibleFloat `json:"size"`
			AvgPrice         flexibleFloat `json:"avgPrice"`
			PositionValue    flexibleFloat `json:"positionValue"`
			MarkPrice        flexibleFloat `json:"markPrice"`
			UnrealisedPnl    flexibleFloat `json:"unrealisedPnl"`
			CumRealisedPnl   flexibleFloat `json:"cumRealisedPnl"`
			LiqPrice         flexibleFloat `json:"liqPrice"`
			Leverage         flexibleFloat `json:"leverage"`
			TradeMode        flexibleInt   `json:"tradeMode"`
			PositionIdx      flexibleInt   `json:"positionIdx"`
			UpdatedTime      flexibleInt   `json:"updatedTime"`
			AutoAddMargin    flexibleInt   `json:"autoAddMargin"`
			PositionIM       flexibleFloat `json:"positionIM"`
			PositionMM       flexibleFloat `json:"positionMM"`
			TakeProfit       flexibleFloat `json:"takeProfit"`
			StopLoss         flexibleFloat `json:"stopLoss"`
			TrailingStop     flexibleFloat `json:"trailingStop"`
			SessionAvgPrice  flexibleFloat `json:"sessionAvgPrice"`
			AdlRankIndicator flexibleInt   `json:"adlRankIndicator"`
		} `json:"list"`
	}]

	if err := c.signedGET("/v5/position/list", q, &out); err != nil {
		return domain.PositionInfo{}, err
	}

	if out.RetCode != 0 {
		return domain.PositionInfo{}, fmt.Errorf("Bybit position retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	for _, p := range out.Result.List {
		if p.Symbol != symbol {
			continue
		}

		qty := float64(p.Size)

		side := domain.SideShort
		if strings.EqualFold(p.Side, "Buy") {
			side = domain.SideLong
		}

		marginMode := "cross"
		if int64(p.TradeMode) == 1 {
			marginMode = "isolated"
		}

		return domain.PositionInfo{
			Exchange:         c.Name(),
			Symbol:           symbol,
			NativeSymbol:     symbol,
			Side:             side,
			Qty:              qty,
			NotionalUSDT:     float64(p.PositionValue),
			EntryPrice:       float64(p.AvgPrice),
			MarkPrice:        float64(p.MarkPrice),
			UnrealizedPNL:    float64(p.UnrealisedPnl),
			RealizedPNL:      float64(p.CumRealisedPnl),
			LiquidationPrice: float64(p.LiqPrice),
			Leverage:         int(float64(p.Leverage)),
			MarginMode:       marginMode,
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

	rows, err := c.fundingFeesByAccountType("UNIFIED", symbol, from, to)
	if err == nil {
		return rows, nil
	}

	contractRows, contractErr := c.fundingFeesByAccountType("CONTRACT", symbol, from, to)
	if contractErr == nil {
		return contractRows, nil
	}

	return nil, err
}

func (c *Client) fundingFeesByAccountType(accountType, symbol string, from, to time.Time) ([]domain.FundingFeeInfo, error) {
	q := url.Values{}
	q.Set("accountType", accountType)
	q.Set("category", categoryLinear)
	q.Set("currency", "USDT")
	q.Set("limit", "50")

	if !from.IsZero() {
		q.Set("startTime", strconv.FormatInt(from.UTC().UnixMilli(), 10))
	}
	if !to.IsZero() {
		q.Set("endTime", strconv.FormatInt(to.UTC().UnixMilli(), 10))
	}

	var out bybitResponse[struct {
		NextPageCursor string `json:"nextPageCursor"`
		List           []struct {
			Symbol          string        `json:"symbol"`
			Category        string        `json:"category"`
			Side            string        `json:"side"`
			TransactionTime flexibleInt   `json:"transactionTime"`
			Type            string        `json:"type"`
			Qty             flexibleFloat `json:"qty"`
			Size            flexibleFloat `json:"size"`
			Currency        string        `json:"currency"`
			TradePrice      flexibleFloat `json:"tradePrice"`
			Funding         flexibleFloat `json:"funding"`
			Fee             flexibleFloat `json:"fee"`
			CashFlow        flexibleFloat `json:"cashFlow"`
			Change          flexibleFloat `json:"change"`
			CashBalance     flexibleFloat `json:"cashBalance"`
			FeeRate         flexibleFloat `json:"feeRate"`
			BonusChange     flexibleFloat `json:"bonusChange"`
			TradeID         string        `json:"tradeId"`
			OrderID         string        `json:"orderId"`
			OrderLinkID     string        `json:"orderLinkId"`
		} `json:"list"`
	}]

	if err := c.signedGET("/v5/account/transaction-log", q, &out); err != nil {
		return nil, err
	}

	if out.RetCode != 0 {
		return nil, fmt.Errorf("Bybit transaction-log accountType=%s retCode=%d retMsg=%s", accountType, out.RetCode, out.RetMsg)
	}

	result := []domain.FundingFeeInfo{}

	for _, r := range out.Result.List {
		if r.Symbol != "" && r.Symbol != symbol {
			continue
		}

		eventType := strings.ToUpper(r.Type)
		if !strings.Contains(eventType, "SETTLEMENT") && !strings.Contains(eventType, "FUNDING") {
			continue
		}

		amount := firstNonZero(float64(r.Funding), float64(r.CashFlow), float64(r.Change))

		result = append(result, domain.FundingFeeInfo{
			Exchange:     c.Name(),
			Symbol:       symbol,
			NativeSymbol: symbol,
			Amount:       amount,
			Asset:        firstNonEmpty(r.Currency, "USDT"),
			IncomeType:   r.Type,
			FeeTime:      time.UnixMilli(int64(r.TransactionTime)).UTC(),
			Raw:          r.OrderID,
		})
	}

	return result, nil
}

func (c *Client) markPrice(symbol string) float64 {
	symbol = normalizeSymbol(symbol)

	q := url.Values{}
	q.Set("category", categoryLinear)
	q.Set("symbol", symbol)

	var out bybitResponse[struct {
		List []struct {
			Symbol    string        `json:"symbol"`
			MarkPrice flexibleFloat `json:"markPrice"`
		} `json:"list"`
	}]

	if err := c.getJSON("/v5/market/tickers", q, &out); err != nil {
		return 0
	}

	if out.RetCode != 0 {
		return 0
	}

	for _, r := range out.Result.List {
		if r.Symbol == symbol {
			return float64(r.MarkPrice)
		}
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
		return fmt.Errorf("Bybit GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	return json.Unmarshal(body, dst)
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	if q == nil {
		q = url.Values{}
	}

	query := q.Encode()

	return c.signedRequest(http.MethodGet, path, query, nil, dst)
}

func (c *Client) signedPOST(path string, payload any, dst any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	return c.signedRequest(http.MethodPost, path, string(raw), raw, dst)
}

func (c *Client) signedRequest(method, path, signPayload string, body []byte, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("Bybit API key or secret is empty")
	}

	timestamp := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	sign := c.sign(timestamp + c.apiKey + recvWindow + signPayload)

	u := c.baseURL + path
	var bodyReader io.Reader

	if method == http.MethodGet {
		if signPayload != "" {
			u += "?" + signPayload
		}
	} else {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequest(method, u, bodyReader)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("X-BAPI-API-KEY", c.apiKey)
	req.Header.Set("X-BAPI-TIMESTAMP", timestamp)
	req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)
	req.Header.Set("X-BAPI-SIGN", sign)

	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("Bybit %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil || len(raw) == 0 {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("Bybit %s %s decode error: %w; body=%s", method, path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return hex.EncodeToString(mac.Sum(nil))
}

type bybitResponse[T any] struct {
	RetCode int         `json:"retCode"`
	RetMsg  string      `json:"retMsg"`
	Result  T           `json:"result"`
	RetExt  any         `json:"retExtInfo"`
	Time    flexibleInt `json:"time"`
}

type bybitOrder struct {
	OrderID            string        `json:"orderId"`
	OrderLinkID        string        `json:"orderLinkId"`
	BlockTradeID       string        `json:"blockTradeId"`
	Symbol             string        `json:"symbol"`
	Price              flexibleFloat `json:"price"`
	Qty                flexibleFloat `json:"qty"`
	Side               string        `json:"side"`
	IsLeverage         string        `json:"isLeverage"`
	PositionIdx        flexibleInt   `json:"positionIdx"`
	OrderStatus        string        `json:"orderStatus"`
	CancelType         string        `json:"cancelType"`
	RejectReason       string        `json:"rejectReason"`
	AvgPrice           flexibleFloat `json:"avgPrice"`
	LeavesQty          flexibleFloat `json:"leavesQty"`
	LeavesValue        flexibleFloat `json:"leavesValue"`
	CumExecQty         flexibleFloat `json:"cumExecQty"`
	CumExecValue       flexibleFloat `json:"cumExecValue"`
	CumExecFee         flexibleFloat `json:"cumExecFee"`
	TimeInForce        string        `json:"timeInForce"`
	OrderType          string        `json:"orderType"`
	StopOrderType      string        `json:"stopOrderType"`
	OrderIv            string        `json:"orderIv"`
	TriggerPrice       flexibleFloat `json:"triggerPrice"`
	TakeProfit         flexibleFloat `json:"takeProfit"`
	StopLoss           flexibleFloat `json:"stopLoss"`
	TpslMode           string        `json:"tpslMode"`
	OcoTriggerBy       string        `json:"ocoTriggerBy"`
	TpLimitPrice       flexibleFloat `json:"tpLimitPrice"`
	SlLimitPrice       flexibleFloat `json:"slLimitPrice"`
	TpTriggerBy        string        `json:"tpTriggerBy"`
	SlTriggerBy        string        `json:"slTriggerBy"`
	TriggerDirection   int           `json:"triggerDirection"`
	TriggerBy          string        `json:"triggerBy"`
	LastPriceOnCreated flexibleFloat `json:"lastPriceOnCreated"`
	ReduceOnly         bool          `json:"reduceOnly"`
	CloseOnTrigger     bool          `json:"closeOnTrigger"`
	PlaceType          string        `json:"placeType"`
	SmpType            string        `json:"smpType"`
	SmpGroup           int           `json:"smpGroup"`
	SmpOrderID         string        `json:"smpOrderId"`
	CreatedTime        flexibleInt   `json:"createdTime"`
	UpdatedTime        flexibleInt   `json:"updatedTime"`
}

func (o bybitOrder) toDomain(exchange domain.ExchangeName) domain.OrderResult {
	createdAt := time.Now().UTC()
	if o.CreatedTime > 0 {
		createdAt = time.UnixMilli(int64(o.CreatedTime)).UTC()
	}

	updatedAt := time.Now().UTC()
	if o.UpdatedTime > 0 {
		updatedAt = time.UnixMilli(int64(o.UpdatedTime)).UTC()
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: o.OrderID,
		ClientOrderID:   o.OrderLinkID,
		Symbol:          o.Symbol,
		NativeSymbol:    o.Symbol,
		Side:            o.Side,
		Type:            o.OrderType,
		Status:          o.OrderStatus,
		OrderStatus:     mapOrderStatus(o.OrderStatus),
		Price:           float64(o.Price),
		AvgPrice:        float64(o.AvgPrice),
		Qty:             float64(o.Qty),
		FilledQty:       float64(o.CumExecQty),
		FilledNotional:  float64(o.CumExecValue),
		RemainingQty:    float64(o.LeavesQty),
		Fee:             float64(o.CumExecFee),
		FeeAsset:        "USDT",
		ReduceOnly:      o.ReduceOnly,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

func mapOrderStatus(s string) domain.OrderStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "new", "untriggered", "triggered":
		return domain.OrderStatusNew
	case "partiallyfilled":
		return domain.OrderStatusPartiallyFill
	case "filled":
		return domain.OrderStatusFilled
	case "cancelled", "canceled":
		return domain.OrderStatusCanceled
	case "rejected":
		return domain.OrderStatusRejected
	case "deactivated":
		return domain.OrderStatusExpired
	default:
		if s == "" {
			return domain.OrderStatusUnknown
		}

		return domain.OrderStatusUnknown
	}
}

type flexibleFloat float64

func (f *flexibleFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)

	if s == "" || s == "null" {
		*f = 0
		return nil
	}

	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return err
	}

	*f = flexibleFloat(v)

	return nil
}

type flexibleInt int64

func (i *flexibleInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)

	if s == "" || s == "null" {
		*i = 0
		return nil
	}

	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}

		v = int64(f)
	}

	*i = flexibleInt(v)

	return nil
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

func normalizeSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
}

func normalizeBybitSide(side string) string {
	switch strings.ToUpper(strings.TrimSpace(side)) {
	case "BUY":
		return "Buy"
	case "SELL":
		return "Sell"
	default:
		return strings.TrimSpace(side)
	}
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

func bybitOrderLinkID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	replacer := strings.NewReplacer(
		":", "-",
		"/", "-",
		" ", "-",
		"_", "-",
	)

	s = replacer.Replace(s)

	if len(s) > 36 {
		s = s[len(s)-36:]
	}

	return s
}

func looksLikeBybitOrderID(s string) bool {
	s = strings.TrimSpace(s)

	if s == "" {
		return false
	}

	if strings.Contains(s, ":") {
		return false
	}

	return len(s) >= 24
}

func isIgnorableBybitRet(code int, msg string) bool {
	msg = strings.ToLower(msg)

	if code == 0 {
		return true
	}

	ignoredCodes := map[int]bool{
		110025: true,
		110026: true,
		110043: true,
		34036:  true,
	}

	if ignoredCodes[code] {
		return true
	}

	return strings.Contains(msg, "not modified") ||
		strings.Contains(msg, "same") ||
		strings.Contains(msg, "no need")
}

func isIgnorablePositionConfigError(err error) bool {
	if err == nil {
		return true
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "not modified") ||
		strings.Contains(msg, "same") ||
		strings.Contains(msg, "no need") ||
		strings.Contains(msg, "110025") ||
		strings.Contains(msg, "110026") ||
		strings.Contains(msg, "110043")
}

func isIgnorableCancelRet(code int, msg string) bool {
	msg = strings.ToLower(msg)

	if code == 0 {
		return true
	}

	ignoredCodes := map[int]bool{
		110001: true,
		110008: true,
		110010: true,
	}

	if ignoredCodes[code] {
		return true
	}

	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "already") ||
		strings.Contains(msg, "finished") ||
		strings.Contains(msg, "cancel")
}

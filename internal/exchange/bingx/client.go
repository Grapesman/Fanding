package bingx

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

type response[T any] struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	Message   string `json:"message"`
	Data      T      `json:"data"`
	Timestamp int64  `json:"timestamp"`
}

type bingXTickerRow struct {
	Symbol      string
	LastPrice   string
	QuoteVolume string
	Volume      string
	AskPrice    string
	BidPrice    string
	VolumeUSDT  float64
}

type bingXPremiumInfo struct {
	Symbol        string
	FundingRate   float64
	MarkPrice     float64
	IndexPrice    float64
	NextFunding   time.Time
	IntervalHours float64
	OK            bool
}

type premiumPayload struct {
	Symbol          string `json:"symbol"`
	LastFundingRate string `json:"lastFundingRate"`
	FundingRate     string `json:"fundingRate"`
	MarkPrice       string `json:"markPrice"`
	IndexPrice      string `json:"indexPrice"`

	NextFundingTime flexibleInt `json:"nextFundingTime"`
	FundingTime     flexibleInt `json:"fundingTime"`
	UpdateTime      flexibleInt `json:"updateTime"`

	FundingIntervalHours flexibleFloat `json:"fundingIntervalHours"`
	FundingInterval      flexibleFloat `json:"fundingInterval"`
	FundingIntervalHour  flexibleFloat `json:"fundingIntervalHour"`
	Interval             flexibleFloat `json:"interval"`
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
	return domain.ExchangeBingX
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ServerTime flexibleInt `json:"serverTime"`
		} `json:"data"`
	}

	if err := c.getJSON("/openApi/swap/v2/server/time", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.Code != 0 {
		return time.Time{}, fmt.Errorf("BingX server time code=%d msg=%s", out.Code, out.Msg)
	}

	if out.Data.ServerTime > 0 {
		return time.UnixMilli(int64(out.Data.ServerTime)).UTC(), nil
	}

	return time.Now().UTC(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "BingX API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	params := url.Values{}
	params.Set("recvWindow", "5000")

	var out response[[]struct {
		UserID           string        `json:"userId"`
		Asset            string        `json:"asset"`
		Balance          flexibleFloat `json:"balance"`
		Equity           flexibleFloat `json:"equity"`
		UnrealizedProfit flexibleFloat `json:"unrealizedProfit"`
		RealizedProfit   flexibleFloat `json:"realizedProfit"`
		AvailableMargin  flexibleFloat `json:"availableMargin"`
		UsedMargin       flexibleFloat `json:"usedMargin"`
		FrozenMargin     flexibleFloat `json:"frozenMargin"`
		ShortUID         string        `json:"shortUid"`
	}]

	if err := c.signedGET("/openApi/swap/v3/user/balance", params, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "BingX futures balance error: " + err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	if out.Code != 0 {
		err := fmt.Errorf("BingX balance code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))

		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	for _, b := range out.Data {
		if strings.EqualFold(b.Asset, "USDT") {
			wallet := float64(b.Balance)
			if wallet <= 0 {
				wallet = float64(b.Equity)
			}

			return domain.Balance{
				Exchange:      c.Name(),
				WalletUSDT:    wallet,
				AvailableUSDT: float64(b.AvailableMargin),
				PrivateOK:     true,
				Error:         "",
				UpdatedAt:     time.Now().UTC(),
			}, nil
		}
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "BingX USDT futures balance not found",
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	var tick response[[]struct {
		Symbol      string `json:"symbol"`
		LastPrice   string `json:"lastPrice"`
		QuoteVolume string `json:"quoteVolume"`
		Volume      string `json:"volume"`
		AskPrice    string `json:"askPrice"`
		BidPrice    string `json:"bidPrice"`
	}]

	if err := c.getJSON("/openApi/swap/v2/quote/ticker", nil, &tick); err != nil {
		return nil, err
	}

	if tick.Code != 0 {
		return nil, fmt.Errorf("BingX ticker code=%d msg=%s", tick.Code, firstNonEmpty(tick.Msg, tick.Message))
	}

	rows := make([]bingXTickerRow, 0, len(tick.Data))

	for _, t := range tick.Data {
		if !strings.Contains(strings.ToUpper(t.Symbol), "USDT") {
			continue
		}

		vol := parseFloat(t.QuoteVolume)
		if vol <= 0 {
			vol = parseFloat(t.Volume) * parseFloat(t.LastPrice)
		}

		rows = append(rows, bingXTickerRow{
			Symbol:      t.Symbol,
			LastPrice:   t.LastPrice,
			QuoteVolume: t.QuoteVolume,
			Volume:      t.Volume,
			AskPrice:    t.AskPrice,
			BidPrice:    t.BidPrice,
			VolumeUSDT:  vol,
		})
	}

	premiumBySymbol := c.premiumInfo()
	intervalBySymbol := c.contractIntervals()

	now := time.Now().UTC()
	out := make([]domain.Candidate, 0, len(rows))

	for _, t := range rows {
		symbol := normalizeSymbol(t.Symbol)

		price := parseFloat(t.LastPrice)
		prem := premiumBySymbol[symbol]

		mark := prem.MarkPrice
		if mark <= 0 {
			mark = price
		}

		fundingRate := prem.FundingRate

		intervalHours := prem.IntervalHours
		if intervalHours <= 0 {
			intervalHours = intervalBySymbol[symbol]
		}
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := prem.NextFunding
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		bid := parseFloat(t.BidPrice)
		ask := parseFloat(t.AskPrice)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		out = append(out, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          fundingRate,
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        t.VolumeUSDT,
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

func (c *Client) premiumInfo() map[string]bingXPremiumInfo {
	out := map[string]bingXPremiumInfo{}

	var raw struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}

	if err := c.getJSON("/openApi/swap/v2/quote/premiumIndex", nil, &raw); err != nil {
		return out
	}

	if raw.Code != 0 {
		return out
	}

	var arr []premiumPayload

	if err := json.Unmarshal(raw.Data, &arr); err != nil {
		var one premiumPayload
		if err2 := json.Unmarshal(raw.Data, &one); err2 != nil {
			return out
		}
		arr = []premiumPayload{one}
	}

	for _, p := range arr {
		symbol := normalizeSymbol(p.Symbol)
		if symbol == "" {
			continue
		}

		fr := parseFloat(firstNonEmpty(p.FundingRate, p.LastFundingRate))
		interval := extractIntervalHours(
			p.FundingIntervalHours,
			p.FundingIntervalHour,
			p.FundingInterval,
			p.Interval,
		)

		nextFunding := time.Time{}
		if p.NextFundingTime > 0 {
			nextFunding = parseMillisOrSeconds(int64(p.NextFundingTime))
		} else if p.FundingTime > 0 {
			nextFunding = parseMillisOrSeconds(int64(p.FundingTime))
		}

		out[symbol] = bingXPremiumInfo{
			Symbol:        symbol,
			FundingRate:   fr,
			MarkPrice:     parseFloat(p.MarkPrice),
			IndexPrice:    parseFloat(p.IndexPrice),
			NextFunding:   nextFunding,
			IntervalHours: interval,
			OK:            true,
		}
	}

	return out
}

func (c *Client) contractIntervals() map[string]float64 {
	out := map[string]float64{}

	var resp response[[]struct {
		Symbol              string        `json:"symbol"`
		Status              flexibleInt   `json:"status"`
		BaseAsset           string        `json:"baseAsset"`
		QuoteAsset          string        `json:"quoteAsset"`
		Asset               string        `json:"asset"`
		Size                flexibleFloat `json:"size"`
		QuantityPrecision   flexibleInt   `json:"quantityPrecision"`
		PricePrecision      flexibleInt   `json:"pricePrecision"`
		MinQty              flexibleFloat `json:"minQty"`
		MinNotional         flexibleFloat `json:"minNotional"`
		FundingInterval     flexibleFloat `json:"fundingInterval"`
		FundingIntervalHour flexibleFloat `json:"fundingIntervalHour"`
		FundingTime         flexibleFloat `json:"fundingTime"`
	}]

	if err := c.getJSON("/openApi/swap/v2/quote/contracts", nil, &resp); err != nil {
		return out
	}

	if resp.Code != 0 {
		return out
	}

	for _, s := range resp.Data {
		symbol := normalizeSymbol(s.Symbol)
		if symbol == "" {
			continue
		}

		interval := extractIntervalHours(s.FundingIntervalHour, s.FundingInterval, s.FundingTime)
		if interval <= 0 {
			continue
		}

		out[symbol] = interval
	}

	return out
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	symbol = normalizeSymbol(symbol)

	var resp response[[]struct {
		Symbol            string        `json:"symbol"`
		Status            flexibleInt   `json:"status"`
		BaseAsset         string        `json:"baseAsset"`
		QuoteAsset        string        `json:"quoteAsset"`
		Asset             string        `json:"asset"`
		Size              flexibleFloat `json:"size"`
		QuantityPrecision flexibleInt   `json:"quantityPrecision"`
		PricePrecision    flexibleInt   `json:"pricePrecision"`
		MinQty            flexibleFloat `json:"minQty"`
		MaxQty            flexibleFloat `json:"maxQty"`
		MinNotional       flexibleFloat `json:"minNotional"`
		TradeMinQuantity  flexibleFloat `json:"tradeMinQuantity"`
		TradeMinUSDT      flexibleFloat `json:"tradeMinUSDT"`
		StepSize          flexibleFloat `json:"stepSize"`
		TickSize          flexibleFloat `json:"tickSize"`
	}]

	if err := c.getJSON("/openApi/swap/v2/quote/contracts", nil, &resp); err != nil {
		return domain.SymbolRules{}, err
	}

	if resp.Code != 0 {
		return domain.SymbolRules{}, fmt.Errorf("BingX contracts code=%d msg=%s", resp.Code, firstNonEmpty(resp.Msg, resp.Message))
	}

	for _, item := range resp.Data {
		if normalizeSymbol(item.Symbol) != symbol {
			continue
		}

		qtyStep := float64(item.StepSize)
		if qtyStep <= 0 {
			qtyStep = precisionStep(int64(item.QuantityPrecision))
		}
		if qtyStep <= 0 {
			qtyStep = 0.001
		}

		tickSize := float64(item.TickSize)
		if tickSize <= 0 {
			tickSize = precisionStep(int64(item.PricePrecision))
		}

		minQty := firstNonZero(float64(item.MinQty), float64(item.TradeMinQuantity))
		minNotional := firstNonZero(float64(item.MinNotional), float64(item.TradeMinUSDT))

		return domain.SymbolRules{
			Exchange:                    c.Name(),
			Symbol:                      symbol,
			NativeSymbol:                symbol,
			SupportsNotionalMarketOrder: false,
			MinQty:                      minQty,
			MaxQty:                      float64(item.MaxQty),
			QtyStep:                     qtyStep,
			MinNotional:                 minNotional,
			PriceStep:                   tickSize,
			TickSize:                    tickSize,
			ContractSize:                firstNonZero(float64(item.Size), 1),
			BaseAsset:                   item.BaseAsset,
			QuoteAsset:                  firstNonEmpty(item.QuoteAsset, item.Asset, "USDT"),
			MarginAsset:                 "USDT",
			UpdatedAt:                   time.Now().UTC(),
		}, nil
	}

	return domain.SymbolRules{}, fmt.Errorf("BingX symbol rules not found: %s", symbol)
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

	var marginOut response[any]
	if err := c.signedPOST("/openApi/swap/v2/trade/marginType", marginParams, &marginOut); err != nil {
		if !isIgnorableConfigError(err) {
			return err
		}
	} else if marginOut.Code != 0 && !isIgnorableBingXCode(marginOut.Code, firstNonEmpty(marginOut.Msg, marginOut.Message)) {
		return fmt.Errorf("BingX marginType code=%d msg=%s", marginOut.Code, firstNonEmpty(marginOut.Msg, marginOut.Message))
	}

	levParams := url.Values{}
	levParams.Set("symbol", symbol)
	levParams.Set("side", "SHORT")
	levParams.Set("leverage", strconv.Itoa(leverage))

	var levOut response[any]
	if err := c.signedPOST("/openApi/swap/v2/trade/leverage", levParams, &levOut); err != nil {
		if !isIgnorableConfigError(err) {
			return err
		}
	} else if levOut.Code != 0 && !isIgnorableBingXCode(levOut.Code, firstNonEmpty(levOut.Msg, levOut.Message)) {
		return fmt.Errorf("BingX leverage code=%d msg=%s", levOut.Code, firstNonEmpty(levOut.Msg, levOut.Message))
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

	return domain.OrderResult{}, fmt.Errorf("BingX unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
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
		return domain.OrderResult{}, fmt.Errorf("BingX %s mark price is zero", symbol)
	}

	qty := req.Qty
	if qty <= 0 {
		qty = firstNonZero(req.NotionalUSDT, req.MarginUSDT) / price
	}

	qty = floorToStep(qty, rules.QtyStep)
	if qty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("BingX %s calculated qty is zero", symbol)
	}

	notional := qty * price

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("BingX %s qty %.12f < min qty %.12f", symbol, qty, rules.MinQty)
	}

	if rules.MinNotional > 0 && notional < rules.MinNotional {
		return domain.OrderResult{}, fmt.Errorf("BingX %s notional %.8f < min notional %.8f", symbol, notional, rules.MinNotional)
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", "SELL")
	params.Set("positionSide", "SHORT")
	params.Set("type", "MARKET")
	params.Set("quantity", formatFloat(qty))

	if req.ClientOrderID != "" {
		params.Set("clientOrderID", bingXClientOrderID(req.ClientOrderID))
	}

	var out response[bingXOrderData]
	if err := c.signedPOST("/openApi/swap/v2/trade/order", params, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != 0 {
		return domain.OrderResult{}, fmt.Errorf("BingX market short code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))
	}

	res := out.Data.toDomain(c.Name(), symbol)

	if res.ExchangeOrderID != "" {
		if fresh, err := c.GetOrderStatus(symbol, res.ExchangeOrderID); err == nil {
			res = fresh
		}
	}

	if res.Price <= 0 {
		res.Price = price
	}
	if res.Qty <= 0 {
		res.Qty = qty
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
		return domain.OrderResult{}, fmt.Errorf("BingX %s limit price is zero", symbol)
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
		return domain.OrderResult{}, fmt.Errorf("BingX %s calculated limit qty is zero", symbol)
	}

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("BingX %s qty %.12f < min qty %.12f", symbol, qty, rules.MinQty)
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", strings.ToUpper(req.Side))
	params.Set("type", "LIMIT")
	params.Set("quantity", formatFloat(qty))
	params.Set("price", formatFloat(price))
	params.Set("timeInForce", firstNonEmpty(req.TimeInForce, "GTC"))
	params.Set("reduceOnly", boolString(req.ReduceOnly))

	if strings.EqualFold(req.Side, "BUY") {
		params.Set("positionSide", "SHORT")
	} else {
		params.Set("positionSide", "LONG")
	}

	if req.ClientOrderID != "" {
		params.Set("clientOrderID", bingXClientOrderID(req.ClientOrderID))
	}

	var out response[bingXOrderData]
	if err := c.signedPOST("/openApi/swap/v2/trade/order", params, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != 0 {
		return domain.OrderResult{}, fmt.Errorf("BingX limit order code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))
	}

	res := out.Data.toDomain(c.Name(), symbol)

	if res.Price <= 0 {
		res.Price = price
	}
	if res.Qty <= 0 {
		res.Qty = qty
	}
	res.ReduceOnly = req.ReduceOnly

	return res, nil
}

func (c *Client) GetOrderStatus(symbol, orderID string) (domain.OrderResult, error) {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	if isInt(orderID) {
		params.Set("orderId", orderID)
	} else {
		params.Set("clientOrderID", bingXClientOrderID(orderID))
	}

	var out response[bingXOrderData]
	if err := c.signedGET("/openApi/swap/v2/trade/order", params, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != 0 {
		return domain.OrderResult{}, fmt.Errorf("BingX order status code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))
	}

	return out.Data.toDomain(c.Name(), symbol), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	if isInt(orderID) {
		params.Set("orderId", orderID)
	} else {
		params.Set("clientOrderID", bingXClientOrderID(orderID))
	}

	var out response[any]
	if err := c.signedDELETE("/openApi/swap/v2/trade/order", params, &out); err != nil {
		return err
	}

	if out.Code != 0 && !isIgnorableCancelCode(out.Code, firstNonEmpty(out.Msg, out.Message)) {
		return fmt.Errorf("BingX cancel order code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))
	}

	return nil
}

func (c *Client) CancelAll(symbol string) error {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	var out response[any]
	if err := c.signedDELETE("/openApi/swap/v2/trade/allOpenOrders", params, &out); err != nil {
		return err
	}

	if out.Code != 0 && !isIgnorableCancelCode(out.Code, firstNonEmpty(out.Msg, out.Message)) {
		return fmt.Errorf("BingX cancel all code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))
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

	side := "BUY"
	positionSide := "SHORT"

	if pos.Side == domain.SideLong {
		side = "SELL"
		positionSide = "LONG"
	}

	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("side", side)
	params.Set("positionSide", positionSide)
	params.Set("type", "MARKET")
	params.Set("quantity", formatFloat(pos.Qty))
	params.Set("reduceOnly", "true")

	var out response[bingXOrderData]
	if err := c.signedPOST("/openApi/swap/v2/trade/order", params, &out); err != nil {
		return err
	}

	if out.Code != 0 {
		return fmt.Errorf("BingX close position market code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))
	}

	return nil
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	symbol = normalizeSymbol(symbol)

	params := url.Values{}
	params.Set("symbol", symbol)

	var out response[[]struct {
		Symbol           string        `json:"symbol"`
		PositionSide     string        `json:"positionSide"`
		PositionAmt      flexibleFloat `json:"positionAmt"`
		AvailableAmt     flexibleFloat `json:"availableAmt"`
		AvgPrice         flexibleFloat `json:"avgPrice"`
		InitialMargin    flexibleFloat `json:"initialMargin"`
		Leverage         flexibleFloat `json:"leverage"`
		UnrealizedProfit flexibleFloat `json:"unrealizedProfit"`
		RealisedProfit   flexibleFloat `json:"realisedProfit"`
		LiquidationPrice flexibleFloat `json:"liquidationPrice"`
		MarkPrice        flexibleFloat `json:"markPrice"`
		MarginType       string        `json:"marginType"`
		UpdateTime       flexibleInt   `json:"updateTime"`
	}]

	if err := c.signedGET("/openApi/swap/v2/user/positions", params, &out); err != nil {
		return domain.PositionInfo{}, err
	}

	if out.Code != 0 {
		return domain.PositionInfo{}, fmt.Errorf("BingX position code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))
	}

	for _, p := range out.Data {
		if normalizeSymbol(p.Symbol) != symbol {
			continue
		}

		qty := math.Abs(float64(p.PositionAmt))
		if qty <= 0 {
			qty = math.Abs(float64(p.AvailableAmt))
		}
		if qty <= 0 {
			continue
		}

		side := domain.SideShort
		if strings.EqualFold(p.PositionSide, "LONG") || float64(p.PositionAmt) > 0 {
			side = domain.SideLong
		}

		return domain.PositionInfo{
			Exchange:         c.Name(),
			Symbol:           symbol,
			NativeSymbol:     symbol,
			Side:             side,
			Qty:              qty,
			NotionalUSDT:     qty * float64(p.MarkPrice),
			EntryPrice:       float64(p.AvgPrice),
			MarkPrice:        float64(p.MarkPrice),
			UnrealizedPNL:    float64(p.UnrealizedProfit),
			RealizedPNL:      float64(p.RealisedProfit),
			LiquidationPrice: float64(p.LiquidationPrice),
			Leverage:         int(float64(p.Leverage)),
			MarginMode:       strings.ToLower(p.MarginType),
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
	params.Set("incomeType", "FUNDING_FEE")
	params.Set("limit", "1000")

	if !from.IsZero() {
		params.Set("startTime", strconv.FormatInt(from.UTC().UnixMilli(), 10))
	}

	if !to.IsZero() {
		params.Set("endTime", strconv.FormatInt(to.UTC().UnixMilli(), 10))
	}

	var out response[[]struct {
		Symbol     string        `json:"symbol"`
		IncomeType string        `json:"incomeType"`
		Income     flexibleFloat `json:"income"`
		Asset      string        `json:"asset"`
		Time       flexibleInt   `json:"time"`
		Info       string        `json:"info"`
	}]

	if err := c.signedGET("/openApi/swap/v2/user/income", params, &out); err != nil {
		return nil, err
	}

	if out.Code != 0 {
		return nil, fmt.Errorf("BingX income code=%d msg=%s", out.Code, firstNonEmpty(out.Msg, out.Message))
	}

	result := make([]domain.FundingFeeInfo, 0, len(out.Data))

	for _, r := range out.Data {
		if r.Symbol != "" && normalizeSymbol(r.Symbol) != symbol {
			continue
		}

		result = append(result, domain.FundingFeeInfo{
			Exchange:     c.Name(),
			Symbol:       symbol,
			NativeSymbol: symbol,
			Amount:       float64(r.Income),
			Asset:        firstNonEmpty(r.Asset, "USDT"),
			IncomeType:   firstNonEmpty(r.IncomeType, "FUNDING_FEE"),
			FeeTime:      parseMillisOrSeconds(int64(r.Time)),
			Raw:          r.Info,
		})
	}

	return result, nil
}

func (c *Client) markPrice(symbol string) float64 {
	info := c.premiumInfo()
	if p, ok := info[normalizeSymbol(symbol)]; ok && p.MarkPrice > 0 {
		return p.MarkPrice
	}

	params := url.Values{}
	params.Set("symbol", normalizeSymbol(symbol))

	var out response[struct {
		Symbol    string `json:"symbol"`
		MarkPrice string `json:"markPrice"`
	}]

	if err := c.getJSON("/openApi/swap/v2/quote/premiumIndex", params, &out); err != nil {
		return 0
	}

	if out.Code != 0 {
		return 0
	}

	return parseFloat(out.Data.MarkPrice)
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

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("BingX GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("BingX GET %s decode error: %w body=%s", path, err, string(body))
	}

	return nil
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
		return fmt.Errorf("BingX API key or secret is empty")
	}

	if params == nil {
		params = url.Values{}
	}

	if params.Get("timestamp") == "" {
		params.Set("timestamp", strconv.FormatInt(time.Now().UTC().UnixMilli(), 10))
	}

	query := params.Encode()
	signature := c.sign(query)
	query += "&signature=" + signature

	fullURL := c.baseURL + path
	var body io.Reader

	if method == http.MethodGet || method == http.MethodDelete {
		fullURL += "?" + query
	} else {
		body = strings.NewReader(query)
	}

	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return err
	}

	req.Header.Set("X-BX-APIKEY", c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")

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
		return fmt.Errorf("BingX %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil || len(raw) == 0 {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("BingX %s %s decode error: %w body=%s", method, path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(query string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(query))

	return hex.EncodeToString(mac.Sum(nil))
}

type bingXOrderData struct {
	OrderID       flexibleInt   `json:"orderId"`
	ClientOrderID string        `json:"clientOrderID"`
	Symbol        string        `json:"symbol"`
	Side          string        `json:"side"`
	PositionSide  string        `json:"positionSide"`
	Type          string        `json:"type"`
	Status        string        `json:"status"`
	Price         flexibleFloat `json:"price"`
	AvgPrice      flexibleFloat `json:"avgPrice"`
	OrigQty       flexibleFloat `json:"origQty"`
	Quantity      flexibleFloat `json:"quantity"`
	ExecutedQty   flexibleFloat `json:"executedQty"`
	CumQuote      flexibleFloat `json:"cumQuote"`
	Profit        flexibleFloat `json:"profit"`
	Commission    flexibleFloat `json:"commission"`
	ReduceOnly    bool          `json:"reduceOnly"`
	Time          flexibleInt   `json:"time"`
	UpdateTime    flexibleInt   `json:"updateTime"`
}

func (o bingXOrderData) toDomain(exchange domain.ExchangeName, fallbackSymbol string) domain.OrderResult {
	symbol := normalizeSymbol(firstNonEmpty(o.Symbol, fallbackSymbol))

	qty := firstNonZero(float64(o.OrigQty), float64(o.Quantity))

	createdAt := time.Now().UTC()
	if o.Time > 0 {
		createdAt = parseMillisOrSeconds(int64(o.Time))
	}

	updatedAt := time.Now().UTC()
	if o.UpdateTime > 0 {
		updatedAt = parseMillisOrSeconds(int64(o.UpdateTime))
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: strconv.FormatInt(int64(o.OrderID), 10),
		ClientOrderID:   o.ClientOrderID,
		Symbol:          symbol,
		NativeSymbol:    symbol,
		Side:            o.Side,
		PositionSide:    o.PositionSide,
		Type:            o.Type,
		Status:          o.Status,
		OrderStatus:     mapOrderStatus(o.Status),
		Price:           float64(o.Price),
		AvgPrice:        float64(o.AvgPrice),
		Qty:             qty,
		FilledQty:       float64(o.ExecutedQty),
		FilledNotional:  float64(o.CumQuote),
		RemainingQty:    math.Max(0, qty-float64(o.ExecutedQty)),
		Fee:             float64(o.Commission),
		FeeAsset:        "USDT",
		ReduceOnly:      o.ReduceOnly,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

func mapOrderStatus(s string) domain.OrderStatus {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "NEW", "PENDING":
		return domain.OrderStatusNew
	case "PARTIALLY_FILLED", "PARTIALLYFILLED":
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

func precisionStep(precision int64) float64 {
	if precision <= 0 {
		return 0
	}

	return math.Pow10(-int(precision))
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

func parseMillisOrSeconds(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}

	if v > 1_000_000_000_000 {
		return time.UnixMilli(v).UTC()
	}

	return time.Unix(v, 0).UTC()
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

func bingXClientOrderID(s string) string {
	s = strings.TrimSpace(s)

	replacer := strings.NewReplacer(
		":", "-",
		"/", "-",
		" ", "-",
		"_", "-",
	)

	s = replacer.Replace(s)

	if len(s) > 40 {
		s = s[len(s)-40:]
	}

	return s
}

func isIgnorableConfigError(err error) bool {
	if err == nil {
		return true
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "no need") ||
		strings.Contains(msg, "same") ||
		strings.Contains(msg, "not modified") ||
		strings.Contains(msg, "already")
}

func isIgnorableBingXCode(code int, msg string) bool {
	if code == 0 {
		return true
	}

	msg = strings.ToLower(msg)

	return strings.Contains(msg, "no need") ||
		strings.Contains(msg, "same") ||
		strings.Contains(msg, "not modified") ||
		strings.Contains(msg, "already")
}

func isIgnorableCancelCode(code int, msg string) bool {
	if code == 0 {
		return true
	}

	msg = strings.ToLower(msg)

	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "not exist") ||
		strings.Contains(msg, "already") ||
		strings.Contains(msg, "finished") ||
		strings.Contains(msg, "canceled") ||
		strings.Contains(msg, "cancelled")
}

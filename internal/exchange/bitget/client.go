package bitget

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
	"os"
	"strconv"
	"strings"
	"time"

	"funding-bot/internal/domain"
)

const (
	productTypeUSDTFutures = "USDT-FUTURES"
	marginCoinUSDT         = "USDT"
)

type Client struct {
	baseURL    string
	apiKey     string
	apiSecret  string
	passphrase string
	http       *http.Client
}

func New(baseURL, apiKey, apiSecret string, passphrase ...string) *Client {
	pp := ""

	if len(passphrase) > 0 {
		pp = passphrase[0]
	}

	if pp == "" {
		pp = os.Getenv("BITGET_API_PASSPHRASE")
	}

	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		apiSecret:  strings.TrimSpace(apiSecret),
		passphrase: strings.TrimSpace(pp),
		http:       &http.Client{Timeout: 12 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeBitget
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	if err == nil {
		return true
	}

	if c.apiKey != "" && c.apiSecret != "" && c.passphrase != "" {
		b, balanceErr := c.Balance()
		return balanceErr == nil && b.PrivateOK
	}

	return false
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Code        string      `json:"code"`
		Msg         string      `json:"msg"`
		RequestTime flexibleInt `json:"requestTime"`
		Data        flexibleInt `json:"data"`
	}

	if err := c.getJSON("/api/v2/public/time", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.Code != "" && out.Code != "00000" {
		return time.Time{}, fmt.Errorf("Bitget server time code=%s msg=%s", out.Code, out.Msg)
	}

	ts := int64(out.Data)
	if ts <= 0 {
		ts = int64(out.RequestTime)
	}

	if ts > 0 {
		return parseMillisOrSeconds(ts), nil
	}

	return time.Now().UTC(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bitget API key, secret or passphrase is empty",
			UpdatedAt: now,
		}, nil
	}

	q := url.Values{}
	q.Set("productType", productTypeUSDTFutures)

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			MarginCoin           string        `json:"marginCoin"`
			Available            flexibleFloat `json:"available"`
			CrossedMaxAvailable  flexibleFloat `json:"crossedMaxAvailable"`
			IsolatedMaxAvailable flexibleFloat `json:"isolatedMaxAvailable"`
			MaxTransferOut       flexibleFloat `json:"maxTransferOut"`
			AccountEquity        flexibleFloat `json:"accountEquity"`
			USDTEQuity           flexibleFloat `json:"usdtEquity"`
			UnionAvailable       flexibleFloat `json:"unionAvailable"`
			AssetList            []struct {
				Coin      string        `json:"coin"`
				Balance   flexibleFloat `json:"balance"`
				Available flexibleFloat `json:"available"`
			} `json:"assetList"`
		} `json:"data"`
	}

	if err := c.signedGET("/api/v2/mix/account/accounts", q, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bitget futures balance error: " + err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	if out.Code != "" && out.Code != "00000" {
		err := fmt.Errorf("Bitget balance code=%s msg=%s", out.Code, out.Msg)
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	for _, a := range out.Data {
		if strings.EqualFold(a.MarginCoin, marginCoinUSDT) {
			wallet := firstNonZero(float64(a.USDTEQuity), float64(a.AccountEquity))
			available := firstNonZero(
				float64(a.Available),
				float64(a.IsolatedMaxAvailable),
				float64(a.CrossedMaxAvailable),
				float64(a.UnionAvailable),
				float64(a.MaxTransferOut),
			)

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

	for _, a := range out.Data {
		for _, asset := range a.AssetList {
			if strings.EqualFold(asset.Coin, marginCoinUSDT) {
				return domain.Balance{
					Exchange:      c.Name(),
					WalletUSDT:    float64(asset.Balance),
					AvailableUSDT: float64(asset.Available),
					PrivateOK:     true,
					Error:         "",
					UpdatedAt:     time.Now().UTC(),
				}, nil
			}
		}
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "Bitget USDT futures balance not found",
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	intervalBySymbol := c.contractFundingIntervals()

	q := url.Values{}
	q.Set("productType", productTypeUSDTFutures)

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol              string        `json:"symbol"`
			LastPr              flexibleFloat `json:"lastPr"`
			MarkPrice           flexibleFloat `json:"markPrice"`
			IndexPrice          flexibleFloat `json:"indexPrice"`
			FundingRate         flexibleFloat `json:"fundingRate"`
			FundingRateInterval flexibleFloat `json:"fundingRateInterval"`
			FundInterval        flexibleFloat `json:"fundInterval"`
			NextUpdate          flexibleInt   `json:"nextUpdate"`
			NextFundingTime     flexibleInt   `json:"nextFundingTime"`
			UsdtVolume          flexibleFloat `json:"usdtVolume"`
			BaseVolume          flexibleFloat `json:"baseVolume"`
			QuoteVolume         flexibleFloat `json:"quoteVolume"`
			BidPr               flexibleFloat `json:"bidPr"`
			AskPr               flexibleFloat `json:"askPr"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v2/mix/market/tickers", q, &out); err != nil {
		return nil, err
	}

	if out.Code != "" && out.Code != "00000" {
		return nil, fmt.Errorf("Bitget tickers code=%s msg=%s", out.Code, out.Msg)
	}

	now := time.Now().UTC()
	res := make([]domain.Candidate, 0, len(out.Data))

	for _, t := range out.Data {
		symbol := normalizeSymbol(t.Symbol)
		if symbol == "" || !strings.Contains(symbol, "USDT") {
			continue
		}

		last := float64(t.LastPr)

		mark := float64(t.MarkPrice)
		if mark <= 0 {
			mark = float64(t.IndexPrice)
		}
		if mark <= 0 {
			mark = last
		}

		vol := firstNonZero(
			float64(t.UsdtVolume),
			float64(t.QuoteVolume),
			float64(t.BaseVolume)*last,
		)

		bid := float64(t.BidPr)
		ask := float64(t.AskPr)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		intervalHours := extractIntervalHours(t.FundingRateInterval, t.FundInterval)
		if intervalHours <= 0 {
			intervalHours = intervalBySymbol[symbol]
		}
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextRaw := int64(t.NextFundingTime)
		if nextRaw <= 0 {
			nextRaw = int64(t.NextUpdate)
		}

		nextFunding := parseMillisOrSeconds(nextRaw)
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         symbol,
			Price:                last,
			MarkPrice:            mark,
			FundingRate:          float64(t.FundingRate),
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        vol,
			Bid:                  bid,
			Ask:                  ask,
			Spread:               spread,
			UpdatedAt:            now,
		})
	}

	return res, nil
}

func (c *Client) contractFundingIntervals() map[string]float64 {
	out := map[string]float64{}

	q := url.Values{}
	q.Set("productType", productTypeUSDTFutures)

	var resp struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol       string        `json:"symbol"`
			FundInterval flexibleFloat `json:"fundInterval"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v2/mix/market/contracts", q, &resp); err != nil {
		return out
	}

	if resp.Code != "" && resp.Code != "00000" {
		return out
	}

	for _, item := range resp.Data {
		symbol := normalizeSymbol(item.Symbol)
		if symbol == "" {
			continue
		}

		interval := extractIntervalHours(item.FundInterval)
		if interval > 0 {
			out[symbol] = interval
		}
	}

	return out
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	symbol = normalizeSymbol(symbol)

	q := url.Values{}
	q.Set("productType", productTypeUSDTFutures)

	var resp struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol             string        `json:"symbol"`
			BaseCoin           string        `json:"baseCoin"`
			QuoteCoin          string        `json:"quoteCoin"`
			SettleCoin         string        `json:"settleCoin"`
			MinTradeNum        flexibleFloat `json:"minTradeNum"`
			MinTradeUSDT       flexibleFloat `json:"minTradeUSDT"`
			MaxTradeNum        flexibleFloat `json:"maxTradeNum"`
			SizeMultiplier     flexibleFloat `json:"sizeMultiplier"`
			PriceEndStep       flexibleFloat `json:"priceEndStep"`
			VolumePlace        flexibleInt   `json:"volumePlace"`
			PricePlace         flexibleInt   `json:"pricePlace"`
			SupportMarginCoins []string      `json:"supportMarginCoins"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v2/mix/market/contracts", q, &resp); err != nil {
		return domain.SymbolRules{}, err
	}

	if resp.Code != "" && resp.Code != "00000" {
		return domain.SymbolRules{}, fmt.Errorf("Bitget contracts code=%s msg=%s", resp.Code, resp.Msg)
	}

	for _, item := range resp.Data {
		if normalizeSymbol(item.Symbol) != symbol {
			continue
		}

		qtyStep := firstNonZero(float64(item.SizeMultiplier), precisionStep(int64(item.VolumePlace)), 0.001)
		tickSize := firstNonZero(float64(item.PriceEndStep), precisionStep(int64(item.PricePlace)))

		return domain.SymbolRules{
			Exchange:                    c.Name(),
			Symbol:                      symbol,
			NativeSymbol:                symbol,
			SupportsNotionalMarketOrder: false,
			MinQty:                      float64(item.MinTradeNum),
			MaxQty:                      float64(item.MaxTradeNum),
			QtyStep:                     qtyStep,
			MinNotional:                 float64(item.MinTradeUSDT),
			PriceStep:                   tickSize,
			TickSize:                    tickSize,
			ContractSize:                1,
			BaseAsset:                   item.BaseCoin,
			QuoteAsset:                  item.QuoteCoin,
			MarginAsset:                 firstNonEmpty(item.SettleCoin, marginCoinUSDT),
			UpdatedAt:                   time.Now().UTC(),
		}, nil
	}

	return domain.SymbolRules{}, fmt.Errorf("Bitget symbol rules not found: %s", symbol)
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

	_ = c.signedPOST("/api/v2/mix/account/set-position-mode", map[string]string{
		"productType": productTypeUSDTFutures,
		"posMode":     "one_way_mode",
	}, nil)

	var marginOut bitgetAPIResponse[any]
	if err := c.signedPOST("/api/v2/mix/account/set-margin-mode", map[string]string{
		"symbol":      symbol,
		"productType": productTypeUSDTFutures,
		"marginCoin":  marginCoinUSDT,
		"marginMode":  marginMode,
	}, &marginOut); err != nil {
		return err
	}

	if marginOut.Code != "" && marginOut.Code != "00000" && !isIgnorableBitgetMsg(marginOut.Msg) {
		return fmt.Errorf("Bitget set-margin-mode code=%s msg=%s", marginOut.Code, marginOut.Msg)
	}

	var levOut bitgetAPIResponse[any]
	if err := c.signedPOST("/api/v2/mix/account/set-leverage", map[string]string{
		"symbol":      symbol,
		"productType": productTypeUSDTFutures,
		"marginCoin":  marginCoinUSDT,
		"leverage":    strconv.Itoa(leverage),
	}, &levOut); err != nil {
		return err
	}

	if levOut.Code != "" && levOut.Code != "00000" && !isIgnorableBitgetMsg(levOut.Msg) {
		return fmt.Errorf("Bitget set-leverage code=%s msg=%s", levOut.Code, levOut.Msg)
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

	return domain.OrderResult{}, fmt.Errorf("Bitget unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
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
		return domain.OrderResult{}, fmt.Errorf("Bitget %s mark price is zero", symbol)
	}

	qty := req.Qty
	if qty <= 0 {
		qty = firstNonZero(req.NotionalUSDT, req.MarginUSDT) / price
	}

	qty = floorToStep(qty, rules.QtyStep)
	if qty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("Bitget %s calculated qty is zero", symbol)
	}

	notional := qty * price

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("Bitget %s qty %.12f < min qty %.12f", symbol, qty, rules.MinQty)
	}

	if rules.MinNotional > 0 && notional < rules.MinNotional {
		return domain.OrderResult{}, fmt.Errorf("Bitget %s notional %.8f < min notional %.8f", symbol, notional, rules.MinNotional)
	}

	clientOID := bitgetClientOID(req.ClientOrderID)

	body := map[string]string{
		"symbol":      symbol,
		"productType": productTypeUSDTFutures,
		"marginMode":  strings.ToLower(firstNonEmpty(req.MarginMode, "isolated")),
		"marginCoin":  marginCoinUSDT,
		"side":        "sell",
		"tradeSide":   "open",
		"orderType":   "market",
		"size":        formatFloat(qty),
		"reduceOnly":  "NO",
		"clientOid":   clientOID,
	}

	var out bitgetAPIResponse[bitgetOrderData]
	if err := c.signedPOST("/api/v2/mix/order/place-order", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "00000" {
		return domain.OrderResult{}, fmt.Errorf("Bitget market short code=%s msg=%s", out.Code, out.Msg)
	}

	res := out.Data.toDomain(c.Name(), symbol)
	if res.ExchangeOrderID == "" && res.ClientOrderID == "" {
		res.ClientOrderID = clientOID
	}

	if res.ExchangeOrderID != "" || res.ClientOrderID != "" {
		if fresh, err := c.GetOrderStatus(symbol, firstNonEmpty(res.ExchangeOrderID, res.ClientOrderID)); err == nil {
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
		return domain.OrderResult{}, fmt.Errorf("Bitget %s limit price is zero", symbol)
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
		return domain.OrderResult{}, fmt.Errorf("Bitget %s calculated limit qty is zero", symbol)
	}

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("Bitget %s qty %.12f < min qty %.12f", symbol, qty, rules.MinQty)
	}

	side := strings.ToLower(req.Side)
	if side == "" {
		return domain.OrderResult{}, fmt.Errorf("Bitget limit side is empty")
	}

	reduceOnly := "NO"
	if req.ReduceOnly {
		reduceOnly = "YES"
	}

	tradeSide := "open"
	if req.ReduceOnly {
		tradeSide = "close"
	}

	clientOID := bitgetClientOID(req.ClientOrderID)

	body := map[string]string{
		"symbol":      symbol,
		"productType": productTypeUSDTFutures,
		"marginMode":  strings.ToLower(firstNonEmpty(req.MarginMode, "isolated")),
		"marginCoin":  marginCoinUSDT,
		"side":        side,
		"tradeSide":   tradeSide,
		"orderType":   "limit",
		"force":       strings.ToLower(firstNonEmpty(req.TimeInForce, "gtc")),
		"price":       formatFloat(price),
		"size":        formatFloat(qty),
		"reduceOnly":  reduceOnly,
		"clientOid":   clientOID,
	}

	var out bitgetAPIResponse[bitgetOrderData]
	if err := c.signedPOST("/api/v2/mix/order/place-order", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "00000" {
		return domain.OrderResult{}, fmt.Errorf("Bitget limit order code=%s msg=%s", out.Code, out.Msg)
	}

	res := out.Data.toDomain(c.Name(), symbol)
	if res.ExchangeOrderID == "" && res.ClientOrderID == "" {
		res.ClientOrderID = clientOID
	}
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

	q := url.Values{}
	q.Set("symbol", symbol)
	q.Set("productType", productTypeUSDTFutures)

	if isNumeric(orderID) {
		q.Set("orderId", orderID)
	} else {
		q.Set("clientOid", bitgetClientOID(orderID))
	}

	var out bitgetAPIResponse[bitgetOrderData]
	if err := c.signedGET("/api/v2/mix/order/detail", q, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "00000" {
		return domain.OrderResult{}, fmt.Errorf("Bitget order detail code=%s msg=%s", out.Code, out.Msg)
	}

	return out.Data.toDomain(c.Name(), symbol), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	symbol = normalizeSymbol(symbol)

	body := map[string]string{
		"symbol":      symbol,
		"productType": productTypeUSDTFutures,
		"marginCoin":  marginCoinUSDT,
	}

	if isNumeric(orderID) {
		body["orderId"] = orderID
	} else {
		body["clientOid"] = bitgetClientOID(orderID)
	}

	var out bitgetAPIResponse[any]
	if err := c.signedPOST("/api/v2/mix/order/cancel-order", body, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "00000" && !isIgnorableCancel(out.Msg) {
		return fmt.Errorf("Bitget cancel order code=%s msg=%s", out.Code, out.Msg)
	}

	return nil
}

func (c *Client) CancelAll(symbol string) error {
	symbol = normalizeSymbol(symbol)

	body := map[string]string{
		"symbol":      symbol,
		"productType": productTypeUSDTFutures,
		"marginCoin":  marginCoinUSDT,
	}

	var out bitgetAPIResponse[any]
	if err := c.signedPOST("/api/v2/mix/order/cancel-all-orders", body, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "00000" && !isIgnorableCancel(out.Msg) {
		return fmt.Errorf("Bitget cancel all code=%s msg=%s", out.Code, out.Msg)
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

	side := "buy"
	if pos.Side == domain.SideLong {
		side = "sell"
	}

	body := map[string]string{
		"symbol":      symbol,
		"productType": productTypeUSDTFutures,
		"marginMode":  strings.ToLower(firstNonEmpty(pos.MarginMode, "isolated")),
		"marginCoin":  marginCoinUSDT,
		"side":        side,
		"tradeSide":   "close",
		"orderType":   "market",
		"size":        formatFloat(pos.Qty),
		"reduceOnly":  "YES",
		"clientOid":   bitgetClientOID("close-" + symbol + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)),
	}

	var out bitgetAPIResponse[bitgetOrderData]
	if err := c.signedPOST("/api/v2/mix/order/place-order", body, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "00000" {
		return fmt.Errorf("Bitget close market code=%s msg=%s", out.Code, out.Msg)
	}

	return nil
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	symbol = normalizeSymbol(symbol)

	q := url.Values{}
	q.Set("symbol", symbol)
	q.Set("productType", productTypeUSDTFutures)
	q.Set("marginCoin", marginCoinUSDT)

	var out bitgetAPIResponse[[]bitgetPositionData]
	if err := c.signedGET("/api/v2/mix/position/single-position", q, &out); err != nil {
		return domain.PositionInfo{}, err
	}

	if out.Code != "" && out.Code != "00000" {
		return domain.PositionInfo{}, fmt.Errorf("Bitget position code=%s msg=%s", out.Code, out.Msg)
	}

	for _, p := range out.Data {
		qty := math.Abs(float64(p.Total))
		if qty <= 0 {
			qty = math.Abs(float64(p.Available))
		}
		if qty <= 0 {
			continue
		}

		side := domain.SideShort
		if strings.EqualFold(p.HoldSide, "long") {
			side = domain.SideLong
		}

		return domain.PositionInfo{
			Exchange:         c.Name(),
			Symbol:           symbol,
			NativeSymbol:     symbol,
			Side:             side,
			Qty:              qty,
			NotionalUSDT:     qty * float64(p.MarkPrice),
			EntryPrice:       float64(p.OpenPriceAvg),
			MarkPrice:        float64(p.MarkPrice),
			UnrealizedPNL:    float64(p.UnrealizedPL),
			RealizedPNL:      float64(p.AchievedProfits),
			LiquidationPrice: float64(p.LiquidationPrice),
			Leverage:         int(float64(p.Leverage)),
			MarginMode:       strings.ToLower(p.MarginMode),
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
	return nil, nil
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
	req.Header.Set("locale", "en-US")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("Bitget GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("Bitget GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	return c.signedRequest(http.MethodGet, path, q, nil, dst)
}

func (c *Client) signedPOST(path string, body any, dst any) error {
	return c.signedRequest(http.MethodPost, path, nil, body, dst)
}

func (c *Client) signedDELETE(path string, body any, dst any) error {
	return c.signedRequest(http.MethodDelete, path, nil, body, dst)
}

func (c *Client) signedRequest(method, path string, q url.Values, body any, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return fmt.Errorf("Bitget API key, secret or passphrase is empty")
	}

	requestPath := path

	if q != nil && len(q) > 0 {
		requestPath += "?" + q.Encode()
	}

	bodyBytes := []byte{}
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	timestamp := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	signature := c.sign(timestamp + method + requestPath + string(bodyBytes))

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
	req.Header.Set("locale", "en-US")
	req.Header.Set("ACCESS-KEY", c.apiKey)
	req.Header.Set("ACCESS-SIGN", signature)
	req.Header.Set("ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("ACCESS-PASSPHRASE", c.passphrase)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("Bitget signed %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("Bitget signed %s %s decode error: %w; body=%s", method, path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type bitgetAPIResponse[T any] struct {
	Code        string      `json:"code"`
	Msg         string      `json:"msg"`
	RequestTime flexibleInt `json:"requestTime"`
	Data        T           `json:"data"`
}

type bitgetOrderData struct {
	OrderID     string        `json:"orderId"`
	ClientOID   string        `json:"clientOid"`
	Symbol      string        `json:"symbol"`
	Size        flexibleFloat `json:"size"`
	BaseVolume  flexibleFloat `json:"baseVolume"`
	QuoteVolume flexibleFloat `json:"quoteVolume"`
	Price       flexibleFloat `json:"price"`
	PriceAvg    flexibleFloat `json:"priceAvg"`
	Fee         flexibleFloat `json:"fee"`
	Side        string        `json:"side"`
	TradeSide   string        `json:"tradeSide"`
	OrderType   string        `json:"orderType"`
	Status      string        `json:"status"`
	ReduceOnly  string        `json:"reduceOnly"`
	CTime       flexibleInt   `json:"cTime"`
	UTime       flexibleInt   `json:"uTime"`
}

func (o bitgetOrderData) toDomain(exchange domain.ExchangeName, fallbackSymbol string) domain.OrderResult {
	symbol := normalizeSymbol(firstNonEmpty(o.Symbol, fallbackSymbol))

	qty := firstNonZero(float64(o.Size), float64(o.BaseVolume))

	createdAt := time.Now().UTC()
	if o.CTime > 0 {
		createdAt = parseMillisOrSeconds(int64(o.CTime))
	}

	updatedAt := time.Now().UTC()
	if o.UTime > 0 {
		updatedAt = parseMillisOrSeconds(int64(o.UTime))
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: o.OrderID,
		ClientOrderID:   o.ClientOID,
		Symbol:          symbol,
		NativeSymbol:    symbol,
		Side:            strings.ToUpper(o.Side),
		PositionSide:    strings.ToUpper(o.TradeSide),
		Type:            strings.ToUpper(o.OrderType),
		Status:          o.Status,
		OrderStatus:     mapBitgetOrderStatus(o.Status),
		Price:           float64(o.Price),
		AvgPrice:        float64(o.PriceAvg),
		Qty:             qty,
		FilledQty:       float64(o.BaseVolume),
		FilledNotional:  float64(o.QuoteVolume),
		RemainingQty:    math.Max(0, qty-float64(o.BaseVolume)),
		Fee:             float64(o.Fee),
		FeeAsset:        "USDT",
		ReduceOnly:      strings.EqualFold(o.ReduceOnly, "YES") || strings.EqualFold(o.ReduceOnly, "true"),
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

type bitgetPositionData struct {
	Symbol           string        `json:"symbol"`
	MarginCoin       string        `json:"marginCoin"`
	HoldSide         string        `json:"holdSide"`
	Total            flexibleFloat `json:"total"`
	Available        flexibleFloat `json:"available"`
	Locked           flexibleFloat `json:"locked"`
	OpenPriceAvg     flexibleFloat `json:"openPriceAvg"`
	MarginSize       flexibleFloat `json:"marginSize"`
	Leverage         flexibleFloat `json:"leverage"`
	UnrealizedPL     flexibleFloat `json:"unrealizedPL"`
	AchievedProfits  flexibleFloat `json:"achievedProfits"`
	LiquidationPrice flexibleFloat `json:"liquidationPrice"`
	MarginMode       string        `json:"marginMode"`
	MarkPrice        flexibleFloat `json:"markPrice"`
}

func (c *Client) markPrice(symbol string) float64 {
	symbol = normalizeSymbol(symbol)

	q := url.Values{}
	q.Set("productType", productTypeUSDTFutures)
	q.Set("symbol", symbol)

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol     string        `json:"symbol"`
			LastPr     flexibleFloat `json:"lastPr"`
			MarkPrice  flexibleFloat `json:"markPrice"`
			IndexPrice flexibleFloat `json:"indexPrice"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v2/mix/market/tickers", q, &out); err != nil {
		return 0
	}

	if out.Code != "" && out.Code != "00000" {
		return 0
	}

	for _, t := range out.Data {
		if normalizeSymbol(t.Symbol) != symbol {
			continue
		}

		return firstNonZero(float64(t.MarkPrice), float64(t.IndexPrice), float64(t.LastPr))
	}

	return 0
}

func mapBitgetOrderStatus(s string) domain.OrderStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "init", "new", "live", "not_trigger":
		return domain.OrderStatusNew
	case "partial-filled", "partially_filled", "partial":
		return domain.OrderStatusPartiallyFill
	case "filled", "full-fill", "full_filled":
		return domain.OrderStatusFilled
	case "cancelled", "canceled":
		return domain.OrderStatusCanceled
	case "fail", "rejected":
		return domain.OrderStatusRejected
	case "expired":
		return domain.OrderStatusExpired
	default:
		return domain.OrderStatusUnknown
	}
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

func normalizeSymbol(symbol string) string {
	return strings.ToUpper(strings.TrimSpace(symbol))
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

func precisionStep(precision int64) float64 {
	if precision <= 0 {
		return 0
	}

	return math.Pow10(-int(precision))
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

func bitgetClientOID(s string) string {
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

	if len(s) > 40 {
		s = s[len(s)-40:]
	}

	return s
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}

	_, err := strconv.ParseInt(s, 10, 64)

	return err == nil
}

func isIgnorableBitgetMsg(msg string) bool {
	msg = strings.ToLower(msg)

	return strings.Contains(msg, "no need") ||
		strings.Contains(msg, "same") ||
		strings.Contains(msg, "not modified") ||
		strings.Contains(msg, "already") ||
		strings.Contains(msg, "exist")
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

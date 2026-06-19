package mexc

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

type mexcFundingInfo struct {
	Symbol        string
	FundingRate   float64
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
	return domain.ExchangeMEXC
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Success bool        `json:"success"`
		Code    int         `json:"code"`
		Message string      `json:"message"`
		Data    flexibleInt `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/ping", nil, &out); err != nil {
		return time.Time{}, err
	}

	if !out.Success && out.Code != 0 {
		return time.Time{}, fmt.Errorf("MEXC ping code=%d msg=%s", out.Code, out.Message)
	}

	if out.Data > 0 {
		return time.UnixMilli(int64(out.Data)).UTC(), nil
	}

	return time.Now().UTC(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "MEXC API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	balance, err := c.balanceAssets()
	if err == nil && balance.PrivateOK {
		return balance, nil
	}

	single, singleErr := c.balanceAssetUSDT()
	if singleErr == nil && single.PrivateOK {
		return single, nil
	}

	if err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "MEXC futures balance error: " + err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	if singleErr != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "MEXC futures balance error: " + singleErr.Error(),
			UpdatedAt: time.Now().UTC(),
		}, singleErr
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "MEXC USDT futures balance not found",
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (c *Client) balanceAssets() (domain.Balance, error) {
	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			Currency         string        `json:"currency"`
			Asset            string        `json:"asset"`
			Equity           flexibleFloat `json:"equity"`
			WalletBalance    flexibleFloat `json:"walletBalance"`
			Balance          flexibleFloat `json:"balance"`
			AvailableBalance flexibleFloat `json:"availableBalance"`
			Available        flexibleFloat `json:"available"`
			CashBalance      flexibleFloat `json:"cashBalance"`
			PositionMargin   flexibleFloat `json:"positionMargin"`
			OrderMargin      flexibleFloat `json:"orderMargin"`
			FrozenBalance    flexibleFloat `json:"frozenBalance"`
		} `json:"data"`
	}

	if err := c.signedGET("/api/v1/private/account/assets", nil, &out); err != nil {
		return domain.Balance{}, err
	}

	if !out.Success && out.Code != 0 {
		return domain.Balance{}, fmt.Errorf("MEXC assets code=%d msg=%s", out.Code, out.Message)
	}

	for _, a := range out.Data {
		currency := strings.ToUpper(firstNonEmpty(a.Currency, a.Asset))
		if currency != "USDT" {
			continue
		}

		wallet := firstNonZero(
			float64(a.Equity),
			float64(a.WalletBalance),
			float64(a.Balance),
			float64(a.CashBalance),
		)

		available := firstNonZero(
			float64(a.AvailableBalance),
			float64(a.Available),
			float64(a.CashBalance),
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

	return domain.Balance{}, fmt.Errorf("MEXC USDT not found in account assets")
}

func (c *Client) balanceAssetUSDT() (domain.Balance, error) {
	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Currency         string        `json:"currency"`
			Asset            string        `json:"asset"`
			Equity           flexibleFloat `json:"equity"`
			WalletBalance    flexibleFloat `json:"walletBalance"`
			Balance          flexibleFloat `json:"balance"`
			AvailableBalance flexibleFloat `json:"availableBalance"`
			Available        flexibleFloat `json:"available"`
			CashBalance      flexibleFloat `json:"cashBalance"`
			PositionMargin   flexibleFloat `json:"positionMargin"`
			OrderMargin      flexibleFloat `json:"orderMargin"`
			FrozenBalance    flexibleFloat `json:"frozenBalance"`
		} `json:"data"`
	}

	if err := c.signedGET("/api/v1/private/account/asset/USDT", nil, &out); err != nil {
		return domain.Balance{}, err
	}

	if !out.Success && out.Code != 0 {
		return domain.Balance{}, fmt.Errorf("MEXC asset/USDT code=%d msg=%s", out.Code, out.Message)
	}

	a := out.Data

	wallet := firstNonZero(
		float64(a.Equity),
		float64(a.WalletBalance),
		float64(a.Balance),
		float64(a.CashBalance),
	)

	available := firstNonZero(
		float64(a.AvailableBalance),
		float64(a.Available),
		float64(a.CashBalance),
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

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	fundingBySymbol := c.fundingInfoMap()

	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			Symbol       string        `json:"symbol"`
			LastPrice    flexibleFloat `json:"lastPrice"`
			FairPrice    flexibleFloat `json:"fairPrice"`
			IndexPrice   flexibleFloat `json:"indexPrice"`
			MarkPrice    flexibleFloat `json:"markPrice"`
			Bid1         flexibleFloat `json:"bid1"`
			Ask1         flexibleFloat `json:"ask1"`
			Bid1Price    flexibleFloat `json:"bid1Price"`
			Ask1Price    flexibleFloat `json:"ask1Price"`
			Volume24     flexibleFloat `json:"volume24"`
			Amount24     flexibleFloat `json:"amount24"`
			HoldVol      flexibleFloat `json:"holdVol"`
			FundingRate  flexibleFloat `json:"fundingRate"`
			RiseFallRate flexibleFloat `json:"riseFallRate"`
			Timestamp    flexibleInt   `json:"timestamp"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/ticker", nil, &out); err != nil {
		return nil, err
	}

	if !out.Success && out.Code != 0 {
		return nil, fmt.Errorf("MEXC ticker code=%d msg=%s", out.Code, out.Message)
	}

	now := time.Now().UTC()
	res := make([]domain.Candidate, 0, len(out.Data))

	for _, t := range out.Data {
		symbol := normalizeSymbol(t.Symbol)
		if symbol == "" || !strings.Contains(symbol, "USDT") {
			continue
		}

		price := float64(t.LastPrice)

		mark := firstNonZero(
			float64(t.FairPrice),
			float64(t.MarkPrice),
			float64(t.IndexPrice),
			price,
		)

		funding := fundingBySymbol[symbol]

		fundingRate := float64(t.FundingRate)
		if fundingRate == 0 {
			fundingRate = funding.FundingRate
		}

		intervalHours := funding.IntervalHours
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := funding.NextFunding
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		bid := firstNonZero(float64(t.Bid1Price), float64(t.Bid1))
		ask := firstNonZero(float64(t.Ask1Price), float64(t.Ask1))

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		volumeUSDT := firstNonZero(
			float64(t.Amount24),
			float64(t.Volume24)*price,
		)

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         symbol,
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

	sort.Slice(res, func(i, j int) bool {
		if res[i].FundingRate == res[j].FundingRate {
			return res[i].Volume24hUSDT > res[j].Volume24hUSDT
		}

		return res[i].FundingRate > res[j].FundingRate
	})

	return res, nil
}

func (c *Client) fundingInfoMap() map[string]mexcFundingInfo {
	out := map[string]mexcFundingInfo{}

	symbols := c.contractSymbols()

	for _, symbol := range symbols {
		info, ok := c.fetchFundingInfo(symbol)
		if ok {
			out[symbol] = info
		}
	}

	return out
}

func (c *Client) contractSymbols() []string {
	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			Symbol string `json:"symbol"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/detail", nil, &out); err != nil {
		return nil
	}

	if !out.Success && out.Code != 0 {
		return nil
	}

	res := make([]string, 0, len(out.Data))

	for _, item := range out.Data {
		symbol := normalizeSymbol(item.Symbol)
		if symbol != "" && strings.Contains(symbol, "USDT") {
			res = append(res, symbol)
		}
	}

	return res
}

func (c *Client) fetchFundingInfo(symbol string) (mexcFundingInfo, bool) {
	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Symbol          string        `json:"symbol"`
			FundingRate     flexibleFloat `json:"fundingRate"`
			NextSettleTime  flexibleInt   `json:"nextSettleTime"`
			CollectCycle    flexibleInt   `json:"collectCycle"`
			FundingInterval flexibleInt   `json:"fundingInterval"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/funding_rate/"+url.PathEscape(symbol), nil, &out); err != nil {
		return mexcFundingInfo{}, false
	}

	if !out.Success && out.Code != 0 {
		return mexcFundingInfo{}, false
	}

	intervalHours := extractIntervalHoursFromInt(out.Data.CollectCycle)
	if intervalHours <= 0 {
		intervalHours = extractIntervalHoursFromInt(out.Data.FundingInterval)
	}
	if intervalHours <= 0 {
		intervalHours = 8
	}

	nextFunding := parseMillisOrSeconds(int64(out.Data.NextSettleTime))

	return mexcFundingInfo{
		Symbol:        symbol,
		FundingRate:   float64(out.Data.FundingRate),
		NextFunding:   nextFunding,
		IntervalHours: intervalHours,
		OK:            true,
	}, true
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	symbol = normalizeSymbol(symbol)

	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			Symbol       string        `json:"symbol"`
			DisplayName  string        `json:"displayName"`
			BaseCoin     string        `json:"baseCoin"`
			QuoteCoin    string        `json:"quoteCoin"`
			SettleCoin   string        `json:"settleCoin"`
			ContractSize flexibleFloat `json:"contractSize"`
			MinVol       flexibleFloat `json:"minVol"`
			MaxVol       flexibleFloat `json:"maxVol"`
			VolScale     flexibleInt   `json:"volScale"`
			PriceScale   flexibleInt   `json:"priceScale"`
			PriceUnit    flexibleFloat `json:"priceUnit"`
			AmountScale  flexibleInt   `json:"amountScale"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/detail", nil, &out); err != nil {
		return domain.SymbolRules{}, err
	}

	if !out.Success && out.Code != 0 {
		return domain.SymbolRules{}, fmt.Errorf("MEXC contract detail code=%d msg=%s", out.Code, out.Message)
	}

	for _, item := range out.Data {
		if normalizeSymbol(item.Symbol) != symbol {
			continue
		}

		qtyStep := precisionStep(int64(item.VolScale))
		if qtyStep <= 0 {
			qtyStep = 1
		}

		tickSize := firstNonZero(float64(item.PriceUnit), precisionStep(int64(item.PriceScale)))

		return domain.SymbolRules{
			Exchange:                    c.Name(),
			Symbol:                      symbol,
			NativeSymbol:                symbol,
			SupportsNotionalMarketOrder: false,
			MinQty:                      firstNonZero(float64(item.MinVol), qtyStep),
			MaxQty:                      float64(item.MaxVol),
			QtyStep:                     qtyStep,
			MinNotional:                 0,
			PriceStep:                   tickSize,
			TickSize:                    tickSize,
			ContractSize:                firstNonZero(float64(item.ContractSize), 1),
			BaseAsset:                   item.BaseCoin,
			QuoteAsset:                  firstNonEmpty(item.QuoteCoin, "USDT"),
			MarginAsset:                 firstNonEmpty(item.SettleCoin, "USDT"),
			UpdatedAt:                   time.Now().UTC(),
		}, nil
	}

	return domain.SymbolRules{}, fmt.Errorf("MEXC symbol rules not found: %s", symbol)
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	symbol = normalizeSymbol(symbol)

	if leverage <= 0 {
		leverage = 1
	}

	if err := c.setLeverage(symbol, leverage, 3); err != nil {
		if !isIgnorableMEXCMsg(err.Error()) {
			return err
		}
	}

	return nil
}

func (c *Client) setLeverage(symbol string, leverage int, positionType int) error {
	body := map[string]any{
		"symbol":       symbol,
		"leverage":     leverage,
		"positionType": positionType,
	}

	var out mexcResponse[any]
	if err := c.signedPOST("/api/v1/private/position/change_leverage", body, &out); err != nil {
		return err
	}

	if !out.Success && out.Code != 0 && !isIgnorableMEXCMsg(out.Message) {
		return fmt.Errorf("MEXC change leverage code=%d msg=%s", out.Code, out.Message)
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

	return domain.OrderResult{}, fmt.Errorf("MEXC unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
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
		return domain.OrderResult{}, fmt.Errorf("MEXC %s mark price is zero", symbol)
	}

	contractsQty := req.Qty
	if contractsQty <= 0 {
		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		contractsQty = notional / price / firstNonZero(rules.ContractSize, 1)
	}

	contractsQty = floorToStep(contractsQty, rules.QtyStep)
	if contractsQty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("MEXC %s calculated contracts qty is zero", symbol)
	}

	if rules.MinQty > 0 && contractsQty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("MEXC %s qty %.12f < min qty %.12f", symbol, contractsQty, rules.MinQty)
	}

	body := map[string]any{
		"symbol":      symbol,
		"vol":         formatFloat(contractsQty),
		"side":        3,
		"type":        5,
		"openType":    mexcOpenType(req.MarginMode),
		"leverage":    firstNonZeroInt(req.Leverage, 1),
		"externalOid": mexcClientOID(req.ClientOrderID),
	}

	var out mexcResponse[mexcOrderSubmitData]
	if err := c.signedPOST("/api/v1/private/order/submit", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if !out.Success && out.Code != 0 {
		return domain.OrderResult{}, fmt.Errorf("MEXC market short code=%d msg=%s", out.Code, out.Message)
	}

	orderID := firstNonEmpty(out.Data.OrderID, out.Data.ID, out.Data.ExternalOID)

	res := domain.OrderResult{
		Exchange:        c.Name(),
		ExchangeOrderID: orderID,
		ClientOrderID:   mexcClientOID(req.ClientOrderID),
		Symbol:          symbol,
		NativeSymbol:    symbol,
		Side:            "SELL",
		Type:            string(domain.OrderTypeMarket),
		Status:          "submitted",
		OrderStatus:     domain.OrderStatusNew,
		Price:           price,
		Qty:             contractsQty,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}

	if orderID != "" {
		if fresh, err := c.GetOrderStatus(symbol, orderID); err == nil {
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
		return domain.OrderResult{}, fmt.Errorf("MEXC %s limit price is zero", symbol)
	}

	price = floorToStep(price, rules.TickSize)

	contractsQty := req.Qty
	if contractsQty <= 0 {
		ref := c.markPrice(symbol)
		if ref <= 0 {
			ref = price
		}

		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		contractsQty = notional / ref / firstNonZero(rules.ContractSize, 1)
	}

	contractsQty = floorToStep(contractsQty, rules.QtyStep)
	if contractsQty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("MEXC %s calculated contracts qty is zero", symbol)
	}

	if rules.MinQty > 0 && contractsQty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("MEXC %s qty %.12f < min qty %.12f", symbol, contractsQty, rules.MinQty)
	}

	side := mexcOrderSide(req.Side, req.ReduceOnly)

	body := map[string]any{
		"symbol":      symbol,
		"price":       formatFloat(price),
		"vol":         formatFloat(contractsQty),
		"side":        side,
		"type":        1,
		"openType":    mexcOpenType(req.MarginMode),
		"leverage":    firstNonZeroInt(req.Leverage, 1),
		"externalOid": mexcClientOID(req.ClientOrderID),
	}

	var out mexcResponse[mexcOrderSubmitData]
	if err := c.signedPOST("/api/v1/private/order/submit", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if !out.Success && out.Code != 0 {
		return domain.OrderResult{}, fmt.Errorf("MEXC limit order code=%d msg=%s", out.Code, out.Message)
	}

	orderID := firstNonEmpty(out.Data.OrderID, out.Data.ID, out.Data.ExternalOID)

	return domain.OrderResult{
		Exchange:        c.Name(),
		ExchangeOrderID: orderID,
		ClientOrderID:   mexcClientOID(req.ClientOrderID),
		Symbol:          symbol,
		NativeSymbol:    symbol,
		Side:            strings.ToUpper(req.Side),
		Type:            string(domain.OrderTypeLimit),
		Status:          "submitted",
		OrderStatus:     domain.OrderStatusNew,
		Price:           price,
		Qty:             contractsQty,
		ReduceOnly:      req.ReduceOnly,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
	}, nil
}

func (c *Client) GetOrderStatus(symbol, orderID string) (domain.OrderResult, error) {
	symbol = normalizeSymbol(symbol)

	var out mexcResponse[mexcOrderData]
	if err := c.signedGET("/api/v1/private/order/get/"+url.PathEscape(orderID), nil, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if !out.Success && out.Code != 0 {
		return domain.OrderResult{}, fmt.Errorf("MEXC order status code=%d msg=%s", out.Code, out.Message)
	}

	rules, _ := c.SymbolRules(symbol)

	return out.Data.toDomain(c.Name(), symbol, rules.ContractSize), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	var out mexcResponse[any]
	if err := c.signedPOST("/api/v1/private/order/cancel/"+url.PathEscape(orderID), map[string]any{}, &out); err != nil {
		return err
	}

	if !out.Success && out.Code != 0 && !isIgnorableCancel(out.Message) {
		return fmt.Errorf("MEXC cancel order code=%d msg=%s", out.Code, out.Message)
	}

	return nil
}

func (c *Client) CancelAll(symbol string) error {
	symbol = normalizeSymbol(symbol)

	body := map[string]any{
		"symbol": symbol,
	}

	var out mexcResponse[any]
	if err := c.signedPOST("/api/v1/private/order/cancel_all", body, &out); err != nil {
		return err
	}

	if !out.Success && out.Code != 0 && !isIgnorableCancel(out.Message) {
		return fmt.Errorf("MEXC cancel all code=%d msg=%s", out.Code, out.Message)
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

	side := 2
	if pos.Side == domain.SideLong {
		side = 4
	}

	body := map[string]any{
		"symbol":      symbol,
		"vol":         formatFloat(pos.Qty),
		"side":        side,
		"type":        5,
		"openType":    mexcOpenType(pos.MarginMode),
		"externalOid": mexcClientOID("close-" + symbol + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)),
	}

	var out mexcResponse[mexcOrderSubmitData]
	if err := c.signedPOST("/api/v1/private/order/submit", body, &out); err != nil {
		return err
	}

	if !out.Success && out.Code != 0 {
		return fmt.Errorf("MEXC close market code=%d msg=%s", out.Code, out.Message)
	}

	return nil
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	symbol = normalizeSymbol(symbol)

	q := url.Values{}
	q.Set("symbol", symbol)

	var out mexcResponse[[]mexcPositionData]
	if err := c.signedGET("/api/v1/private/position/open_positions", q, &out); err != nil {
		return domain.PositionInfo{}, err
	}

	if !out.Success && out.Code != 0 {
		return domain.PositionInfo{}, fmt.Errorf("MEXC positions code=%d msg=%s", out.Code, out.Message)
	}

	for _, p := range out.Data {
		if normalizeSymbol(p.Symbol) != symbol {
			continue
		}

		qty := math.Abs(float64(p.HoldVol))
		if qty <= 0 {
			continue
		}

		side := domain.SideShort
		if p.PositionType == 1 {
			side = domain.SideLong
		}

		return domain.PositionInfo{
			Exchange:         c.Name(),
			Symbol:           symbol,
			NativeSymbol:     symbol,
			Side:             side,
			Qty:              qty,
			NotionalUSDT:     math.Abs(float64(p.HoldVol) * float64(p.MarkPrice) * float64(p.ContractSize)),
			EntryPrice:       float64(p.OpenAvgPrice),
			MarkPrice:        float64(p.MarkPrice),
			UnrealizedPNL:    float64(p.Unrealised),
			RealizedPNL:      float64(p.Realised),
			LiquidationPrice: float64(p.LiquidatePrice),
			Leverage:         int(float64(p.Leverage)),
			MarginMode:       mexcMarginModeName(int(float64(p.OpenType))),
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

func (c *Client) markPrice(symbol string) float64 {
	symbol = normalizeSymbol(symbol)

	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			Symbol     string        `json:"symbol"`
			LastPrice  flexibleFloat `json:"lastPrice"`
			FairPrice  flexibleFloat `json:"fairPrice"`
			IndexPrice flexibleFloat `json:"indexPrice"`
			MarkPrice  flexibleFloat `json:"markPrice"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/ticker", nil, &out); err != nil {
		return 0
	}

	if !out.Success && out.Code != 0 {
		return 0
	}

	for _, t := range out.Data {
		if normalizeSymbol(t.Symbol) != symbol {
			continue
		}

		return firstNonZero(float64(t.FairPrice), float64(t.MarkPrice), float64(t.IndexPrice), float64(t.LastPrice))
	}

	return 0
}

type mexcResponse[T any] struct {
	Success bool   `json:"success"`
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type mexcOrderSubmitData struct {
	OrderID     string `json:"orderId"`
	ID          string `json:"id"`
	ExternalOID string `json:"externalOid"`
}

type mexcOrderData struct {
	ID           flexibleID    `json:"id"`
	OrderID      flexibleID    `json:"orderId"`
	ExternalOID  string        `json:"externalOid"`
	Symbol       string        `json:"symbol"`
	Price        flexibleFloat `json:"price"`
	Vol          flexibleFloat `json:"vol"`
	DealVol      flexibleFloat `json:"dealVol"`
	RemainVol    flexibleFloat `json:"remainVol"`
	AvgPrice     flexibleFloat `json:"avgPrice"`
	DealAvgPrice flexibleFloat `json:"dealAvgPrice"`
	Side         flexibleInt   `json:"side"`
	Type         flexibleInt   `json:"type"`
	State        flexibleInt   `json:"state"`
	Fee          flexibleFloat `json:"fee"`
	CreateTime   flexibleInt   `json:"createTime"`
	UpdateTime   flexibleInt   `json:"updateTime"`
}

func (o mexcOrderData) toDomain(exchange domain.ExchangeName, fallbackSymbol string, contractSize float64) domain.OrderResult {
	symbol := normalizeSymbol(firstNonEmpty(o.Symbol, fallbackSymbol))

	qty := float64(o.Vol)
	filled := float64(o.DealVol)
	if filled <= 0 && o.RemainVol > 0 {
		filled = math.Max(0, qty-float64(o.RemainVol))
	}

	side := mexcSideText(int(o.Side))
	orderType := mexcTypeText(int(o.Type))

	price := float64(o.Price)
	avg := firstNonZero(float64(o.AvgPrice), float64(o.DealAvgPrice))

	createdAt := time.Now().UTC()
	if o.CreateTime > 0 {
		createdAt = parseMillisOrSeconds(int64(o.CreateTime))
	}

	updatedAt := time.Now().UTC()
	if o.UpdateTime > 0 {
		updatedAt = parseMillisOrSeconds(int64(o.UpdateTime))
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: firstNonEmpty(string(o.OrderID), string(o.ID)),
		ClientOrderID:   o.ExternalOID,
		Symbol:          symbol,
		NativeSymbol:    symbol,
		Side:            side,
		Type:            orderType,
		Status:          strconv.Itoa(int(o.State)),
		OrderStatus:     mapMEXCOrderStatus(int(o.State), qty, filled),
		Price:           price,
		AvgPrice:        avg,
		Qty:             qty,
		FilledQty:       filled,
		FilledNotional:  filled * firstNonZero(avg, price) * firstNonZero(contractSize, 1),
		RemainingQty:    math.Max(0, qty-filled),
		Fee:             float64(o.Fee),
		FeeAsset:        "USDT",
		ReduceOnly:      int(o.Side) == 2 || int(o.Side) == 4,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

type mexcPositionData struct {
	Symbol         string        `json:"symbol"`
	PositionType   flexibleInt   `json:"positionType"`
	OpenType       flexibleFloat `json:"openType"`
	State          flexibleInt   `json:"state"`
	HoldVol        flexibleFloat `json:"holdVol"`
	FrozenVol      flexibleFloat `json:"frozenVol"`
	CloseVol       flexibleFloat `json:"closeVol"`
	HoldAvgPrice   flexibleFloat `json:"holdAvgPrice"`
	OpenAvgPrice   flexibleFloat `json:"openAvgPrice"`
	MarkPrice      flexibleFloat `json:"markPrice"`
	LiquidatePrice flexibleFloat `json:"liquidatePrice"`
	Leverage       flexibleFloat `json:"leverage"`
	Unrealised     flexibleFloat `json:"unrealised"`
	Realised       flexibleFloat `json:"realised"`
	ContractSize   flexibleFloat `json:"contractSize"`
	CreateTime     flexibleInt   `json:"createTime"`
	UpdateTime     flexibleInt   `json:"updateTime"`
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
		return fmt.Errorf("MEXC GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("MEXC GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	return c.signedRequest(http.MethodGet, path, q, nil, dst)
}

func (c *Client) signedPOST(path string, body any, dst any) error {
	return c.signedRequest(http.MethodPost, path, nil, body, dst)
}

func (c *Client) signedRequest(method, path string, q url.Values, body any, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("MEXC API key or secret is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	requestPath := path
	query := q.Encode()

	if query != "" {
		requestPath += "?" + query
	}

	bodyBytes := []byte{}
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}

	reqTime := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	signPayload := query
	if len(bodyBytes) > 0 {
		signPayload = string(bodyBytes)
	}

	signature := c.sign(reqTime, signPayload)

	u := c.baseURL + requestPath

	var reader io.Reader
	if len(bodyBytes) > 0 {
		reader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequest(method, u, reader)
	if err != nil {
		return err
	}

	req.Header.Set("ApiKey", c.apiKey)
	req.Header.Set("Request-Time", reqTime)
	req.Header.Set("Signature", signature)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")

	if len(bodyBytes) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("MEXC signed %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("MEXC signed %s %s decode error: %w; body=%s", method, path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(reqTime, payload string) string {
	raw := c.apiKey + reqTime + payload

	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(raw))

	return hex.EncodeToString(mac.Sum(nil))
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

func extractIntervalHoursFromInt(v flexibleInt) float64 {
	raw := int64(v)
	if raw <= 0 {
		return 0
	}

	if raw >= 3600000 && raw%3600000 == 0 {
		return float64(raw) / 3600000
	}

	if raw >= 3600 && raw%3600 == 0 {
		return float64(raw) / 3600
	}

	if raw >= 60 && raw <= 1440 && raw%60 == 0 {
		return float64(raw) / 60
	}

	if raw > 0 && raw <= 24 {
		return float64(raw)
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

func mexcClientOID(s string) string {
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

	if len(s) > 32 {
		s = s[len(s)-32:]
	}

	return s
}

func mexcOpenType(marginMode string) int {
	marginMode = strings.ToLower(strings.TrimSpace(marginMode))

	if marginMode == "cross" || marginMode == "crossed" {
		return 2
	}

	return 1
}

func mexcMarginModeName(openType int) string {
	if openType == 2 {
		return "cross"
	}

	return "isolated"
}

func mexcOrderSide(side string, reduceOnly bool) int {
	side = strings.ToUpper(strings.TrimSpace(side))

	if side == "BUY" {
		if reduceOnly {
			return 2
		}

		return 1
	}

	if reduceOnly {
		return 4
	}

	return 3
}

func mexcSideText(side int) string {
	switch side {
	case 1, 2:
		return "BUY"
	case 3, 4:
		return "SELL"
	default:
		return ""
	}
}

func mexcTypeText(t int) string {
	if t == 5 || t == 6 {
		return string(domain.OrderTypeMarket)
	}

	return string(domain.OrderTypeLimit)
}

func mapMEXCOrderStatus(state int, qty float64, filled float64) domain.OrderStatus {
	switch state {
	case 1, 2:
		return domain.OrderStatusNew
	case 3:
		return domain.OrderStatusFilled
	case 4:
		return domain.OrderStatusCanceled
	case 5:
		if filled > 0 && filled < qty {
			return domain.OrderStatusPartiallyFill
		}

		return domain.OrderStatusCanceled
	default:
		return domain.OrderStatusUnknown
	}
}

func firstNonZeroInt(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}

	return 0
}

func isIgnorableMEXCMsg(msg string) bool {
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

package kucoin

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
	"sort"
	"strconv"
	"strings"
	"time"

	"funding-bot/internal/domain"
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
		pp = os.Getenv("KUCOIN_API_PASSPHRASE")
	}

	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		apiSecret:  strings.TrimSpace(apiSecret),
		passphrase: strings.TrimSpace(pp),
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeKuCoin
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out kucoinResponse[flexibleInt]

	if err := c.getJSON("/api/v1/timestamp", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.Code != "" && out.Code != "200000" {
		return time.Time{}, fmt.Errorf("KuCoin timestamp code=%s msg=%s", out.Code, out.Msg)
	}

	if out.Data > 0 {
		return time.UnixMilli(int64(out.Data)).UTC(), nil
	}

	return time.Now().UTC(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "KuCoin API key, secret or passphrase is empty",
			UpdatedAt: now,
		}, nil
	}

	q := url.Values{}
	q.Set("currency", "USDT")

	var out kucoinResponse[struct {
		Currency         string        `json:"currency"`
		AccountEquity    flexibleFloat `json:"accountEquity"`
		UnrealisedPNL    flexibleFloat `json:"unrealisedPNL"`
		MarginBalance    flexibleFloat `json:"marginBalance"`
		PositionMargin   flexibleFloat `json:"positionMargin"`
		OrderMargin      flexibleFloat `json:"orderMargin"`
		FrozenFunds      flexibleFloat `json:"frozenFunds"`
		AvailableBalance flexibleFloat `json:"availableBalance"`
	}]

	if err := c.signedGET("/api/v1/account-overview", q, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "KuCoin futures balance error: " + err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	if out.Code != "" && out.Code != "200000" {
		err := fmt.Errorf("KuCoin balance code=%s msg=%s", out.Code, out.Msg)

		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	wallet := firstNonZero(float64(out.Data.AccountEquity), float64(out.Data.MarginBalance))
	available := float64(out.Data.AvailableBalance)

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
	var out kucoinResponse[[]kucoinContract]

	if err := c.getJSON("/api/v1/contracts/active", nil, &out); err != nil {
		return nil, err
	}

	if out.Code != "" && out.Code != "200000" {
		return nil, fmt.Errorf("KuCoin contracts code=%s msg=%s", out.Code, out.Msg)
	}

	now := time.Now().UTC()
	rows := make([]domain.Candidate, 0, len(out.Data))

	for _, item := range out.Data {
		if !strings.EqualFold(item.QuoteCurrency, "USDT") && !strings.Contains(item.Symbol, "USDT") {
			continue
		}

		symbol := kucoinUnifiedSymbol(item.Symbol)

		price := firstNonZero(
			float64(item.LastTradePrice),
			float64(item.MarkPrice),
			float64(item.IndexPrice),
		)

		mark := firstNonZero(float64(item.MarkPrice), float64(item.IndexPrice), price)

		fundingRate := firstNonZero(
			float64(item.FundingFeeRate),
			float64(item.PredictedFundingFeeRate),
		)

		intervalHours := extractIntervalHours(item.FundingRateGranularity)
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := parseMillisOrSeconds(int64(item.NextFundingRateTime))
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		bid := float64(item.Buy)
		ask := float64(item.Sell)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		volumeUSDT := firstNonZero(
			float64(item.TurnoverOf24h),
			float64(item.VolumeOf24h)*price,
		)

		rows = append(rows, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         item.Symbol,
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

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].FundingRate == rows[j].FundingRate {
			return rows[i].Volume24hUSDT > rows[j].Volume24hUSDT
		}

		return rows[i].FundingRate > rows[j].FundingRate
	})

	return rows, nil
}

type kucoinContract struct {
	Symbol                  string        `json:"symbol"`
	RootSymbol              string        `json:"rootSymbol"`
	Type                    string        `json:"type"`
	BaseCurrency            string        `json:"baseCurrency"`
	QuoteCurrency           string        `json:"quoteCurrency"`
	SettleCurrency          string        `json:"settleCurrency"`
	MaxOrderQty             flexibleFloat `json:"maxOrderQty"`
	LotSize                 flexibleFloat `json:"lotSize"`
	TickSize                flexibleFloat `json:"tickSize"`
	Multiplier              flexibleFloat `json:"multiplier"`
	Status                  string        `json:"status"`
	FundingFeeRate          flexibleFloat `json:"fundingFeeRate"`
	PredictedFundingFeeRate flexibleFloat `json:"predictedFundingFeeRate"`
	FundingRateGranularity  flexibleInt   `json:"fundingRateGranularity"`
	TurnoverOf24h           flexibleFloat `json:"turnoverOf24h"`
	VolumeOf24h             flexibleFloat `json:"volumeOf24h"`
	MarkPrice               flexibleFloat `json:"markPrice"`
	IndexPrice              flexibleFloat `json:"indexPrice"`
	LastTradePrice          flexibleFloat `json:"lastTradePrice"`
	NextFundingRateTime     flexibleInt   `json:"nextFundingRateTime"`
	MaxLeverage             flexibleFloat `json:"maxLeverage"`
	Buy                     flexibleFloat `json:"buy"`
	Sell                    flexibleFloat `json:"sell"`
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	native := normalizeKuCoinSymbol(symbol)

	var out kucoinResponse[kucoinContract]

	if err := c.getJSON("/api/v1/contracts/"+url.PathEscape(native), nil, &out); err != nil {
		return domain.SymbolRules{}, err
	}

	if out.Code != "" && out.Code != "200000" {
		return domain.SymbolRules{}, fmt.Errorf("KuCoin contract code=%s msg=%s", out.Code, out.Msg)
	}

	item := out.Data

	if item.Symbol == "" {
		return domain.SymbolRules{}, fmt.Errorf("KuCoin symbol rules not found: %s", native)
	}

	return domain.SymbolRules{
		Exchange:                    c.Name(),
		Symbol:                      kucoinUnifiedSymbol(item.Symbol),
		NativeSymbol:                item.Symbol,
		SupportsNotionalMarketOrder: false,
		MinQty:                      firstNonZero(float64(item.LotSize), 1),
		MaxQty:                      float64(item.MaxOrderQty),
		QtyStep:                     firstNonZero(float64(item.LotSize), 1),
		MinNotional:                 0,
		PriceStep:                   float64(item.TickSize),
		TickSize:                    float64(item.TickSize),
		ContractSize:                firstNonZero(float64(item.Multiplier), 1),
		BaseAsset:                   item.BaseCurrency,
		QuoteAsset:                  firstNonEmpty(item.QuoteCurrency, "USDT"),
		MarginAsset:                 firstNonEmpty(item.SettleCurrency, "USDT"),
		UpdatedAt:                   time.Now().UTC(),
	}, nil
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	// На KuCoin Futures плечо передаётся в каждом ордере.
	// Отдельный endpoint смены плеча для этой стратегии не нужен.
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

	return domain.OrderResult{}, fmt.Errorf("KuCoin unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
}

func (c *Client) PlaceMarketShort(req domain.OrderRequest) (domain.OrderResult, error) {
	native := normalizeKuCoinSymbol(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(native)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		price = c.markPrice(native)
	}
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("KuCoin %s mark price is zero", native)
	}

	contractsQty := req.Qty
	if contractsQty <= 0 {
		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		contractsQty = notional / price / firstNonZero(rules.ContractSize, 1)
	}

	contractsQty = floorToStep(contractsQty, rules.QtyStep)
	if contractsQty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("KuCoin %s calculated contracts qty is zero", native)
	}

	if rules.MinQty > 0 && contractsQty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("KuCoin %s qty %.12f < min qty %.12f", native, contractsQty, rules.MinQty)
	}

	clientOID := kucoinClientOID(req.ClientOrderID)

	body := map[string]any{
		"clientOid": clientOID,
		"side":      "sell",
		"symbol":    native,
		"type":      "market",
		"size":      int64(math.Abs(contractsQty)),
		"leverage":  strconv.Itoa(firstNonZeroInt(req.Leverage, 1)),
	}

	var out kucoinResponse[kucoinOrderData]
	if err := c.signedPOST("/api/v1/orders", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "200000" {
		return domain.OrderResult{}, fmt.Errorf("KuCoin market short code=%s msg=%s", out.Code, out.Msg)
	}

	res := out.Data.toDomain(c.Name(), native, rules.ContractSize)
	if res.ExchangeOrderID == "" {
		res.ExchangeOrderID = out.Data.OrderID
	}
	if res.ClientOrderID == "" {
		res.ClientOrderID = clientOID
	}

	if res.ExchangeOrderID != "" {
		if fresh, err := c.GetOrderStatus(native, res.ExchangeOrderID); err == nil {
			res = fresh
		}
	}

	if res.Price <= 0 {
		res.Price = price
	}
	if res.Qty <= 0 {
		res.Qty = math.Abs(contractsQty)
	}

	return res, nil
}

func (c *Client) PlaceLimitReduceOnly(req domain.OrderRequest) (domain.OrderResult, error) {
	native := normalizeKuCoinSymbol(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(native)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("KuCoin %s limit price is zero", native)
	}

	price = floorToStep(price, rules.TickSize)

	contractsQty := req.Qty
	if contractsQty <= 0 {
		ref := c.markPrice(native)
		if ref <= 0 {
			ref = price
		}

		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		contractsQty = notional / ref / firstNonZero(rules.ContractSize, 1)
	}

	contractsQty = floorToStep(contractsQty, rules.QtyStep)
	if contractsQty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("KuCoin %s calculated contracts qty is zero", native)
	}

	if rules.MinQty > 0 && contractsQty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("KuCoin %s qty %.12f < min qty %.12f", native, contractsQty, rules.MinQty)
	}

	side := strings.ToLower(req.Side)
	if side == "" {
		return domain.OrderResult{}, fmt.Errorf("KuCoin limit side is empty")
	}

	clientOID := kucoinClientOID(req.ClientOrderID)

	body := map[string]any{
		"clientOid":   clientOID,
		"side":        side,
		"symbol":      native,
		"type":        "limit",
		"price":       formatFloat(price),
		"size":        int64(math.Abs(contractsQty)),
		"leverage":    strconv.Itoa(firstNonZeroInt(req.Leverage, 1)),
		"timeInForce": kucoinTimeInForce(req.TimeInForce),
	}

	if req.ReduceOnly {
		body["reduceOnly"] = true
	}

	var out kucoinResponse[kucoinOrderData]
	if err := c.signedPOST("/api/v1/orders", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "200000" {
		return domain.OrderResult{}, fmt.Errorf("KuCoin limit order code=%s msg=%s", out.Code, out.Msg)
	}

	res := out.Data.toDomain(c.Name(), native, rules.ContractSize)
	if res.ExchangeOrderID == "" {
		res.ExchangeOrderID = out.Data.OrderID
	}
	if res.ClientOrderID == "" {
		res.ClientOrderID = clientOID
	}

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
	native := normalizeKuCoinSymbol(symbol)

	var out kucoinResponse[kucoinOrderData]
	if err := c.signedGET("/api/v1/orders/"+url.PathEscape(orderID), nil, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "200000" {
		return domain.OrderResult{}, fmt.Errorf("KuCoin order status code=%s msg=%s", out.Code, out.Msg)
	}

	rules, _ := c.SymbolRules(native)

	return out.Data.toDomain(c.Name(), native, rules.ContractSize), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	var out kucoinResponse[any]
	if err := c.signedDELETE("/api/v1/orders/"+url.PathEscape(orderID), nil, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "200000" && !isIgnorableCancel(out.Msg) {
		return fmt.Errorf("KuCoin cancel order code=%s msg=%s", out.Code, out.Msg)
	}

	return nil
}

func (c *Client) CancelAll(symbol string) error {
	native := normalizeKuCoinSymbol(symbol)

	q := url.Values{}
	q.Set("symbol", native)

	var out kucoinResponse[any]
	if err := c.signedDELETE("/api/v1/orders", q, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "200000" && !isIgnorableCancel(out.Msg) {
		return fmt.Errorf("KuCoin cancel all code=%s msg=%s", out.Code, out.Msg)
	}

	return nil
}

func (c *Client) ClosePositionMarket(symbol string) error {
	native := normalizeKuCoinSymbol(symbol)

	pos, err := c.GetPosition(native)
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

	body := map[string]any{
		"clientOid":  kucoinClientOID("close-" + native + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)),
		"side":       side,
		"symbol":     native,
		"type":       "market",
		"size":       int64(math.Abs(pos.Qty)),
		"reduceOnly": true,
	}

	var out kucoinResponse[kucoinOrderData]
	if err := c.signedPOST("/api/v1/orders", body, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "200000" {
		return fmt.Errorf("KuCoin close market code=%s msg=%s", out.Code, out.Msg)
	}

	return nil
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	native := normalizeKuCoinSymbol(symbol)

	q := url.Values{}
	q.Set("symbol", native)

	var out kucoinResponse[kucoinPositionData]
	if err := c.signedGET("/api/v1/position", q, &out); err != nil {
		return domain.PositionInfo{}, err
	}

	if out.Code != "" && out.Code != "200000" {
		return domain.PositionInfo{}, fmt.Errorf("KuCoin position code=%s msg=%s", out.Code, out.Msg)
	}

	p := out.Data
	qty := math.Abs(float64(p.CurrentQty))

	if qty <= 0 {
		return domain.PositionInfo{
			Exchange:     c.Name(),
			Symbol:       kucoinUnifiedSymbol(native),
			NativeSymbol: native,
			UpdatedAt:    time.Now().UTC(),
		}, nil
	}

	side := domain.SideLong
	if float64(p.CurrentQty) < 0 {
		side = domain.SideShort
	}

	return domain.PositionInfo{
		Exchange:         c.Name(),
		Symbol:           kucoinUnifiedSymbol(native),
		NativeSymbol:     native,
		Side:             side,
		Qty:              qty,
		NotionalUSDT:     math.Abs(float64(p.CurrentQty) * float64(p.MarkPrice) * float64(p.Multiplier)),
		EntryPrice:       float64(p.AvgEntryPrice),
		MarkPrice:        float64(p.MarkPrice),
		UnrealizedPNL:    float64(p.UnrealisedPnl),
		RealizedPNL:      float64(p.RealisedPnl),
		LiquidationPrice: float64(p.LiquidationPrice),
		Leverage:         int(float64(p.RealLeverage)),
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
	native := normalizeKuCoinSymbol(symbol)

	q := url.Values{}
	q.Set("symbol", native)
	q.Set("type", "Funding")
	q.Set("maxCount", "100")

	if !from.IsZero() {
		q.Set("startAt", strconv.FormatInt(from.UTC().UnixMilli(), 10))
	}
	if !to.IsZero() {
		q.Set("endAt", strconv.FormatInt(to.UTC().UnixMilli(), 10))
	}

	var out kucoinResponse[struct {
		DataList []kucoinFundingFeeRow `json:"dataList"`
		HasMore  bool                  `json:"hasMore"`
	}]

	if err := c.signedGET("/api/v1/transaction-history", q, &out); err != nil {
		return nil, err
	}

	if out.Code != "" && out.Code != "200000" {
		return nil, fmt.Errorf("KuCoin funding history code=%s msg=%s", out.Code, out.Msg)
	}

	res := make([]domain.FundingFeeInfo, 0, len(out.Data.DataList))

	for _, r := range out.Data.DataList {
		res = append(res, domain.FundingFeeInfo{
			Exchange:     c.Name(),
			Symbol:       kucoinUnifiedSymbol(native),
			NativeSymbol: native,
			Amount:       float64(r.Amount),
			Asset:        firstNonEmpty(r.Currency, "USDT"),
			IncomeType:   firstNonEmpty(r.Type, "FUNDING_FEE"),
			FeeTime:      parseMillisOrSeconds(int64(r.Time)),
			Raw:          r.Remark,
		})
	}

	return res, nil
}

func (c *Client) markPrice(symbol string) float64 {
	native := normalizeKuCoinSymbol(symbol)

	var out kucoinResponse[kucoinContract]
	if err := c.getJSON("/api/v1/contracts/"+url.PathEscape(native), nil, &out); err != nil {
		return 0
	}

	if out.Code != "" && out.Code != "200000" {
		return 0
	}

	return firstNonZero(float64(out.Data.MarkPrice), float64(out.Data.IndexPrice), float64(out.Data.LastTradePrice))
}

type kucoinOrderData struct {
	ID            string        `json:"id"`
	OrderID       string        `json:"orderId"`
	ClientOid     string        `json:"clientOid"`
	Symbol        string        `json:"symbol"`
	Type          string        `json:"type"`
	Side          string        `json:"side"`
	Price         flexibleFloat `json:"price"`
	Size          flexibleFloat `json:"size"`
	DealSize      flexibleFloat `json:"dealSize"`
	DealValue     flexibleFloat `json:"dealValue"`
	RemainSize    flexibleFloat `json:"remainSize"`
	CancelledSize flexibleFloat `json:"cancelledSize"`
	Status        string        `json:"status"`
	IsActive      bool          `json:"isActive"`
	CancelExist   bool          `json:"cancelExist"`
	ReduceOnly    bool          `json:"reduceOnly"`
	Fee           flexibleFloat `json:"fee"`
	FeeCurrency   string        `json:"feeCurrency"`
	CreatedAt     flexibleInt   `json:"createdAt"`
	UpdatedAt     flexibleInt   `json:"updatedAt"`
}

func (o kucoinOrderData) toDomain(exchange domain.ExchangeName, fallbackSymbol string, contractSize float64) domain.OrderResult {
	native := firstNonEmpty(o.Symbol, fallbackSymbol)

	qty := float64(o.Size)
	filled := float64(o.DealSize)
	remaining := firstNonZero(float64(o.RemainSize), math.Max(0, qty-filled))

	status := o.Status
	if status == "" {
		if o.IsActive {
			status = "open"
		} else if o.CancelExist {
			status = "canceled"
		} else if filled >= qty && qty > 0 {
			status = "done"
		}
	}

	createdAt := time.Now().UTC()
	if o.CreatedAt > 0 {
		createdAt = parseMillisOrSeconds(int64(o.CreatedAt))
	}

	updatedAt := time.Now().UTC()
	if o.UpdatedAt > 0 {
		updatedAt = parseMillisOrSeconds(int64(o.UpdatedAt))
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: firstNonEmpty(o.OrderID, o.ID),
		ClientOrderID:   o.ClientOid,
		Symbol:          kucoinUnifiedSymbol(native),
		NativeSymbol:    native,
		Side:            strings.ToUpper(o.Side),
		Type:            strings.ToUpper(o.Type),
		Status:          status,
		OrderStatus:     mapKuCoinOrderStatus(status, qty, filled, o.IsActive, o.CancelExist),
		Price:           float64(o.Price),
		AvgPrice:        avgPrice(float64(o.DealValue), filled, contractSize),
		Qty:             qty,
		FilledQty:       filled,
		FilledNotional:  float64(o.DealValue),
		RemainingQty:    remaining,
		Fee:             float64(o.Fee),
		FeeAsset:        firstNonEmpty(o.FeeCurrency, "USDT"),
		ReduceOnly:      o.ReduceOnly,
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

type kucoinPositionData struct {
	Symbol                string        `json:"symbol"`
	CurrentQty            flexibleFloat `json:"currentQty"`
	RealLeverage          flexibleFloat `json:"realLeverage"`
	CrossMode             bool          `json:"crossMode"`
	DelevPercentage       flexibleFloat `json:"delevPercentage"`
	OpeningTimestamp      flexibleInt   `json:"openingTimestamp"`
	CurrentTimestamp      flexibleInt   `json:"currentTimestamp"`
	CurrentComm           flexibleFloat `json:"currentComm"`
	CurrentCost           flexibleFloat `json:"currentCost"`
	CurrentUnrealisedCost flexibleFloat `json:"currentUnrealisedCost"`
	RealisedCost          flexibleFloat `json:"realisedCost"`
	UnrealisedCost        flexibleFloat `json:"unrealisedCost"`
	PosCost               flexibleFloat `json:"posCost"`
	PosCross              flexibleFloat `json:"posCross"`
	PosInit               flexibleFloat `json:"posInit"`
	PosComm               flexibleFloat `json:"posComm"`
	MaintMarginReq        flexibleFloat `json:"maintMarginReq"`
	BankruptPrice         flexibleFloat `json:"bankruptPrice"`
	LiquidationPrice      flexibleFloat `json:"liquidationPrice"`
	AvgEntryPrice         flexibleFloat `json:"avgEntryPrice"`
	MarkPrice             flexibleFloat `json:"markPrice"`
	UnrealisedPnl         flexibleFloat `json:"unrealisedPnl"`
	RealisedPnl           flexibleFloat `json:"realisedPnl"`
	Multiplier            flexibleFloat `json:"multiplier"`
}

type kucoinFundingFeeRow struct {
	Time     flexibleInt   `json:"time"`
	Type     string        `json:"type"`
	Amount   flexibleFloat `json:"amount"`
	Fee      flexibleFloat `json:"fee"`
	Currency string        `json:"currency"`
	Remark   string        `json:"remark"`
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
		return fmt.Errorf("KuCoin GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("KuCoin GET %s decode error: %w; body=%s", path, err, string(body))
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
	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return fmt.Errorf("KuCoin API key, secret or passphrase is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	requestPath := path
	if len(q) > 0 {
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
	sign := c.sign(timestamp + method + requestPath + string(bodyBytes))
	passphrase := c.signPassphrase(c.passphrase)

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
	req.Header.Set("KC-API-KEY", c.apiKey)
	req.Header.Set("KC-API-SIGN", sign)
	req.Header.Set("KC-API-TIMESTAMP", timestamp)
	req.Header.Set("KC-API-PASSPHRASE", passphrase)
	req.Header.Set("KC-API-KEY-VERSION", "2")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("KuCoin signed %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("KuCoin signed %s decode error: %w; body=%s", path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func (c *Client) signPassphrase(passphrase string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(passphrase))

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type kucoinResponse[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data T      `json:"data"`
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

func normalizeKuCoinSymbol(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))

	if strings.HasSuffix(s, "M") && strings.Contains(s, "USDT") {
		return s
	}

	if strings.HasSuffix(s, "USDT") {
		base := strings.TrimSuffix(s, "USDT")
		if base == "BTC" {
			base = "XBT"
		}

		return base + "USDTM"
	}

	return s
}

func kucoinUnifiedSymbol(native string) string {
	s := strings.ToUpper(strings.TrimSpace(native))
	s = strings.TrimSuffix(s, "M")

	if strings.HasPrefix(s, "XBT") {
		s = "BTC" + strings.TrimPrefix(s, "XBT")
	}

	return s
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

func extractIntervalHours(v flexibleInt) float64 {
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

func kucoinClientOID(s string) string {
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

func kucoinTimeInForce(v string) string {
	v = strings.ToUpper(strings.TrimSpace(v))
	if v == "" {
		return "GTC"
	}

	switch v {
	case "GTC", "IOC":
		return v
	default:
		return "GTC"
	}
}

func avgPrice(dealValue float64, filledQty float64, contractSize float64) float64 {
	if dealValue <= 0 || filledQty <= 0 {
		return 0
	}

	denom := filledQty * firstNonZero(contractSize, 1)
	if denom <= 0 {
		return 0
	}

	return dealValue / denom
}

func mapKuCoinOrderStatus(status string, qty float64, filled float64, active bool, canceled bool) domain.OrderStatus {
	status = strings.ToLower(strings.TrimSpace(status))

	if canceled || status == "canceled" || status == "cancelled" {
		return domain.OrderStatusCanceled
	}

	if status == "done" || status == "filled" || (qty > 0 && filled >= qty) {
		return domain.OrderStatusFilled
	}

	if filled > 0 && filled < qty {
		return domain.OrderStatusPartiallyFill
	}

	if active || status == "open" || status == "active" {
		return domain.OrderStatusNew
	}

	if status == "rejected" {
		return domain.OrderStatusRejected
	}

	return domain.OrderStatusUnknown
}

func firstNonZeroInt(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}

	return 0
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

package okx

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
	"sync"
	"time"

	"funding-bot/internal/domain"
)

const (
	instTypeSwap = "SWAP"
	okxUSDT      = "USDT"
)

type Client struct {
	baseURL    string
	apiKey     string
	apiSecret  string
	passphrase string
	http       *http.Client
}

type okxTickerRow struct {
	InstID    string
	Last      string
	VolCcy24h string
	Vol24h    string
	AskPx     string
	BidPx     string
}

type okxFundingInfo struct {
	InstID        string
	FundingRate   float64
	NextFunding   time.Time
	IntervalHours float64
	OK            bool
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		apiSecret:  strings.TrimSpace(apiSecret),
		passphrase: strings.TrimSpace(os.Getenv("OKX_API_PASSPHRASE")),
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeOKX
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Ts flexibleInt `json:"ts"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v5/public/time", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.Code != "" && out.Code != "0" {
		return time.Time{}, fmt.Errorf("OKX server time code=%s msg=%s", out.Code, out.Msg)
	}

	if len(out.Data) > 0 && out.Data[0].Ts > 0 {
		return time.UnixMilli(int64(out.Data[0].Ts)).UTC(), nil
	}

	return time.Now().UTC(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now().UTC()

	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "OKX API key, secret or passphrase is empty",
			UpdatedAt: now,
		}, nil
	}

	q := url.Values{}
	q.Set("ccy", okxUSDT)

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			TotalEq flexibleFloat `json:"totalEq"`
			AdjEq   flexibleFloat `json:"adjEq"`
			Details []struct {
				Ccy       string        `json:"ccy"`
				Eq        flexibleFloat `json:"eq"`
				CashBal   flexibleFloat `json:"cashBal"`
				AvailBal  flexibleFloat `json:"availBal"`
				AvailEq   flexibleFloat `json:"availEq"`
				FrozenBal flexibleFloat `json:"frozenBal"`
				Upl       flexibleFloat `json:"upl"`
				EqUsd     flexibleFloat `json:"eqUsd"`
			} `json:"details"`
		} `json:"data"`
	}

	if err := c.signedGET("/api/v5/account/balance", q, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "OKX futures balance error: " + err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	if out.Code != "" && out.Code != "0" {
		err := fmt.Errorf("OKX balance code=%s msg=%s", out.Code, out.Msg)

		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     err.Error(),
			UpdatedAt: time.Now().UTC(),
		}, err
	}

	for _, account := range out.Data {
		wallet := float64(account.TotalEq)
		if wallet <= 0 {
			wallet = float64(account.AdjEq)
		}

		for _, d := range account.Details {
			if !strings.EqualFold(d.Ccy, okxUSDT) {
				continue
			}

			if wallet <= 0 {
				wallet = float64(d.Eq)
			}
			if wallet <= 0 {
				wallet = float64(d.EqUsd)
			}
			if wallet <= 0 {
				wallet = float64(d.CashBal)
			}

			available := firstNonZero(
				float64(d.AvailBal),
				float64(d.AvailEq),
				float64(d.CashBal),
				float64(d.Eq),
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

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "OKX USDT balance not found",
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	q := url.Values{}
	q.Set("instType", instTypeSwap)

	var tick struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID    string `json:"instId"`
			Last      string `json:"last"`
			VolCcy24h string `json:"volCcy24h"`
			Vol24h    string `json:"vol24h"`
			AskPx     string `json:"askPx"`
			BidPx     string `json:"bidPx"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v5/market/tickers", q, &tick); err != nil {
		return nil, err
	}

	if tick.Code != "" && tick.Code != "0" {
		return nil, fmt.Errorf("OKX tickers code=%s msg=%s", tick.Code, tick.Msg)
	}

	rows := make([]okxTickerRow, 0, len(tick.Data))

	for _, t := range tick.Data {
		if !strings.HasSuffix(t.InstID, "USDT-SWAP") {
			continue
		}

		rows = append(rows, okxTickerRow{
			InstID:    t.InstID,
			Last:      t.Last,
			VolCcy24h: t.VolCcy24h,
			Vol24h:    t.Vol24h,
			AskPx:     t.AskPx,
			BidPx:     t.BidPx,
		})
	}

	now := time.Now().UTC()
	fundingByInst := c.fetchFundingMap(rows, now)

	res := make([]domain.Candidate, 0, len(rows))

	for _, row := range rows {
		res = append(res, c.buildCandidate(row, now, fundingByInst))
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].FundingRate == res[j].FundingRate {
			return res[i].Volume24hUSDT > res[j].Volume24hUSDT
		}

		return res[i].FundingRate > res[j].FundingRate
	})

	return res, nil
}

func (c *Client) buildCandidate(row okxTickerRow, now time.Time, fundingByInst map[string]okxFundingInfo) domain.Candidate {
	price := parseFloat(row.Last)

	vol := parseFloat(row.VolCcy24h)
	if vol <= 0 {
		vol = parseFloat(row.Vol24h) * price
	}

	bid := parseFloat(row.BidPx)
	ask := parseFloat(row.AskPx)

	spread := 0.0
	if bid > 0 && ask > 0 {
		spread = (ask - bid) / ((ask + bid) / 2)
	}

	funding := fundingByInst[row.InstID]

	nextFunding := funding.NextFunding
	if nextFunding.IsZero() {
		nextFunding = nextFundingByInterval(now, 8)
	}

	intervalHours := funding.IntervalHours
	if intervalHours <= 0 {
		intervalHours = inferIntervalFromNextFunding(now, nextFunding)
	}
	if intervalHours <= 0 {
		intervalHours = 8
	}

	symbol := okxUnifiedSymbol(row.InstID)

	return domain.Candidate{
		Exchange:             c.Name(),
		Symbol:               symbol,
		NativeSymbol:         row.InstID,
		Price:                price,
		MarkPrice:            price,
		FundingRate:          funding.FundingRate,
		FundingIntervalHours: intervalHours,
		NextFundingTime:      nextFunding,
		Volume24hUSDT:        vol,
		Bid:                  bid,
		Ask:                  ask,
		Spread:               spread,
		UpdatedAt:            now,
	}
}

func (c *Client) fetchFundingMap(rows []okxTickerRow, now time.Time) map[string]okxFundingInfo {
	out := map[string]okxFundingInfo{}

	type result struct {
		instID string
		info   okxFundingInfo
		ok     bool
	}

	jobs := make(chan string)
	results := make(chan result)

	workerCount := 20

	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for instID := range jobs {
				info, ok := c.fetchFundingOne(instID, now)
				results <- result{
					instID: instID,
					info:   info,
					ok:     ok,
				}
			}
		}()
	}

	go func() {
		for _, row := range rows {
			jobs <- row.InstID
		}

		close(jobs)
		wg.Wait()
		close(results)
	}()

	for r := range results {
		if r.ok {
			out[r.instID] = r.info
		}
	}

	return out
}

func (c *Client) fetchFundingOne(instID string, now time.Time) (okxFundingInfo, bool) {
	q := url.Values{}
	q.Set("instId", instID)

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID          string        `json:"instId"`
			FundingRate     flexibleFloat `json:"fundingRate"`
			NextFundingTime flexibleInt   `json:"nextFundingTime"`
			FundingTime     flexibleInt   `json:"fundingTime"`
			FundingInterval flexibleFloat `json:"fundingInterval"`
			SettFundingRate flexibleFloat `json:"settFundingRate"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v5/public/funding-rate", q, &out); err != nil {
		return okxFundingInfo{}, false
	}

	if out.Code != "" && out.Code != "0" {
		return okxFundingInfo{}, false
	}

	if len(out.Data) == 0 {
		return okxFundingInfo{}, false
	}

	d := out.Data[0]

	rate := float64(d.FundingRate)
	if rate == 0 {
		rate = float64(d.SettFundingRate)
	}

	nextFunding := parseOKXFundingTime(int64(d.NextFundingTime))
	if nextFunding.IsZero() {
		nextFunding = parseOKXFundingTime(int64(d.FundingTime))
	}

	intervalHours := extractIntervalHours(d.FundingInterval)

	if intervalHours <= 0 {
		fundingTime := parseOKXFundingTime(int64(d.FundingTime))
		if !fundingTime.IsZero() && !nextFunding.IsZero() && nextFunding.After(fundingTime) {
			diff := nextFunding.Sub(fundingTime).Hours()
			if diff > 0 && diff <= 24 {
				intervalHours = diff
			}
		}
	}

	if intervalHours <= 0 {
		intervalHours = inferIntervalFromNextFunding(now, nextFunding)
	}

	if intervalHours <= 0 {
		intervalHours = 8
	}

	if nextFunding.IsZero() {
		nextFunding = nextFundingByInterval(now, int(intervalHours))
	}

	return okxFundingInfo{
		InstID:        instID,
		FundingRate:   rate,
		NextFunding:   nextFunding,
		IntervalHours: intervalHours,
		OK:            true,
	}, true
}

func (c *Client) SymbolRules(symbol string) (domain.SymbolRules, error) {
	instID := normalizeOKXInstID(symbol)

	q := url.Values{}
	q.Set("instType", instTypeSwap)
	q.Set("instId", instID)

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID    string        `json:"instId"`
			BaseCcy   string        `json:"baseCcy"`
			QuoteCcy  string        `json:"quoteCcy"`
			SettleCcy string        `json:"settleCcy"`
			CtVal     flexibleFloat `json:"ctVal"`
			MinSz     flexibleFloat `json:"minSz"`
			LotSz     flexibleFloat `json:"lotSz"`
			TickSz    flexibleFloat `json:"tickSz"`
			MaxMktSz  flexibleFloat `json:"maxMktSz"`
			MaxLmtSz  flexibleFloat `json:"maxLmtSz"`
			State     string        `json:"state"`
			Lever     flexibleFloat `json:"lever"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v5/public/instruments", q, &out); err != nil {
		return domain.SymbolRules{}, err
	}

	if out.Code != "" && out.Code != "0" {
		return domain.SymbolRules{}, fmt.Errorf("OKX instruments code=%s msg=%s", out.Code, out.Msg)
	}

	if len(out.Data) == 0 {
		return domain.SymbolRules{}, fmt.Errorf("OKX symbol rules not found: %s", instID)
	}

	item := out.Data[0]

	return domain.SymbolRules{
		Exchange:                    c.Name(),
		Symbol:                      okxUnifiedSymbol(item.InstID),
		NativeSymbol:                item.InstID,
		SupportsNotionalMarketOrder: false,
		MinQty:                      float64(item.MinSz),
		MaxQty:                      firstNonZero(float64(item.MaxMktSz), float64(item.MaxLmtSz)),
		QtyStep:                     firstNonZero(float64(item.LotSz), 1),
		MinNotional:                 0,
		PriceStep:                   float64(item.TickSz),
		TickSize:                    float64(item.TickSz),
		ContractSize:                firstNonZero(float64(item.CtVal), 1),
		BaseAsset:                   item.BaseCcy,
		QuoteAsset:                  item.QuoteCcy,
		MarginAsset:                 firstNonEmpty(item.SettleCcy, okxUSDT),
		UpdatedAt:                   time.Now().UTC(),
	}, nil
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	instID := normalizeOKXInstID(symbol)

	if leverage <= 0 {
		leverage = 1
	}

	marginMode = strings.ToLower(strings.TrimSpace(marginMode))
	if marginMode == "" {
		marginMode = "isolated"
	}

	body := map[string]string{
		"instId":  instID,
		"lever":   strconv.Itoa(leverage),
		"mgnMode": marginMode,
	}

	var out okxResponse[any]
	if err := c.signedPOST("/api/v5/account/set-leverage", body, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "0" && !isIgnorableOKXMsg(out.Msg) {
		return fmt.Errorf("OKX set leverage code=%s msg=%s", out.Code, out.Msg)
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

	return domain.OrderResult{}, fmt.Errorf("OKX unsupported order request: type=%s side=%s reduce_only=%v", req.Type, req.Side, req.ReduceOnly)
}

func (c *Client) PlaceMarketShort(req domain.OrderRequest) (domain.OrderResult, error) {
	instID := normalizeOKXInstID(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(instID)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		price = c.markPrice(instID)
	}

	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("OKX %s mark price is zero", instID)
	}

	qty := req.Qty
	if qty <= 0 {
		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		qty = notional / price / firstNonZero(rules.ContractSize, 1)
	}

	qty = floorToStep(qty, rules.QtyStep)
	if qty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("OKX %s calculated qty is zero", instID)
	}

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("OKX %s qty %.12f < min qty %.12f", instID, qty, rules.MinQty)
	}

	clientOID := okxClientOID(req.ClientOrderID)

	body := map[string]string{
		"instId":  instID,
		"tdMode":  strings.ToLower(firstNonEmpty(req.MarginMode, "isolated")),
		"side":    "sell",
		"ordType": "market",
		"sz":      formatFloat(qty),
		"clOrdId": clientOID,
	}

	addOKXPosSide(body, "short")

	var out okxResponse[[]okxOrderData]
	if err := c.signedPOST("/api/v5/trade/order", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "0" {
		return domain.OrderResult{}, fmt.Errorf("OKX market short code=%s msg=%s", out.Code, firstNonEmpty(out.Msg, firstDataMsg(out.Data)))
	}

	res := okxFirstOrder(out.Data).toDomain(c.Name(), instID)

	if res.ExchangeOrderID == "" && res.ClientOrderID == "" {
		res.ClientOrderID = clientOID
	}

	if res.ExchangeOrderID != "" || res.ClientOrderID != "" {
		if fresh, err := c.GetOrderStatus(instID, firstNonEmpty(res.ExchangeOrderID, res.ClientOrderID)); err == nil {
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
	instID := normalizeOKXInstID(firstNonEmpty(req.NativeSymbol, req.Symbol))

	rules, err := c.SymbolRules(instID)
	if err != nil {
		return domain.OrderResult{}, err
	}

	price := req.Price
	if price <= 0 {
		return domain.OrderResult{}, fmt.Errorf("OKX %s limit price is zero", instID)
	}

	price = floorToStep(price, rules.TickSize)

	qty := req.Qty
	if qty <= 0 {
		ref := c.markPrice(instID)
		if ref <= 0 {
			ref = price
		}

		notional := firstNonZero(req.NotionalUSDT, req.MarginUSDT)
		qty = notional / ref / firstNonZero(rules.ContractSize, 1)
	}

	qty = floorToStep(qty, rules.QtyStep)
	if qty <= 0 {
		return domain.OrderResult{}, fmt.Errorf("OKX %s calculated limit qty is zero", instID)
	}

	if rules.MinQty > 0 && qty < rules.MinQty {
		return domain.OrderResult{}, fmt.Errorf("OKX %s qty %.12f < min qty %.12f", instID, qty, rules.MinQty)
	}

	side := strings.ToLower(req.Side)
	if side == "" {
		return domain.OrderResult{}, fmt.Errorf("OKX limit side is empty")
	}

	clientOID := okxClientOID(req.ClientOrderID)

	body := map[string]string{
		"instId":  instID,
		"tdMode":  strings.ToLower(firstNonEmpty(req.MarginMode, "isolated")),
		"side":    side,
		"ordType": "limit",
		"px":      formatFloat(price),
		"sz":      formatFloat(qty),
		"clOrdId": clientOID,
	}

	if req.ReduceOnly {
		body["reduceOnly"] = "true"
	}

	if strings.EqualFold(side, "buy") {
		addOKXPosSide(body, "short")
	} else {
		addOKXPosSide(body, "long")
	}

	var out okxResponse[[]okxOrderData]
	if err := c.signedPOST("/api/v5/trade/order", body, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "0" {
		return domain.OrderResult{}, fmt.Errorf("OKX limit order code=%s msg=%s", out.Code, firstNonEmpty(out.Msg, firstDataMsg(out.Data)))
	}

	res := okxFirstOrder(out.Data).toDomain(c.Name(), instID)

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
	instID := normalizeOKXInstID(symbol)

	q := url.Values{}
	q.Set("instId", instID)

	if isNumeric(orderID) {
		q.Set("ordId", orderID)
	} else {
		q.Set("clOrdId", okxClientOID(orderID))
	}

	var out okxResponse[[]okxOrderData]
	if err := c.signedGET("/api/v5/trade/order", q, &out); err != nil {
		return domain.OrderResult{}, err
	}

	if out.Code != "" && out.Code != "0" {
		return domain.OrderResult{}, fmt.Errorf("OKX order status code=%s msg=%s", out.Code, out.Msg)
	}

	if len(out.Data) == 0 {
		return domain.OrderResult{}, fmt.Errorf("OKX order status empty: %s %s", instID, orderID)
	}

	return out.Data[0].toDomain(c.Name(), instID), nil
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	instID := normalizeOKXInstID(symbol)

	body := map[string]string{
		"instId": instID,
	}

	if isNumeric(orderID) {
		body["ordId"] = orderID
	} else {
		body["clOrdId"] = okxClientOID(orderID)
	}

	var out okxResponse[[]okxOrderData]
	if err := c.signedPOST("/api/v5/trade/cancel-order", body, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "0" && !isIgnorableCancel(out.Msg) {
		return fmt.Errorf("OKX cancel order code=%s msg=%s", out.Code, firstNonEmpty(out.Msg, firstDataMsg(out.Data)))
	}

	return nil
}

func (c *Client) CancelAll(symbol string) error {
	instID := normalizeOKXInstID(symbol)

	q := url.Values{}
	q.Set("instType", instTypeSwap)
	q.Set("instId", instID)

	var pending okxResponse[[]okxOrderData]
	if err := c.signedGET("/api/v5/trade/orders-pending", q, &pending); err != nil {
		return err
	}

	if pending.Code != "" && pending.Code != "0" {
		return fmt.Errorf("OKX pending orders code=%s msg=%s", pending.Code, pending.Msg)
	}

	for _, order := range pending.Data {
		id := firstNonEmpty(order.OrdID, order.ClOrdID)
		if id == "" {
			continue
		}

		_ = c.CancelOrder(instID, id)
	}

	return nil
}

func (c *Client) ClosePositionMarket(symbol string) error {
	instID := normalizeOKXInstID(symbol)

	pos, err := c.GetPosition(instID)
	if err != nil {
		return err
	}

	if pos.Qty <= 0 {
		return nil
	}

	side := "buy"
	posSide := "short"

	if pos.Side == domain.SideLong {
		side = "sell"
		posSide = "long"
	}

	body := map[string]string{
		"instId":     instID,
		"tdMode":     strings.ToLower(firstNonEmpty(pos.MarginMode, "isolated")),
		"side":       side,
		"ordType":    "market",
		"sz":         formatFloat(pos.Qty),
		"reduceOnly": "true",
		"clOrdId":    okxClientOID("close-" + okxUnifiedSymbol(instID) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)),
	}

	addOKXPosSide(body, posSide)

	var out okxResponse[[]okxOrderData]
	if err := c.signedPOST("/api/v5/trade/order", body, &out); err != nil {
		return err
	}

	if out.Code != "" && out.Code != "0" {
		return fmt.Errorf("OKX close market code=%s msg=%s", out.Code, firstNonEmpty(out.Msg, firstDataMsg(out.Data)))
	}

	return nil
}

func (c *Client) GetPosition(symbol string) (domain.PositionInfo, error) {
	instID := normalizeOKXInstID(symbol)

	q := url.Values{}
	q.Set("instType", instTypeSwap)
	q.Set("instId", instID)

	var out okxResponse[[]okxPositionData]
	if err := c.signedGET("/api/v5/account/positions", q, &out); err != nil {
		return domain.PositionInfo{}, err
	}

	if out.Code != "" && out.Code != "0" {
		return domain.PositionInfo{}, fmt.Errorf("OKX positions code=%s msg=%s", out.Code, out.Msg)
	}

	for _, p := range out.Data {
		qty := math.Abs(float64(p.Pos))
		if qty <= 0 {
			continue
		}

		side := domain.SideShort
		if strings.EqualFold(p.PosSide, "long") || float64(p.Pos) > 0 {
			side = domain.SideLong
		}

		return domain.PositionInfo{
			Exchange:         c.Name(),
			Symbol:           okxUnifiedSymbol(instID),
			NativeSymbol:     instID,
			Side:             side,
			Qty:              qty,
			NotionalUSDT:     math.Abs(float64(p.NotionalUsd)),
			EntryPrice:       float64(p.AvgPx),
			MarkPrice:        float64(p.MarkPx),
			UnrealizedPNL:    float64(p.Upl),
			RealizedPNL:      float64(p.RealizedPnl),
			LiquidationPrice: float64(p.LiqPx),
			Leverage:         int(float64(p.Lever)),
			MarginMode:       strings.ToLower(p.MgnMode),
			UpdatedAt:        time.Now().UTC(),
		}, nil
	}

	return domain.PositionInfo{
		Exchange:     c.Name(),
		Symbol:       okxUnifiedSymbol(instID),
		NativeSymbol: instID,
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
	instID := normalizeOKXInstID(symbol)

	q := url.Values{}
	q.Set("instType", instTypeSwap)
	q.Set("instId", instID)
	q.Set("type", "8")
	q.Set("limit", "100")

	if !from.IsZero() {
		q.Set("begin", strconv.FormatInt(from.UTC().UnixMilli(), 10))
	}
	if !to.IsZero() {
		q.Set("end", strconv.FormatInt(to.UTC().UnixMilli(), 10))
	}

	var out okxResponse[[]struct {
		InstID  string        `json:"instId"`
		BillID  string        `json:"billId"`
		Type    string        `json:"type"`
		SubType string        `json:"subType"`
		Pnl     flexibleFloat `json:"pnl"`
		BalChg  flexibleFloat `json:"balChg"`
		Ccy     string        `json:"ccy"`
		Ts      flexibleInt   `json:"ts"`
	}]

	if err := c.signedGET("/api/v5/account/bills", q, &out); err != nil {
		return nil, err
	}

	if out.Code != "" && out.Code != "0" {
		return nil, fmt.Errorf("OKX funding fee bills code=%s msg=%s", out.Code, out.Msg)
	}

	res := make([]domain.FundingFeeInfo, 0, len(out.Data))

	for _, b := range out.Data {
		if b.InstID != "" && b.InstID != instID {
			continue
		}

		amount := firstNonZero(float64(b.Pnl), float64(b.BalChg))

		res = append(res, domain.FundingFeeInfo{
			Exchange:     c.Name(),
			Symbol:       okxUnifiedSymbol(instID),
			NativeSymbol: instID,
			Amount:       amount,
			Asset:        firstNonEmpty(b.Ccy, okxUSDT),
			IncomeType:   firstNonEmpty(b.Type, "FUNDING_FEE"),
			FeeTime:      parseOKXFundingTime(int64(b.Ts)),
			Raw:          b.BillID,
		})
	}

	return res, nil
}

func (c *Client) markPrice(symbol string) float64 {
	instID := normalizeOKXInstID(symbol)

	q := url.Values{}
	q.Set("instId", instID)

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID  string        `json:"instId"`
			MarkPx  flexibleFloat `json:"markPx"`
			IndexPx flexibleFloat `json:"indexPx"`
			Ts      flexibleInt   `json:"ts"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v5/public/mark-price", q, &out); err != nil {
		return 0
	}

	if out.Code != "" && out.Code != "0" {
		return 0
	}

	if len(out.Data) == 0 {
		return 0
	}

	return firstNonZero(float64(out.Data[0].MarkPx), float64(out.Data[0].IndexPx))
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
		return fmt.Errorf("OKX GET %s: status=%d body=%s", path, resp.StatusCode, string(body))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("OKX GET %s decode error: %w; body=%s", path, err, string(body))
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
	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return fmt.Errorf("OKX API key, secret or passphrase is empty")
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

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	payload := timestamp + method + requestPath + string(bodyBytes)
	signature := c.sign(payload)

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
	req.Header.Set("OK-ACCESS-KEY", c.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", signature)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", c.passphrase)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("OKX signed %s %s: status=%d body=%s", method, path, resp.StatusCode, string(raw))
	}

	if dst == nil {
		return nil
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("OKX signed %s %s decode error: %w; body=%s", method, path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

type okxResponse[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data T      `json:"data"`
}

type okxOrderData struct {
	InstID     string        `json:"instId"`
	OrdID      string        `json:"ordId"`
	ClOrdID    string        `json:"clOrdId"`
	Tag        string        `json:"tag"`
	Px         flexibleFloat `json:"px"`
	AvgPx      flexibleFloat `json:"avgPx"`
	Sz         flexibleFloat `json:"sz"`
	AccFillSz  flexibleFloat `json:"accFillSz"`
	FillSz     flexibleFloat `json:"fillSz"`
	FillPx     flexibleFloat `json:"fillPx"`
	Side       string        `json:"side"`
	PosSide    string        `json:"posSide"`
	OrdType    string        `json:"ordType"`
	State      string        `json:"state"`
	ReduceOnly string        `json:"reduceOnly"`
	Fee        flexibleFloat `json:"fee"`
	FeeCcy     string        `json:"feeCcy"`
	CTime      flexibleInt   `json:"cTime"`
	UTime      flexibleInt   `json:"uTime"`
	SCode      string        `json:"sCode"`
	SMsg       string        `json:"sMsg"`
}

func (o okxOrderData) toDomain(exchange domain.ExchangeName, fallbackInstID string) domain.OrderResult {
	instID := firstNonEmpty(o.InstID, fallbackInstID)
	qty := float64(o.Sz)
	filled := firstNonZero(float64(o.AccFillSz), float64(o.FillSz))

	createdAt := time.Now().UTC()
	if o.CTime > 0 {
		createdAt = parseOKXFundingTime(int64(o.CTime))
	}

	updatedAt := time.Now().UTC()
	if o.UTime > 0 {
		updatedAt = parseOKXFundingTime(int64(o.UTime))
	}

	return domain.OrderResult{
		Exchange:        exchange,
		ExchangeOrderID: o.OrdID,
		ClientOrderID:   o.ClOrdID,
		Symbol:          okxUnifiedSymbol(instID),
		NativeSymbol:    instID,
		Side:            strings.ToUpper(o.Side),
		PositionSide:    strings.ToUpper(o.PosSide),
		Type:            strings.ToUpper(o.OrdType),
		Status:          firstNonEmpty(o.State, o.SCode),
		OrderStatus:     mapOKXOrderStatus(firstNonEmpty(o.State, o.SCode)),
		Price:           float64(o.Px),
		AvgPrice:        firstNonZero(float64(o.AvgPx), float64(o.FillPx)),
		Qty:             qty,
		FilledQty:       filled,
		FilledNotional:  0,
		RemainingQty:    math.Max(0, qty-filled),
		Fee:             float64(o.Fee),
		FeeAsset:        firstNonEmpty(o.FeeCcy, okxUSDT),
		ReduceOnly:      strings.EqualFold(o.ReduceOnly, "true"),
		CreatedAt:       createdAt,
		UpdatedAt:       updatedAt,
	}
}

type okxPositionData struct {
	InstID      string        `json:"instId"`
	PosSide     string        `json:"posSide"`
	Pos         flexibleFloat `json:"pos"`
	AvailPos    flexibleFloat `json:"availPos"`
	AvgPx       flexibleFloat `json:"avgPx"`
	MarkPx      flexibleFloat `json:"markPx"`
	Upl         flexibleFloat `json:"upl"`
	RealizedPnl flexibleFloat `json:"realizedPnl"`
	NotionalUsd flexibleFloat `json:"notionalUsd"`
	LiqPx       flexibleFloat `json:"liqPx"`
	Lever       flexibleFloat `json:"lever"`
	MgnMode     string        `json:"mgnMode"`
	UTime       flexibleInt   `json:"uTime"`
}

func okxFirstOrder(items []okxOrderData) okxOrderData {
	if len(items) == 0 {
		return okxOrderData{}
	}

	return items[0]
}

func firstDataMsg(items []okxOrderData) string {
	for _, item := range items {
		if item.SMsg != "" {
			return item.SMsg
		}
	}

	return ""
}

func mapOKXOrderStatus(s string) domain.OrderStatus {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "live", "effective", "0":
		return domain.OrderStatusNew
	case "partially_filled", "partially-filled":
		return domain.OrderStatusPartiallyFill
	case "filled":
		return domain.OrderStatusFilled
	case "canceled", "cancelled":
		return domain.OrderStatusCanceled
	case "rejected", "-1", "51000", "51008":
		return domain.OrderStatusRejected
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

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func parseOKXFundingTime(v int64) time.Time {
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

func normalizeOKXInstID(symbol string) string {
	s := strings.ToUpper(strings.TrimSpace(symbol))

	if strings.Contains(s, "-") && strings.HasSuffix(s, "-SWAP") {
		return s
	}

	s = strings.TrimSuffix(s, "SWAP")

	if strings.HasSuffix(s, "USDT") {
		base := strings.TrimSuffix(s, "USDT")
		return base + "-USDT-SWAP"
	}

	return s
}

func okxUnifiedSymbol(instID string) string {
	s := strings.ToUpper(strings.TrimSpace(instID))
	s = strings.TrimSuffix(s, "-SWAP")
	s = strings.ReplaceAll(s, "-", "")
	return s
}

func addOKXPosSide(body map[string]string, posSide string) {
	posSide = strings.ToLower(strings.TrimSpace(posSide))
	if posSide == "" {
		return
	}

	body["posSide"] = posSide
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

func okxClientOID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "fg-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	}

	replacer := strings.NewReplacer(
		":", "",
		"/", "",
		" ", "",
		"_", "",
		"-", "",
	)

	s = replacer.Replace(s)

	if len(s) > 32 {
		s = s[len(s)-32:]
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

func isIgnorableOKXMsg(msg string) bool {
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

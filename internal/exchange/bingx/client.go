package bingx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
		return time.Time{}, fmt.Errorf("bingx server time code=%d msg=%s", out.Code, out.Msg)
	}

	if out.Data.ServerTime > 0 {
		return time.UnixMilli(int64(out.Data.ServerTime)), nil
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

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

	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
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
		} `json:"data"`
	}

	if err := c.signedGET("/openApi/swap/v3/user/balance", params, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "BingX futures balance error: " + err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	if out.Code != 0 {
		err := fmt.Errorf("BingX balance code=%d msg=%s", out.Code, out.Msg)

		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	for _, b := range out.Data {
		if strings.EqualFold(b.Asset, "USDT") {
			wallet := float64(b.Balance)
			if wallet <= 0 {
				wallet = float64(b.Equity)
			}

			available := float64(b.AvailableMargin)

			return domain.Balance{
				Exchange:      c.Name(),
				WalletUSDT:    wallet,
				AvailableUSDT: available,
				PrivateOK:     true,
				Error:         "",
				UpdatedAt:     time.Now(),
			}, nil
		}
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "BingX USDT futures balance not found",
		UpdatedAt: time.Now(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	var tick struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol      string `json:"symbol"`
			LastPrice   string `json:"lastPrice"`
			QuoteVolume string `json:"quoteVolume"`
			Volume      string `json:"volume"`
			AskPrice    string `json:"askPrice"`
			BidPrice    string `json:"bidPrice"`
		} `json:"data"`
	}

	if err := c.getJSON("/openApi/swap/v2/quote/ticker", nil, &tick); err != nil {
		return nil, err
	}

	if tick.Code != 0 {
		return nil, fmt.Errorf("bingx ticker code=%d msg=%s", tick.Code, tick.Msg)
	}

	rows := make([]bingXTickerRow, 0, len(tick.Data))

	for _, t := range tick.Data {
		if !strings.Contains(t.Symbol, "USDT") {
			continue
		}

		vol, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		if vol <= 0 {
			vol, _ = strconv.ParseFloat(t.Volume, 64)
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

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].VolumeUSDT > rows[j].VolumeUSDT
	})

	// BingX отдаёт funding одним bulk-запросом.
	// Но в dashboard пока выводим 250 самых ликвидных инструментов,
	// чтобы таблица и скан работали стабильно.
	if len(rows) > 250 {
		rows = rows[:250]
	}

	now := time.Now().UTC()
	premiumBySymbol := c.fetchPremiumMap(now)

	fmt.Printf("BingX premium map loaded: %d, rows: %d\n", len(premiumBySymbol), len(rows))

	res := make([]domain.Candidate, 0, len(rows))

	for _, row := range rows {
		res = append(res, c.buildCandidate(row, now, premiumBySymbol))
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].FundingRate == res[j].FundingRate {
			return res[i].Volume24hUSDT > res[j].Volume24hUSDT
		}

		return res[i].FundingRate > res[j].FundingRate
	})

	return res, nil
}

func (c *Client) buildCandidate(row bingXTickerRow, now time.Time, premiumBySymbol map[string]bingXPremiumInfo) domain.Candidate {
	price, _ := strconv.ParseFloat(row.LastPrice, 64)

	vol := row.VolumeUSDT
	if vol <= 0 {
		vol, _ = strconv.ParseFloat(row.QuoteVolume, 64)
	}
	if vol <= 0 {
		vol, _ = strconv.ParseFloat(row.Volume, 64)
	}

	bid, _ := strconv.ParseFloat(row.BidPrice, 64)
	ask, _ := strconv.ParseFloat(row.AskPrice, 64)

	spread := 0.0
	if bid > 0 && ask > 0 {
		spread = (ask - bid) / ((ask + bid) / 2)
	}

	premium, ok := premiumBySymbol[row.Symbol]
	if !ok {
		premium = bingXPremiumInfo{
			Symbol:        row.Symbol,
			FundingRate:   0,
			MarkPrice:     price,
			IndexPrice:    price,
			NextFunding:   nextFundingByInterval(now, 8),
			IntervalHours: 8,
			OK:            false,
		}
	}

	mark := premium.MarkPrice
	if mark <= 0 {
		mark = premium.IndexPrice
	}
	if mark <= 0 {
		mark = price
	}

	nextFunding := premium.NextFunding
	if nextFunding.IsZero() {
		nextFunding = nextFundingByInterval(now, 8)
	}

	intervalHours := premium.IntervalHours
	if intervalHours <= 0 {
		intervalHours = inferIntervalFromNextFunding(now, nextFunding)
	}
	if intervalHours <= 0 {
		intervalHours = 8
	}

	return domain.Candidate{
		Exchange:             c.Name(),
		Symbol:               strings.ReplaceAll(row.Symbol, "-", ""),
		NativeSymbol:         row.Symbol,
		Price:                price,
		MarkPrice:            mark,
		FundingRate:          premium.FundingRate,
		FundingIntervalHours: intervalHours,
		NextFundingTime:      nextFunding,
		Volume24hUSDT:        vol,
		Bid:                  bid,
		Ask:                  ask,
		Spread:               spread,
		UpdatedAt:            now,
	}
}

func (c *Client) fetchPremiumMap(now time.Time) map[string]bingXPremiumInfo {
	out := map[string]bingXPremiumInfo{}

	var resp struct {
		Code int              `json:"code"`
		Msg  string           `json:"msg"`
		Data []premiumPayload `json:"data"`
	}

	if err := c.getJSON("/openApi/swap/v2/quote/premiumIndex", nil, &resp); err != nil {
		fmt.Printf("BingX premium bulk error: %v\n", err)
		return out
	}

	if resp.Code != 0 {
		fmt.Printf("BingX premium bulk code=%d msg=%s\n", resp.Code, resp.Msg)
		return out
	}

	for _, payload := range resp.Data {
		info := parsePremiumPayload(payload, now)
		if info.Symbol == "" {
			continue
		}

		out[info.Symbol] = info
	}

	return out
}

func parsePremiumPayload(p premiumPayload, now time.Time) bingXPremiumInfo {
	fr, _ := strconv.ParseFloat(firstNonEmpty(p.LastFundingRate, p.FundingRate), 64)

	mark, _ := strconv.ParseFloat(p.MarkPrice, 64)
	index, _ := strconv.ParseFloat(p.IndexPrice, 64)

	nextRaw := int64(p.NextFundingTime)
	if nextRaw <= 0 {
		nextRaw = int64(p.FundingTime)
	}

	next := parseBingXFundingTime(now, nextRaw)

	interval := extractIntervalHours(
		p.FundingIntervalHours,
		p.FundingInterval,
		p.FundingIntervalHour,
		p.Interval,
	)

	if interval <= 0 {
		interval = inferIntervalFromNextFunding(now, next)
	}

	if interval <= 0 {
		interval = 8
	}

	return bingXPremiumInfo{
		Symbol:        p.Symbol,
		FundingRate:   fr,
		MarkPrice:     mark,
		IndexPrice:    index,
		NextFunding:   next,
		IntervalHours: interval,
		OK:            true,
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

func parseBingXFundingTime(now time.Time, v int64) time.Time {
	if v <= 0 {
		return nextFundingByInterval(now.UTC(), 8)
	}

	// Unix milliseconds.
	if v > 1_000_000_000_000 {
		return time.UnixMilli(v).UTC()
	}

	// Remaining time in milliseconds.
	if v < 7*24*60*60*1000 {
		return now.UTC().Add(time.Duration(v) * time.Millisecond)
	}

	return nextFundingByInterval(now.UTC(), 8)
}

func extractIntervalHours(values ...flexibleFloat) float64 {
	for _, raw := range values {
		v := float64(raw)
		if v <= 0 {
			continue
		}

		// Milliseconds.
		if v >= 3600000 && int64(v)%3600000 == 0 {
			return v / 3600000
		}

		// Seconds.
		if v >= 3600 && int64(v)%3600 == 0 {
			return v / 3600
		}

		// Hours.
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

	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	step := time.Duration(hours) * time.Hour

	for t := base; ; t = t.Add(step) {
		if t.After(now) {
			return t
		}
	}
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}

func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	return domain.OrderResult{}, fmt.Errorf("bingx live orders not implemented")
}

func (c *Client) CancelOrder(symbol, orderID string) error {
	return nil
}

func (c *Client) CancelAll(symbol string) error {
	return nil
}

func (c *Client) ClosePositionMarket(symbol string) error {
	return nil
}

func (c *Client) getJSON(path string, q url.Values, dst any) error {
	body, err := c.getRaw(path, q)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("bingx GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) getRaw(path string, q url.Values) ([]byte, error) {
	u := c.baseURL + path
	if q != nil && len(q) > 0 {
		u += "?" + q.Encode()
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("X-SOURCE-KEY", "BX-AI-SKILL")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("bingx GET %s: %s", path, string(body))
	}

	return body, nil
}

func (c *Client) signedGET(path string, params url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("bingx api key or secret is empty")
	}

	if params == nil {
		params = url.Values{}
	}

	if params.Get("timestamp") == "" {
		params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	}

	if params.Get("recvWindow") == "" {
		params.Set("recvWindow", "5000")
	}

	rawQuery := encodeSorted(params)
	signature := c.sign(rawQuery)

	u := c.baseURL + path + "?" + rawQuery + "&signature=" + signature

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("X-BX-APIKEY", c.apiKey)
	req.Header.Set("X-SOURCE-KEY", "BX-AI-SKILL")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("bingx signed GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("bingx signed GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func encodeSorted(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	parts := make([]string, 0, len(keys))

	for _, k := range keys {
		values := v[k]
		sort.Strings(values)

		for _, value := range values {
			parts = append(parts, k+"="+value)
		}
	}

	return strings.Join(parts, "&")
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

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

	v, err := strconv.ParseFloat(s, 64)
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

	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		*i = 0
		return nil
	}

	*i = flexibleInt(v)
	return nil
}

package mexc

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
		return time.Time{}, fmt.Errorf("mexc ping code=%d msg=%s", out.Code, out.Message)
	}

	if out.Data > 0 {
		return time.UnixMilli(int64(out.Data)), nil
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

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
			UpdatedAt: time.Now(),
		}, err
	}

	if singleErr != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "MEXC futures balance error: " + singleErr.Error(),
			UpdatedAt: time.Now(),
		}, singleErr
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "MEXC USDT futures balance not found",
		UpdatedAt: time.Now(),
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

		wallet := float64(a.Equity)
		if wallet <= 0 {
			wallet = float64(a.WalletBalance)
		}
		if wallet <= 0 {
			wallet = float64(a.Balance)
		}
		if wallet <= 0 {
			wallet = float64(a.CashBalance)
		}

		available := float64(a.AvailableBalance)
		if available <= 0 {
			available = float64(a.Available)
		}
		if available <= 0 {
			available = float64(a.CashBalance)
		}

		return domain.Balance{
			Exchange:      c.Name(),
			WalletUSDT:    wallet,
			AvailableUSDT: available,
			PrivateOK:     true,
			Error:         "",
			UpdatedAt:     time.Now(),
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

	wallet := float64(a.Equity)
	if wallet <= 0 {
		wallet = float64(a.WalletBalance)
	}
	if wallet <= 0 {
		wallet = float64(a.Balance)
	}
	if wallet <= 0 {
		wallet = float64(a.CashBalance)
	}

	available := float64(a.AvailableBalance)
	if available <= 0 {
		available = float64(a.Available)
	}
	if available <= 0 {
		available = float64(a.CashBalance)
	}

	return domain.Balance{
		Exchange:      c.Name(),
		WalletUSDT:    wallet,
		AvailableUSDT: available,
		PrivateOK:     true,
		Error:         "",
		UpdatedAt:     time.Now(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	fundingBySymbol := c.fundingInfoMap()

	var out struct {
		Success bool   `json:"success"`
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    []struct {
			Symbol      string        `json:"symbol"`
			LastPrice   flexibleFloat `json:"lastPrice"`
			FairPrice   flexibleFloat `json:"fairPrice"`
			IndexPrice  flexibleFloat `json:"indexPrice"`
			FundingRate flexibleFloat `json:"fundingRate"`
			Volume24    flexibleFloat `json:"volume24"`
			Amount24    flexibleFloat `json:"amount24"`
			HoldVol     flexibleFloat `json:"holdVol"`
			Bid1        flexibleFloat `json:"bid1"`
			Ask1        flexibleFloat `json:"ask1"`
			Timestamp   flexibleInt   `json:"timestamp"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/ticker", nil, &out); err != nil {
		return nil, err
	}

	if !out.Success && out.Code != 0 {
		return nil, fmt.Errorf("mexc ticker response code=%d msg=%s", out.Code, out.Message)
	}

	now := time.Now().UTC()
	res := make([]domain.Candidate, 0, len(out.Data))

	for _, t := range out.Data {
		if t.Symbol == "" || !strings.Contains(t.Symbol, "USDT") {
			continue
		}

		price := float64(t.LastPrice)

		mark := float64(t.FairPrice)
		if mark <= 0 {
			mark = float64(t.IndexPrice)
		}
		if mark <= 0 {
			mark = price
		}

		bid := float64(t.Bid1)
		ask := float64(t.Ask1)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		vol := float64(t.Amount24)
		if vol <= 0 {
			vol = float64(t.Volume24) * price
		}
		if vol <= 0 {
			vol = float64(t.Volume24)
		}

		info := fundingBySymbol[t.Symbol]

		fundingRate := float64(t.FundingRate)
		if fundingRate == 0 && info.OK {
			fundingRate = info.FundingRate
		}

		intervalHours := info.IntervalHours
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := info.NextFunding
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		symbol := strings.ReplaceAll(t.Symbol, "_", "")

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         t.Symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          fundingRate,
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        vol,
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

	var resp struct {
		Success bool            `json:"success"`
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/funding_rate", nil, &resp); err != nil {
		fmt.Printf("MEXC funding bulk error: %v\n", err)
		return out
	}

	if !resp.Success && resp.Code != 0 {
		fmt.Printf("MEXC funding bulk code=%d msg=%s\n", resp.Code, resp.Message)
		return out
	}

	var many []fundingPayload
	if err := json.Unmarshal(resp.Data, &many); err == nil && len(many) > 0 {
		for _, p := range many {
			info := parseFundingPayload(p)
			if info.Symbol != "" {
				out[info.Symbol] = info
			}
		}

		fmt.Printf("MEXC funding info loaded: %d\n", len(out))
		return out
	}

	var one fundingPayload
	if err := json.Unmarshal(resp.Data, &one); err == nil {
		info := parseFundingPayload(one)
		if info.Symbol != "" {
			out[info.Symbol] = info
		}
	}

	fmt.Printf("MEXC funding info loaded: %d\n", len(out))

	return out
}

type fundingPayload struct {
	Symbol          string        `json:"symbol"`
	FundingRate     flexibleFloat `json:"fundingRate"`
	FundingRateAlt  flexibleFloat `json:"funding_rate"`
	SettleTime      flexibleInt   `json:"settleTime"`
	NextSettleTime  flexibleInt   `json:"nextSettleTime"`
	NextFundingTime flexibleInt   `json:"nextFundingTime"`
	CollectCycle    flexibleInt   `json:"collectCycle"`
	FundingInterval flexibleInt   `json:"fundingInterval"`
	Interval        flexibleInt   `json:"interval"`
}

func parseFundingPayload(p fundingPayload) mexcFundingInfo {
	rate := float64(p.FundingRate)
	if rate == 0 {
		rate = float64(p.FundingRateAlt)
	}

	nextRaw := int64(p.NextSettleTime)
	if nextRaw <= 0 {
		nextRaw = int64(p.SettleTime)
	}
	if nextRaw <= 0 {
		nextRaw = int64(p.NextFundingTime)
	}

	nextFunding := parseMEXCFundingTime(nextRaw)

	interval := extractIntervalHours(
		p.CollectCycle,
		p.FundingInterval,
		p.Interval,
	)

	if interval <= 0 {
		interval = 8
	}

	return mexcFundingInfo{
		Symbol:        p.Symbol,
		FundingRate:   rate,
		NextFunding:   nextFunding,
		IntervalHours: interval,
		OK:            true,
	}
}

func parseMEXCFundingTime(v int64) time.Time {
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

func extractIntervalHours(values ...flexibleInt) float64 {
	for _, raw := range values {
		v := int64(raw)
		if v <= 0 {
			continue
		}

		if v >= 3600000 && v%3600000 == 0 {
			return float64(v) / 3600000
		}

		if v >= 3600 && v%3600 == 0 {
			return float64(v) / 3600
		}

		if v >= 60 && v <= 1440 && v%60 == 0 {
			return float64(v) / 60
		}

		if v > 0 && v <= 24 {
			return float64(v)
		}
	}

	return 0
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}

func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	if c.apiKey == "" || c.apiSecret == "" {
		return domain.OrderResult{}, fmt.Errorf("mexc api keys are empty")
	}

	return domain.OrderResult{}, fmt.Errorf("mexc live orders not implemented")
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
		return fmt.Errorf("mexc GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("mexc GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("mexc api key or secret is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	query := encodeSorted(q)
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)

	payload := c.apiKey + timestamp + query
	signature := c.sign(payload)

	u := c.baseURL + path
	if query != "" {
		u += "?" + query
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("ApiKey", c.apiKey)
	req.Header.Set("Request-Time", timestamp)
	req.Header.Set("Signature", signature)
	req.Header.Set("Recv-Window", "30000")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("mexc signed GET %s: %s", path, string(raw))
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("mexc signed GET %s decode error: %w; body=%s", path, err, string(raw))
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
			if value == "" {
				parts = append(parts, k+"=")
				continue
			}

			parts = append(parts, k+"="+url.QueryEscape(value))
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

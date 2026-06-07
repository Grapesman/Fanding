package bitget

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	return domain.ExchangeName("Bitget")
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	if err == nil {
		return true
	}

	// Если private balance работает, значит биржа доступна.
	if c.apiKey != "" && c.apiSecret != "" && c.passphrase != "" {
		b, balanceErr := c.Balance()
		if balanceErr == nil && b.PrivateOK {
			return true
		}
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

	err := c.getJSON("/api/v2/public/time", nil, &out)
	if err != nil {
		return time.Time{}, err
	}

	if out.Code != "" && out.Code != "00000" {
		return time.Time{}, fmt.Errorf("bitget server time code=%s msg=%s", out.Code, out.Msg)
	}

	ts := int64(out.Data)
	if ts <= 0 {
		ts = int64(out.RequestTime)
	}

	if ts > 0 {
		return time.UnixMilli(ts), nil
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bitget API key, secret or passphrase is empty",
			UpdatedAt: now,
		}, nil
	}

	q := url.Values{}
	q.Set("productType", "USDT-FUTURES")

	var out struct {
		Code        string `json:"code"`
		Msg         string `json:"msg"`
		RequestTime int64  `json:"requestTime"`
		Data        []struct {
			MarginCoin           string        `json:"marginCoin"`
			Locked               flexibleFloat `json:"locked"`
			Available            flexibleFloat `json:"available"`
			CrossedMaxAvailable  flexibleFloat `json:"crossedMaxAvailable"`
			IsolatedMaxAvailable flexibleFloat `json:"isolatedMaxAvailable"`
			MaxTransferOut       flexibleFloat `json:"maxTransferOut"`
			AccountEquity        flexibleFloat `json:"accountEquity"`
			USDTEQuity           flexibleFloat `json:"usdtEquity"`
			UnrealizedPL         flexibleFloat `json:"unrealizedPL"`
			CrossedUnrealizedPL  flexibleFloat `json:"crossedUnrealizedPL"`
			IsolatedUnrealizedPL flexibleFloat `json:"isolatedUnrealizedPL"`
			IsolatedMargin       flexibleFloat `json:"isolatedMargin"`
			CrossedMargin        flexibleFloat `json:"crossedMargin"`
			UnionAvailable       flexibleFloat `json:"unionAvailable"`
			UnionTotalMargin     flexibleFloat `json:"unionTotalMargin"`
			AssetMode            string        `json:"assetMode"`
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
			UpdatedAt: time.Now(),
		}, err
	}

	if out.Code != "" && out.Code != "00000" {
		err := fmt.Errorf("Bitget balance code=%s msg=%s", out.Code, out.Msg)

		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	for _, a := range out.Data {
		if strings.EqualFold(a.MarginCoin, "USDT") {
			wallet := float64(a.USDTEQuity)
			if wallet <= 0 {
				wallet = float64(a.AccountEquity)
			}

			available := float64(a.Available)
			if available <= 0 {
				available = float64(a.IsolatedMaxAvailable)
			}
			if available <= 0 {
				available = float64(a.CrossedMaxAvailable)
			}
			if available <= 0 {
				available = float64(a.UnionAvailable)
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
	}

	for _, a := range out.Data {
		for _, asset := range a.AssetList {
			if strings.EqualFold(asset.Coin, "USDT") {
				wallet := float64(asset.Balance)
				available := float64(asset.Available)

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
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "Bitget USDT futures balance not found",
		UpdatedAt: time.Now(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	intervalBySymbol := c.contractFundingIntervals()

	q := url.Values{}
	q.Set("productType", "USDT-FUTURES")

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

	if out.Code != "00000" && out.Code != "" {
		return nil, fmt.Errorf("bitget tickers: %s", out.Msg)
	}

	now := time.Now().UTC()
	res := make([]domain.Candidate, 0, len(out.Data))

	for _, t := range out.Data {
		if !strings.HasSuffix(t.Symbol, "USDT") {
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

		vol := float64(t.UsdtVolume)
		if vol <= 0 {
			vol = float64(t.QuoteVolume)
		}
		if vol <= 0 {
			vol = float64(t.BaseVolume) * last
		}

		bid := float64(t.BidPr)
		ask := float64(t.AskPr)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		intervalHours := extractIntervalHours(t.FundingRateInterval, t.FundInterval)
		if intervalHours <= 0 {
			intervalHours = intervalBySymbol[t.Symbol]
		}
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextRaw := int64(t.NextFundingTime)
		if nextRaw <= 0 {
			nextRaw = int64(t.NextUpdate)
		}

		nextFunding := parseBitgetFundingTime(nextRaw)
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               t.Symbol,
			NativeSymbol:         t.Symbol,
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
	q.Set("productType", "USDT-FUTURES")

	var resp struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol       string        `json:"symbol"`
			FundInterval flexibleFloat `json:"fundInterval"`
			SymbolStatus string        `json:"symbolStatus"`
			SymbolType   string        `json:"symbolType"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v2/mix/market/contracts", q, &resp); err != nil {
		fmt.Printf("Bitget contracts error: %v\n", err)
		return out
	}

	if resp.Code != "" && resp.Code != "00000" {
		fmt.Printf("Bitget contracts code=%s msg=%s\n", resp.Code, resp.Msg)
		return out
	}

	for _, item := range resp.Data {
		if item.Symbol == "" {
			continue
		}

		interval := extractIntervalHours(item.FundInterval)
		if interval <= 0 {
			continue
		}

		out[item.Symbol] = interval
	}

	fmt.Printf("Bitget contract intervals loaded: %d\n", len(out))

	return out
}

func parseBitgetFundingTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}

	if v > 1_000_000_000_000 {
		return time.UnixMilli(v).UTC()
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
	return domain.OrderResult{}, fmt.Errorf("bitget live orders not implemented")
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
	req.Header.Set("locale", "en-US")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("bitget GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("bitget GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return fmt.Errorf("bitget api key, secret or passphrase is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	query := q.Encode()
	requestPath := path
	if query != "" {
		requestPath += "?" + query
	}

	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	method := http.MethodGet
	body := ""

	signature := c.sign(timestamp + method + requestPath + body)

	u := c.baseURL + requestPath

	req, err := http.NewRequest(method, u, nil)
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
		return fmt.Errorf("bitget signed GET %s: %s", path, string(raw))
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("bitget signed GET %s decode error: %w; body=%s", path, err, string(raw))
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

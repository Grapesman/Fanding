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

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		apiSecret: strings.TrimSpace(apiSecret),
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeName("BingX")
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

	err := c.getJSON("/openApi/swap/v2/server/time", nil, &out)
	if err != nil {
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

	now := time.Now()
	res := make([]domain.Candidate, 0, len(tick.Data))

	for _, t := range tick.Data {
		if !strings.Contains(t.Symbol, "USDT") {
			continue
		}

		price, _ := strconv.ParseFloat(t.LastPrice, 64)
		vol, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		if vol <= 0 {
			vol, _ = strconv.ParseFloat(t.Volume, 64)
		}

		bid, _ := strconv.ParseFloat(t.BidPrice, 64)
		ask, _ := strconv.ParseFloat(t.AskPrice, 64)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		q := url.Values{}
		q.Set("symbol", t.Symbol)

		var prem struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				LastFundingRate string `json:"lastFundingRate"`
				FundingRate     string `json:"fundingRate"`
				MarkPrice       string `json:"markPrice"`
				NextFundingTime string `json:"nextFundingTime"`
			} `json:"data"`
		}

		_ = c.getJSON("/openApi/swap/v2/quote/premiumIndex", q, &prem)

		fr, _ := strconv.ParseFloat(firstNonEmpty(prem.Data.LastFundingRate, prem.Data.FundingRate), 64)
		mark, _ := strconv.ParseFloat(prem.Data.MarkPrice, 64)
		if mark <= 0 {
			mark = price
		}

		nextMS, _ := strconv.ParseInt(prem.Data.NextFundingTime, 10, 64)

		nextFunding := time.Time{}
		if nextMS > 0 {
			nextFunding = time.UnixMilli(nextMS)
		} else {
			nextFunding = nextFundingByInterval(now.UTC(), 8)
		}

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               strings.ReplaceAll(t.Symbol, "-", ""),
			NativeSymbol:         t.Symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          fr,
			FundingIntervalHours: 8,
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
	req.Header.Set("X-SOURCE-KEY", "BX-AI-SKILL")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("bingx GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("bingx GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
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

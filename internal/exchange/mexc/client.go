package mexc

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		apiSecret: apiSecret,
		http:      &http.Client{Timeout: 10 * time.Second},
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
		Data    flexibleInt `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/ping", &out); err != nil {
		return time.Time{}, err
	}

	if out.Data > 0 {
		return time.UnixMilli(int64(out.Data)), nil
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	return domain.Balance{
		Exchange:  c.Name(),
		UpdatedAt: time.Now(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	var out struct {
		Success bool `json:"success"`
		Code    int  `json:"code"`
		Data    []struct {
			Symbol      string        `json:"symbol"`
			LastPrice   flexibleFloat `json:"lastPrice"`
			FairPrice   flexibleFloat `json:"fairPrice"`
			IndexPrice  flexibleFloat `json:"indexPrice"`
			FundingRate flexibleFloat `json:"fundingRate"`
			Volume24    flexibleFloat `json:"volume24"`
			Amount24    flexibleFloat `json:"amount24"`
			Bid1        flexibleFloat `json:"bid1"`
			Ask1        flexibleFloat `json:"ask1"`
			Timestamp   flexibleInt   `json:"timestamp"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v1/contract/ticker", &out); err != nil {
		return nil, err
	}

	if !out.Success && out.Code != 0 {
		return nil, fmt.Errorf("mexc ticker response code=%d", out.Code)
	}

	now := time.Now()
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
			vol = float64(t.Volume24)
		}

		nextFunding := nextFundingByInterval(now.UTC(), 8)

		symbol := strings.ReplaceAll(t.Symbol, "_", "")

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         t.Symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          float64(t.FundingRate),
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

func (c *Client) getJSON(path string, dst any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
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

	return json.Unmarshal(body, dst)
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

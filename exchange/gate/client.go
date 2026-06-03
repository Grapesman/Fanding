package gate

import (
	"encoding/json"
	"fmt"
	"funding-bot/internal/domain"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL, apiKey, apiSecret string
	http                       *http.Client
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, apiSecret: apiSecret, http: &http.Client{Timeout: 10 * time.Second}}
}
func (c *Client) Name() domain.ExchangeName { return domain.ExchangeName("Gate") }
func (c *Client) Connected() bool           { _, err := c.ServerTime(); return err == nil }
func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		ServerTime float64 `json:"server_time"`
	}
	err := c.getJSON("/api/v4/futures/usdt/time", nil, &out)
	if err != nil {
		return time.Time{}, err
	}
	if out.ServerTime > 0 {
		return time.Unix(int64(out.ServerTime), 0), nil
	}
	return time.Now(), nil
}
func (c *Client) Balance() (domain.Balance, error) {
	return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
}
func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	var rows []struct {
		Contract, Last, MarkPrice, IndexPrice, FundingRate, FundingInterval, FundingNextApply, Volume24hUSD, Bid1, Ask1 string `json:",omitempty"`
	}
	if err := c.getJSON("/api/v4/futures/usdt/tickers", nil, &rows); err != nil {
		return nil, err
	}
	now := time.Now()
	res := []domain.Candidate{}
	for _, t := range rows {
		if !strings.Contains(t.Contract, "USDT") {
			continue
		}
		price, _ := strconv.ParseFloat(t.Last, 64)
		mark, _ := strconv.ParseFloat(t.MarkPrice, 64)
		fr, _ := strconv.ParseFloat(t.FundingRate, 64)
		interval, _ := strconv.ParseFloat(t.FundingInterval, 64)
		if interval > 100 {
			interval = interval / 3600
		}
		if interval <= 0 {
			interval = 8
		}
		vol, _ := strconv.ParseFloat(t.Volume24hUSD, 64)
		bid, _ := strconv.ParseFloat(t.Bid1, 64)
		ask, _ := strconv.ParseFloat(t.Ask1, 64)
		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}
		nextSec, _ := strconv.ParseInt(t.FundingNextApply, 10, 64)
		sym := strings.ReplaceAll(t.Contract, "_", "")
		res = append(res, domain.Candidate{Exchange: c.Name(), Symbol: sym, Price: price, MarkPrice: mark, FundingRate: fr, FundingIntervalHours: interval, NextFundingTime: time.Unix(nextSec, 0), Volume24hUSDT: vol, Bid: bid, Ask: ask, Spread: spread, UpdatedAt: now})
	}
	return res, nil
}
func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}
func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	return domain.OrderResult{}, fmt.Errorf("gate live orders not implemented")
}
func (c *Client) CancelOrder(symbol, orderID string) error { return nil }
func (c *Client) CancelAll(symbol string) error            { return nil }
func (c *Client) ClosePositionMarket(symbol string) error  { return nil }
func (c *Client) getJSON(path string, q url.Values, dst any) error {
	u := c.baseURL + path
	if q != nil && len(q) > 0 {
		u += "?" + q.Encode()
	}
	resp, err := c.http.Get(u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("gate GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

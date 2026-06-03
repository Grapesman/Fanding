package bitget

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"funding-bot/internal/domain"
)

type Client struct {
	baseURL, apiKey, apiSecret string
	http                       *http.Client
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, apiSecret: apiSecret, http: &http.Client{Timeout: 10 * time.Second}}
}
func (c *Client) Name() domain.ExchangeName { return domain.ExchangeName("Bitget") }
func (c *Client) Connected() bool           { _, err := c.ServerTime(); return err == nil }
func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		RequestTime int64 `json:"requestTime"`
	}
	err := c.getJSON("/api/v2/public/time", nil, &out)
	if err != nil {
		return time.Time{}, err
	}
	if out.RequestTime > 0 {
		return time.UnixMilli(out.RequestTime), nil
	}
	return time.Now(), nil
}
func (c *Client) Balance() (domain.Balance, error) {
	return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
}
func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	q := url.Values{}
	q.Set("productType", "USDT-FUTURES")
	var out struct {
		Code string                                                                                                                           `json:"code"`
		Msg  string                                                                                                                           `json:"msg"`
		Data []struct{ Symbol, LastPr, MarkPrice, IndexPrice, FundingRate, FundingRateInterval, NextUpdate, UsdtVolume, BidPr, AskPr string } `json:"data"`
	}
	if err := c.getJSON("/api/v2/mix/market/tickers", q, &out); err != nil {
		return nil, err
	}
	if out.Code != "00000" && out.Code != "" {
		return nil, fmt.Errorf("bitget tickers: %s", out.Msg)
	}
	now := time.Now()
	res := make([]domain.Candidate, 0, len(out.Data))
	for _, t := range out.Data {
		if !strings.HasSuffix(t.Symbol, "USDT") {
			continue
		}
		last, _ := strconv.ParseFloat(t.LastPr, 64)
		mark, _ := strconv.ParseFloat(t.MarkPrice, 64)
		fr, _ := strconv.ParseFloat(t.FundingRate, 64)
		interval, _ := strconv.ParseFloat(t.FundingRateInterval, 64)
		if interval <= 0 {
			interval = 8
		}
		vol, _ := strconv.ParseFloat(t.UsdtVolume, 64)
		bid, _ := strconv.ParseFloat(t.BidPr, 64)
		ask, _ := strconv.ParseFloat(t.AskPr, 64)
		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}
		nextMS, _ := strconv.ParseInt(t.NextUpdate, 10, 64)
		res = append(res, domain.Candidate{Exchange: c.Name(), Symbol: t.Symbol, Price: last, MarkPrice: mark, FundingRate: fr, FundingIntervalHours: interval, NextFundingTime: time.UnixMilli(nextMS), Volume24hUSDT: vol, Bid: bid, Ask: ask, Spread: spread, UpdatedAt: now})
	}
	return res, nil
}
func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}
func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	return domain.OrderResult{}, fmt.Errorf("bitget live orders not implemented")
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
		return fmt.Errorf("bitget GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

package kucoin

import (
	"encoding/json"
	"fmt"
	"funding-bot/internal/domain"
	"io"
	"net/http"
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
func (c *Client) Name() domain.ExchangeName { return domain.ExchangeName("KuCoin") }
func (c *Client) Connected() bool           { _, err := c.ServerTime(); return err == nil }
func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Code string `json:"code"`
		Data int64  `json:"data"`
	}
	err := c.getJSON("/api/v1/timestamp", &out)
	if err != nil {
		return time.Time{}, err
	}
	if out.Data > 0 {
		return time.UnixMilli(out.Data), nil
	}
	return time.Now(), nil
}
func (c *Client) Balance() (domain.Balance, error) {
	return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
}
func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	var out struct {
		Code string `json:"code"`
		Data []struct {
			Symbol, QuoteCurrency                     string
			MarkPrice, LastTradePrice, FundingFeeRate float64
			NextFundingRateTime                       int64
			VolumeOf24h                               float64
			TurnoverOf24h                             float64
		} `json:"data"`
	}
	if err := c.getJSON("/api/v1/contracts/active", &out); err != nil {
		return nil, err
	}
	if out.Code != "200000" && out.Code != "" {
		return nil, fmt.Errorf("kucoin contracts: %s", out.Code)
	}
	now := time.Now()
	res := []domain.Candidate{}
	for _, t := range out.Data {
		if t.QuoteCurrency != "USDT" && !strings.HasSuffix(t.Symbol, "USDTM") {
			continue
		}
		price := t.LastTradePrice
		if price == 0 {
			price = t.MarkPrice
		}
		vol := t.TurnoverOf24h
		if vol == 0 {
			vol = t.VolumeOf24h * price
		}
		sym := strings.TrimSuffix(t.Symbol, "M")
		res = append(res, domain.Candidate{Exchange: c.Name(), Symbol: sym, Price: price, MarkPrice: t.MarkPrice, FundingRate: t.FundingFeeRate, FundingIntervalHours: 8, NextFundingTime: time.UnixMilli(t.NextFundingRateTime), Volume24hUSDT: vol, UpdatedAt: now})
	}
	return res, nil
}
func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}
func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	return domain.OrderResult{}, fmt.Errorf("kucoin live orders not implemented")
}
func (c *Client) CancelOrder(symbol, orderID string) error { return nil }
func (c *Client) CancelAll(symbol string) error            { return nil }
func (c *Client) ClosePositionMarket(symbol string) error  { return nil }
func (c *Client) getJSON(path string, dst any) error {
	resp, err := c.http.Get(c.baseURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("kucoin GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

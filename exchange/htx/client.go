package htx

import (
	"encoding/json"
	"fmt"
	"funding-bot/internal/domain"
	"io"
	"net/http"
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
func (c *Client) Name() domain.ExchangeName      { return domain.ExchangeName("HTX") }
func (c *Client) Connected() bool                { _, err := c.ServerTime(); return err == nil }
func (c *Client) ServerTime() (time.Time, error) { return time.Now(), nil }
func (c *Client) Balance() (domain.Balance, error) {
	return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
}
func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	var merged struct {
		Status string `json:"status"`
		Ticks  []struct {
			ContractCode    string `json:"contract_code"`
			Close, Ask, Bid []float64
			Amount, Vol     float64
		} `json:"ticks"`
	}
	if err := c.getJSON("/linear-swap-ex/market/detail/batch_merged", &merged); err != nil {
		return nil, err
	}
	var fund struct {
		Status string `json:"status"`
		Data   []struct {
			ContractCode    string `json:"contract_code"`
			FundingRate     string `json:"funding_rate"`
			NextFundingTime int64  `json:"next_funding_time"`
		} `json:"data"`
	}
	_ = c.getJSON("/linear-swap-api/v1/swap_batch_funding_rate", &fund)
	frBy := map[string]struct {
		fr   float64
		next time.Time
	}{}
	for _, f := range fund.Data {
		fr, _ := strconv.ParseFloat(f.FundingRate, 64)
		frBy[f.ContractCode] = struct {
			fr   float64
			next time.Time
		}{fr: fr, next: time.UnixMilli(f.NextFundingTime)}
	}
	now := time.Now()
	res := []domain.Candidate{}
	for _, t := range merged.Ticks {
		if !strings.Contains(t.ContractCode, "USDT") {
			continue
		}
		price := 0.0
		if len(t.Close) > 0 {
			price = t.Close[0]
		}
		bid, ask := 0.0, 0.0
		if len(t.Bid) > 0 {
			bid = t.Bid[0]
		}
		if len(t.Ask) > 0 {
			ask = t.Ask[0]
		}
		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}
		f := frBy[t.ContractCode]
		sym := strings.ReplaceAll(t.ContractCode, "-", "")
		res = append(res, domain.Candidate{Exchange: c.Name(), Symbol: sym, Price: price, MarkPrice: price, FundingRate: f.fr, FundingIntervalHours: 8, NextFundingTime: f.next, Volume24hUSDT: t.Vol, Bid: bid, Ask: ask, Spread: spread, UpdatedAt: now})
	}
	return res, nil
}
func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}
func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	return domain.OrderResult{}, fmt.Errorf("htx live orders not implemented")
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
		return fmt.Errorf("htx GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

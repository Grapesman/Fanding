package mexc

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
func (c *Client) Name() domain.ExchangeName { return domain.ExchangeName("MEXC") }
func (c *Client) Connected() bool           { _, err := c.ServerTime(); return err == nil }
func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Data int64 `json:"data"`
	}
	err := c.getJSON("/api/v1/contract/ping", &out)
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
		Success bool `json:"success"`
		Data    []struct {
			Symbol                                                                        string `json:"symbol"`
			LastPrice, FairPrice, IndexPrice, FundingRate, Volume24, Amount24, Bid1, Ask1 string
		} `json:"data"`
	}
	if err := c.getJSON("/api/v1/contract/ticker", &out); err != nil {
		return nil, err
	}
	now := time.Now()
	res := []domain.Candidate{}
	for _, t := range out.Data {
		if !strings.Contains(t.Symbol, "USDT") {
			continue
		}
		price, _ := strconv.ParseFloat(t.LastPrice, 64)
		mark, _ := strconv.ParseFloat(t.FairPrice, 64)
		fr, _ := strconv.ParseFloat(t.FundingRate, 64)
		vol, _ := strconv.ParseFloat(t.Amount24, 64)
		bid, _ := strconv.ParseFloat(t.Bid1, 64)
		ask, _ := strconv.ParseFloat(t.Ask1, 64)
		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}
		sym := strings.ReplaceAll(t.Symbol, "_", "")
		res = append(res, domain.Candidate{Exchange: c.Name(), Symbol: sym, Price: price, MarkPrice: mark, FundingRate: fr, FundingIntervalHours: 8, Volume24hUSDT: vol, Bid: bid, Ask: ask, Spread: spread, UpdatedAt: now})
	}
	return res, nil
}
func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}
func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	return domain.OrderResult{}, fmt.Errorf("mexc live orders not implemented")
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
		return fmt.Errorf("mexc GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

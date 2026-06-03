package bingx

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
func (c *Client) Name() domain.ExchangeName { return domain.ExchangeName("BingX") }
func (c *Client) Connected() bool           { _, err := c.ServerTime(); return err == nil }
func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Code int `json:"code"`
		Data struct {
			ServerTime int64 `json:"serverTime"`
		} `json:"data"`
	}
	err := c.getJSON("/openApi/swap/v2/server/time", nil, &out)
	if err != nil {
		return time.Time{}, err
	}
	if out.Data.ServerTime > 0 {
		return time.UnixMilli(out.Data.ServerTime), nil
	}
	return time.Now(), nil
}
func (c *Client) Balance() (domain.Balance, error) {
	return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
}
func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	// BingX public API has separate ticker and premiumIndex endpoints. Verify exact response fields after first run.
	var tick struct {
		Code int                                                                           `json:"code"`
		Msg  string                                                                        `json:"msg"`
		Data []struct{ Symbol, LastPrice, QuoteVolume, Volume, AskPrice, BidPrice string } `json:"data"`
	}
	if err := c.getJSON("/openApi/swap/v2/quote/ticker", nil, &tick); err != nil {
		return nil, err
	}
	now := time.Now()
	res := []domain.Candidate{}
	for _, t := range tick.Data {
		if !strings.Contains(t.Symbol, "USDT") {
			continue
		}
		price, _ := strconv.ParseFloat(t.LastPrice, 64)
		vol, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		bid, _ := strconv.ParseFloat(t.BidPrice, 64)
		ask, _ := strconv.ParseFloat(t.AskPrice, 64)
		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}
		q := url.Values{}
		q.Set("symbol", t.Symbol)
		var prem struct {
			Code int                                                                       `json:"code"`
			Data struct{ LastFundingRate, FundingRate, MarkPrice, NextFundingTime string } `json:"data"`
		}
		_ = c.getJSON("/openApi/swap/v2/quote/premiumIndex", q, &prem)
		fr, _ := strconv.ParseFloat(firstNonEmpty(prem.Data.LastFundingRate, prem.Data.FundingRate), 64)
		mark, _ := strconv.ParseFloat(prem.Data.MarkPrice, 64)
		if mark == 0 {
			mark = price
		}
		nextMS, _ := strconv.ParseInt(prem.Data.NextFundingTime, 10, 64)
		res = append(res, domain.Candidate{Exchange: c.Name(), Symbol: strings.ReplaceAll(t.Symbol, "-", ""), Price: price, MarkPrice: mark, FundingRate: fr, FundingIntervalHours: 8, NextFundingTime: time.UnixMilli(nextMS), Volume24hUSDT: vol, Bid: bid, Ask: ask, Spread: spread, UpdatedAt: now})
	}
	return res, nil
}
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}
func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	return domain.OrderResult{}, fmt.Errorf("bingx live orders not implemented")
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
		return fmt.Errorf("bingx GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

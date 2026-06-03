package okx

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
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, apiSecret: apiSecret, http: &http.Client{Timeout: 12 * time.Second}}
}
func (c *Client) Name() domain.ExchangeName { return domain.ExchangeName("OKX") }
func (c *Client) Connected() bool           { _, err := c.ServerTime(); return err == nil }
func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Code string `json:"code"`
		Data []struct {
			Ts string `json:"ts"`
		} `json:"data"`
	}
	err := c.getJSON("/api/v5/public/time", nil, &out)
	if err != nil {
		return time.Time{}, err
	}
	if len(out.Data) > 0 {
		ms, _ := strconv.ParseInt(out.Data[0].Ts, 10, 64)
		if ms > 0 {
			return time.UnixMilli(ms), nil
		}
	}
	return time.Now(), nil
}
func (c *Client) Balance() (domain.Balance, error) {
	return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
}
func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	q := url.Values{}
	q.Set("instType", "SWAP")
	var tick struct {
		Code string                                                   `json:"code"`
		Msg  string                                                   `json:"msg"`
		Data []struct{ InstId, Last, VolCcy24h, AskPx, BidPx string } `json:"data"`
	}
	if err := c.getJSON("/api/v5/market/tickers", q, &tick); err != nil {
		return nil, err
	}
	if tick.Code != "0" && tick.Code != "" {
		return nil, fmt.Errorf("okx tickers: %s", tick.Msg)
	}
	now := time.Now()
	res := []domain.Candidate{}
	for _, t := range tick.Data {
		if !strings.Contains(t.InstId, "USDT-SWAP") {
			continue
		}
		fq := url.Values{}
		fq.Set("instId", t.InstId)
		var frOut struct {
			Code string `json:"code"`
			Data []struct {
				FundingRate, NextFundingTime, InstId string `json:",omitempty"`
			} `json:"data"`
		}
		_ = c.getJSON("/api/v5/public/funding-rate", fq, &frOut)
		fr := 0.0
		next := time.Time{}
		if len(frOut.Data) > 0 {
			fr, _ = strconv.ParseFloat(frOut.Data[0].FundingRate, 64)
			ms, _ := strconv.ParseInt(frOut.Data[0].NextFundingTime, 10, 64)
			next = time.UnixMilli(ms)
		}
		price, _ := strconv.ParseFloat(t.Last, 64)
		vol, _ := strconv.ParseFloat(t.VolCcy24h, 64)
		bid, _ := strconv.ParseFloat(t.BidPx, 64)
		ask, _ := strconv.ParseFloat(t.AskPx, 64)
		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}
		sym := strings.ReplaceAll(strings.TrimSuffix(t.InstId, "-SWAP"), "-", "")
		res = append(res, domain.Candidate{Exchange: c.Name(), Symbol: sym, Price: price, MarkPrice: price, FundingRate: fr, FundingIntervalHours: 8, NextFundingTime: next, Volume24hUSDT: vol, Bid: bid, Ask: ask, Spread: spread, UpdatedAt: now})
	}
	return res, nil
}
func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}
func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	return domain.OrderResult{}, fmt.Errorf("okx live orders not implemented")
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
		return fmt.Errorf("okx GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

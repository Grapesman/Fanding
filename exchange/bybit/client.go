package bybit

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
	baseURL   string
	apiKey    string
	apiSecret string
	http      *http.Client
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, apiSecret: apiSecret, http: &http.Client{Timeout: 8 * time.Second}}
}

func (c *Client) Name() domain.ExchangeName { return domain.ExchangeBybit }
func (c *Client) Connected() bool           { _, err := c.ServerTime(); return err == nil }

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Time   int64 `json:"time"`
		Result struct {
			TimeSecond string `json:"timeSecond"`
			TimeNano   string `json:"timeNano"`
		} `json:"result"`
	}
	if err := c.getJSON("/v5/market/time", nil, &out); err != nil {
		return time.Time{}, err
	}
	if out.Time > 0 {
		return time.UnixMilli(out.Time), nil
	}
	sec, _ := strconv.ParseInt(out.Result.TimeSecond, 10, 64)
	if sec > 0 {
		return time.Unix(sec, 0), nil
	}
	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	// Placeholder until account type is confirmed (UNIFIED/CONTRACT). Monitor/paper mode do not require private balance.
	return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	q := url.Values{}
	q.Set("category", "linear")
	var out struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			List []struct {
				Symbol          string `json:"symbol"`
				LastPrice       string `json:"lastPrice"`
				MarkPrice       string `json:"markPrice"`
				IndexPrice      string `json:"indexPrice"`
				FundingRate     string `json:"fundingRate"`
				NextFundingTime string `json:"nextFundingTime"`
				Volume24h       string `json:"volume24h"`
				Turnover24h     string `json:"turnover24h"`
				Bid1Price       string `json:"bid1Price"`
				Ask1Price       string `json:"ask1Price"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := c.getJSON("/v5/market/tickers", q, &out); err != nil {
		return nil, err
	}
	if out.RetCode != 0 {
		return nil, fmt.Errorf("bybit tickers: %s", out.RetMsg)
	}
	now := time.Now()
	res := make([]domain.Candidate, 0, len(out.Result.List))
	for _, t := range out.Result.List {
		if !strings.HasSuffix(t.Symbol, "USDT") {
			continue
		}
		last, _ := strconv.ParseFloat(t.LastPrice, 64)
		mark, _ := strconv.ParseFloat(t.MarkPrice, 64)
		fr, _ := strconv.ParseFloat(t.FundingRate, 64)
		nextMS, _ := strconv.ParseInt(t.NextFundingTime, 10, 64)
		turn, _ := strconv.ParseFloat(t.Turnover24h, 64)
		bid, _ := strconv.ParseFloat(t.Bid1Price, 64)
		ask, _ := strconv.ParseFloat(t.Ask1Price, 64)
		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}
		res = append(res, domain.Candidate{
			Exchange: c.Name(), Symbol: t.Symbol, Price: last, MarkPrice: mark, FundingRate: fr,
			NextFundingTime: time.UnixMilli(nextMS), Volume24hUSDT: turn, Bid: bid, Ask: ask, Spread: spread, UpdatedAt: now,
		})
	}
	return res, nil
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}
func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	if c.apiKey == "" || c.apiSecret == "" {
		return domain.OrderResult{}, fmt.Errorf("bybit api keys are empty")
	}
	return domain.OrderResult{}, fmt.Errorf("live Bybit order placement is scaffolded; enable after testnet validation")
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
		return fmt.Errorf("bybit GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

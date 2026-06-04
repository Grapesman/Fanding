package gate

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
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		apiSecret: apiSecret,
		http:      &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeGate
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		ServerTime int64 `json:"server_time"`
	}

	// Gate futures does not have the same simple futures time endpoint pattern as some others.
	// Use a lightweight public futures contract endpoint as a connectivity check.
	var contract struct {
		Name string `json:"name"`
	}

	if err := c.getJSON("/api/v4/futures/usdt/contracts/BTC_USDT", nil, &contract); err != nil {
		return time.Time{}, err
	}

	if contract.Name == "" {
		return time.Time{}, fmt.Errorf("gate connectivity check returned empty contract")
	}

	if out.ServerTime > 0 {
		return time.Unix(out.ServerTime, 0), nil
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
	var rows []struct {
		Contract              string `json:"contract"`
		Last                  string `json:"last"`
		MarkPrice             string `json:"mark_price"`
		IndexPrice            string `json:"index_price"`
		FundingRate           string `json:"funding_rate"`
		FundingRateIndicative string `json:"funding_rate_indicative"`

		// Gate ticker endpoint usually does not return funding interval / next apply.
		// These fields may exist on contract endpoint, so keep them optional here.
		FundingInterval  int64 `json:"funding_interval"`
		FundingNextApply int64 `json:"funding_next_apply"`

		Volume24h       string `json:"volume_24h"`
		Volume24hBase   string `json:"volume_24h_base"`
		Volume24hQuote  string `json:"volume_24h_quote"`
		Volume24hSettle string `json:"volume_24h_settle"`

		HighestBid string `json:"highest_bid"`
		LowestAsk  string `json:"lowest_ask"`
	}

	if err := c.getJSON("/api/v4/futures/usdt/tickers", nil, &rows); err != nil {
		return nil, err
	}

	now := time.Now()
	out := make([]domain.Candidate, 0, len(rows))

	for _, t := range rows {
		if t.Contract == "" {
			continue
		}

		if !strings.HasSuffix(t.Contract, "_USDT") {
			continue
		}

		price, _ := strconv.ParseFloat(t.Last, 64)
		mark, _ := strconv.ParseFloat(t.MarkPrice, 64)
		if mark <= 0 {
			mark = price
		}

		fr, _ := strconv.ParseFloat(t.FundingRate, 64)
		if fr == 0 && t.FundingRateIndicative != "" {
			fr, _ = strconv.ParseFloat(t.FundingRateIndicative, 64)
		}

		vol, _ := strconv.ParseFloat(t.Volume24hQuote, 64)
		if vol <= 0 {
			vol, _ = strconv.ParseFloat(t.Volume24hSettle, 64)
		}
		if vol <= 0 {
			vol, _ = strconv.ParseFloat(t.Volume24h, 64)
		}

		bid, _ := strconv.ParseFloat(t.HighestBid, 64)
		ask, _ := strconv.ParseFloat(t.LowestAsk, 64)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		intervalHours := float64(t.FundingInterval)
		if intervalHours > 100 {
			intervalHours = intervalHours / 3600
		}
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFundingTime := time.Time{}
		if t.FundingNextApply > 0 {
			nextFundingTime = time.Unix(t.FundingNextApply, 0)
		} else {
			nextFundingTime = nextFundingByInterval(now.UTC(), int(intervalHours))
		}

		symbol := strings.ReplaceAll(t.Contract, "_", "")

		out = append(out, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         t.Contract,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          fr,
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFundingTime,
			Volume24hUSDT:        vol,
			Bid:                  bid,
			Ask:                  ask,
			Spread:               spread,
			UpdatedAt:            now,
		})
	}

	return out, nil
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
	return domain.OrderResult{}, fmt.Errorf("gate live orders not implemented")
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

	resp, err := c.http.Do(req)
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

package htx

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
	return domain.ExchangeHTX
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Status string `json:"status"`
		TS     int64  `json:"ts"`
	}

	if err := c.getJSON("/linear-swap-api/v1/swap_contract_info?contract_code=BTC-USDT", &out); err != nil {
		return time.Time{}, err
	}

	if out.TS > 0 {
		return time.UnixMilli(out.TS), nil
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
	var merged struct {
		Status string `json:"status"`
		Ticks  []struct {
			ContractCode  string        `json:"contract_code"`
			Close         flexibleFloat `json:"close"`
			Amount        flexibleFloat `json:"amount"`
			Vol           flexibleFloat `json:"vol"`
			TradeTurnover flexibleFloat `json:"trade_turnover"`
			Ask           []float64     `json:"ask"`
			Bid           []float64     `json:"bid"`
		} `json:"ticks"`
	}

	if err := c.getJSON("/linear-swap-ex/market/detail/batch_merged", &merged); err != nil {
		return nil, err
	}

	if merged.Status != "" && merged.Status != "ok" {
		return nil, fmt.Errorf("htx merged status: %s", merged.Status)
	}

	var fund struct {
		Status string `json:"status"`
		Data   []struct {
			ContractCode    string        `json:"contract_code"`
			FundingRate     flexibleFloat `json:"funding_rate"`
			FundingTime     flexibleInt   `json:"funding_time"`
			NextFundingTime flexibleInt   `json:"next_funding_time"`
		} `json:"data"`
	}

	if err := c.getJSON("/linear-swap-api/v1/swap_batch_funding_rate", &fund); err != nil {
		return nil, err
	}

	frBy := map[string]struct {
		rate float64
		next time.Time
	}{}

	for _, f := range fund.Data {
		nextMS := int64(f.NextFundingTime)
		if nextMS <= 0 {
			nextMS = int64(f.FundingTime)
		}

		nextTime := time.Time{}
		if nextMS > 0 {
			nextTime = time.UnixMilli(nextMS)
		}

		frBy[f.ContractCode] = struct {
			rate float64
			next time.Time
		}{
			rate: float64(f.FundingRate),
			next: nextTime,
		}
	}

	now := time.Now()
	res := make([]domain.Candidate, 0, len(merged.Ticks))

	for _, t := range merged.Ticks {
		if t.ContractCode == "" || !strings.Contains(t.ContractCode, "USDT") {
			continue
		}

		price := float64(t.Close)
		bid := 0.0
		ask := 0.0

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

		vol := float64(t.TradeTurnover)
		if vol <= 0 {
			vol = float64(t.Vol)
		}
		if vol <= 0 {
			vol = float64(t.Amount) * price
		}

		funding := frBy[t.ContractCode]

		nextFunding := funding.next
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now.UTC(), 8)
		}

		symbol := strings.ReplaceAll(t.ContractCode, "-", "")

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         t.ContractCode,
			Price:                price,
			MarkPrice:            price,
			FundingRate:          funding.rate,
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
	return domain.OrderResult{}, fmt.Errorf("htx live orders not implemented")
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
		return fmt.Errorf("htx GET %s: %s", path, string(body))
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

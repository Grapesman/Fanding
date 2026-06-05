package binance

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		apiSecret: strings.TrimSpace(apiSecret),
		http:      &http.Client{Timeout: 8 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeBinance
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		ServerTime int64 `json:"serverTime"`
	}

	if err := c.getJSON("/fapi/v1/time", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.ServerTime <= 0 {
		return time.Time{}, fmt.Errorf("binance server time is empty")
	}

	return time.UnixMilli(out.ServerTime), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Binance API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	serverTime, err := c.ServerTime()
	if err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Binance server time error: " + err.Error(),
			UpdatedAt: now,
		}, err
	}

	params := url.Values{}
	params.Set("timestamp", strconv.FormatInt(serverTime.UnixMilli(), 10))

	var rows []struct {
		AccountAlias       string `json:"accountAlias"`
		Asset              string `json:"asset"`
		Balance            string `json:"balance"`
		CrossWalletBalance string `json:"crossWalletBalance"`
		CrossUnPnl         string `json:"crossUnPnl"`
		AvailableBalance   string `json:"availableBalance"`
		MaxWithdrawAmount  string `json:"maxWithdrawAmount"`
		MarginAvailable    bool   `json:"marginAvailable"`
		UpdateTime         int64  `json:"updateTime"`
	}

	if err := c.signedGET("/fapi/v2/balance", params, &rows); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Binance futures balance error: " + err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	for _, r := range rows {
		if strings.EqualFold(r.Asset, "USDT") {
			wallet, _ := strconv.ParseFloat(r.Balance, 64)
			available, _ := strconv.ParseFloat(r.AvailableBalance, 64)

			return domain.Balance{
				Exchange:      c.Name(),
				WalletUSDT:    wallet,
				AvailableUSDT: available,
				PrivateOK:     true,
				Error:         "",
				UpdatedAt:     time.Now(),
			}, nil
		}
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "USDT futures balance not found",
		UpdatedAt: time.Now(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	var premium []struct {
		Symbol          string `json:"symbol"`
		MarkPrice       string `json:"markPrice"`
		IndexPrice      string `json:"indexPrice"`
		LastFundingRate string `json:"lastFundingRate"`
		NextFundingTime int64  `json:"nextFundingTime"`
	}

	if err := c.getJSON("/fapi/v1/premiumIndex", nil, &premium); err != nil {
		return nil, err
	}

	var tickers []struct {
		Symbol      string `json:"symbol"`
		LastPrice   string `json:"lastPrice"`
		QuoteVolume string `json:"quoteVolume"`
	}

	_ = c.getJSON("/fapi/v1/ticker/24hr", nil, &tickers)

	volumeBySymbol := map[string]float64{}
	lastBySymbol := map[string]float64{}

	for _, t := range tickers {
		v, _ := strconv.ParseFloat(t.QuoteVolume, 64)
		p, _ := strconv.ParseFloat(t.LastPrice, 64)

		volumeBySymbol[t.Symbol] = v
		lastBySymbol[t.Symbol] = p
	}

	bidBySymbol := map[string]float64{}
	askBySymbol := map[string]float64{}

	var books []struct {
		Symbol   string `json:"symbol"`
		BidPrice string `json:"bidPrice"`
		AskPrice string `json:"askPrice"`
	}

	_ = c.getJSON("/fapi/v1/ticker/bookTicker", nil, &books)

	for _, b := range books {
		bid, _ := strconv.ParseFloat(b.BidPrice, 64)
		ask, _ := strconv.ParseFloat(b.AskPrice, 64)

		bidBySymbol[b.Symbol] = bid
		askBySymbol[b.Symbol] = ask
	}

	out := make([]domain.Candidate, 0, len(premium))
	now := time.Now()

	for _, p := range premium {
		if !strings.HasSuffix(p.Symbol, "USDT") {
			continue
		}

		mark, _ := strconv.ParseFloat(p.MarkPrice, 64)
		fr, _ := strconv.ParseFloat(p.LastFundingRate, 64)

		price := lastBySymbol[p.Symbol]
		if price <= 0 {
			price = mark
		}

		bid := bidBySymbol[p.Symbol]
		ask := askBySymbol[p.Symbol]

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		nextFunding := time.Time{}
		if p.NextFundingTime > 0 {
			nextFunding = time.UnixMilli(p.NextFundingTime)
		}

		out = append(out, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               p.Symbol,
			NativeSymbol:         p.Symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          fr,
			FundingIntervalHours: 8,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        volumeBySymbol[p.Symbol],
			Bid:                  bid,
			Ask:                  ask,
			Spread:               spread,
			UpdatedAt:            now,
		})
	}

	return out, nil
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}

func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	if c.apiKey == "" || c.apiSecret == "" {
		return domain.OrderResult{}, fmt.Errorf("binance api keys are empty")
	}

	return domain.OrderResult{}, fmt.Errorf("live Binance order placement is scaffolded; enable after validation")
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
		return fmt.Errorf("binance GET %s: %s", path, string(body))
	}

	return json.Unmarshal(body, dst)
}

func (c *Client) signedGET(path string, params url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("binance api key or secret is empty")
	}

	params.Set("recvWindow", "10000")

	rawQuery := params.Encode()
	signature := c.sign(rawQuery)

	u := c.baseURL + path + "?" + rawQuery + "&signature=" + signature

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("X-MBX-APIKEY", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("binance signed GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("binance signed GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return hex.EncodeToString(mac.Sum(nil))
}

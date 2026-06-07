package kucoin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"funding-bot/internal/domain"
)

type Client struct {
	baseURL    string
	apiKey     string
	apiSecret  string
	passphrase string
	http       *http.Client
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		apiSecret:  strings.TrimSpace(apiSecret),
		passphrase: strings.TrimSpace(os.Getenv("KUCOIN_API_PASSPHRASE")),
		http:       &http.Client{Timeout: 12 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeName("KuCoin")
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Code string      `json:"code"`
		Data flexibleInt `json:"data"`
	}

	if err := c.getJSON("/api/v1/timestamp", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.Code != "" && out.Code != "200000" {
		return time.Time{}, fmt.Errorf("kucoin server time code=%s", out.Code)
	}

	if out.Data > 0 {
		return time.UnixMilli(int64(out.Data)), nil
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "KuCoin API key, secret or passphrase is empty",
			UpdatedAt: now,
		}, nil
	}

	q := url.Values{}
	q.Set("currency", "USDT")

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccountEquity    flexibleFloat `json:"accountEquity"`
			UnrealisedPNL    flexibleFloat `json:"unrealisedPNL"`
			MarginBalance    flexibleFloat `json:"marginBalance"`
			PositionMargin   flexibleFloat `json:"positionMargin"`
			OrderMargin      flexibleFloat `json:"orderMargin"`
			FrozenFunds      flexibleFloat `json:"frozenFunds"`
			AvailableBalance flexibleFloat `json:"availableBalance"`
			Currency         string        `json:"currency"`
		} `json:"data"`
	}

	if err := c.signedGET("/api/v1/account-overview", q, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "KuCoin futures balance error: " + err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	if out.Code != "" && out.Code != "200000" {
		err := fmt.Errorf("KuCoin balance code=%s msg=%s", out.Code, out.Msg)

		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	wallet := float64(out.Data.AccountEquity)
	if wallet <= 0 {
		wallet = float64(out.Data.MarginBalance)
	}

	available := float64(out.Data.AvailableBalance)

	return domain.Balance{
		Exchange:      c.Name(),
		WalletUSDT:    wallet,
		AvailableUSDT: available,
		PrivateOK:     true,
		Error:         "",
		UpdatedAt:     time.Now(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Symbol                  string        `json:"symbol"`
			RootSymbol              string        `json:"rootSymbol"`
			Type                    string        `json:"type"`
			QuoteCurrency           string        `json:"quoteCurrency"`
			BaseCurrency            string        `json:"baseCurrency"`
			Status                  string        `json:"status"`
			MarkPrice               flexibleFloat `json:"markPrice"`
			IndexPrice              flexibleFloat `json:"indexPrice"`
			LastTradePrice          flexibleFloat `json:"lastTradePrice"`
			FundingFeeRate          flexibleFloat `json:"fundingFeeRate"`
			PredictedFundingFeeRate flexibleFloat `json:"predictedFundingFeeRate"`
			FundingRateGranularity  flexibleInt   `json:"fundingRateGranularity"`
			FundingRateInterval     flexibleInt   `json:"fundingRateInterval"`
			FundingInterval         flexibleInt   `json:"fundingInterval"`
			NextFundingRateTime     flexibleInt   `json:"nextFundingRateTime"`
			NextFundingTime         flexibleInt   `json:"nextFundingTime"`
			VolumeOf24h             flexibleFloat `json:"volumeOf24h"`
			TurnoverOf24h           flexibleFloat `json:"turnoverOf24h"`
			BidPrice                flexibleFloat `json:"bidPrice"`
			AskPrice                flexibleFloat `json:"askPrice"`
			BestBidPrice            flexibleFloat `json:"bestBidPrice"`
			BestAskPrice            flexibleFloat `json:"bestAskPrice"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v1/contracts/active", nil, &out); err != nil {
		return nil, err
	}

	if out.Code != "" && out.Code != "200000" {
		return nil, fmt.Errorf("kucoin contracts code=%s msg=%s", out.Code, out.Msg)
	}

	now := time.Now().UTC()
	res := make([]domain.Candidate, 0, len(out.Data))

	for _, t := range out.Data {
		if t.QuoteCurrency != "USDT" && !strings.HasSuffix(t.Symbol, "USDTM") {
			continue
		}

		price := float64(t.LastTradePrice)
		if price <= 0 {
			price = float64(t.MarkPrice)
		}
		if price <= 0 {
			price = float64(t.IndexPrice)
		}

		mark := float64(t.MarkPrice)
		if mark <= 0 {
			mark = float64(t.IndexPrice)
		}
		if mark <= 0 {
			mark = price
		}

		fundingRate := float64(t.FundingFeeRate)
		if fundingRate == 0 {
			fundingRate = float64(t.PredictedFundingFeeRate)
		}

		intervalHours := extractIntervalHours(
			t.FundingRateGranularity,
			t.FundingRateInterval,
			t.FundingInterval,
		)
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextRaw := int64(t.NextFundingRateTime)
		if nextRaw <= 0 {
			nextRaw = int64(t.NextFundingTime)
		}

		nextFunding := parseKuCoinFundingTime(nextRaw)
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		vol := float64(t.TurnoverOf24h)
		if vol <= 0 {
			vol = float64(t.VolumeOf24h) * price
		}

		bid := float64(t.BidPrice)
		if bid <= 0 {
			bid = float64(t.BestBidPrice)
		}

		ask := float64(t.AskPrice)
		if ask <= 0 {
			ask = float64(t.BestAskPrice)
		}

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		symbol := strings.TrimSuffix(t.Symbol, "M")
		if symbol == "" {
			symbol = strings.ReplaceAll(t.Symbol, "-", "")
		}

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         t.Symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          fundingRate,
			FundingIntervalHours: intervalHours,
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

func parseKuCoinFundingTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}

	if v > 1_000_000_000_000 {
		return time.UnixMilli(v).UTC()
	}

	if v > 1_000_000_000 {
		return time.Unix(v, 0).UTC()
	}

	return time.Time{}
}

func extractIntervalHours(values ...flexibleInt) float64 {
	for _, raw := range values {
		v := int64(raw)
		if v <= 0 {
			continue
		}

		// KuCoin обычно отдаёт fundingRateGranularity в миллисекундах:
		// 3600000 / 14400000 / 28800000.
		if v >= 3600000 && v%3600000 == 0 {
			return float64(v) / 3600000
		}

		// Seconds.
		if v >= 3600 && v%3600 == 0 {
			return float64(v) / 3600
		}

		// Minutes.
		if v >= 60 && v <= 1440 && v%60 == 0 {
			return float64(v) / 60
		}

		// Hours.
		if v > 0 && v <= 24 {
			return float64(v)
		}
	}

	return 0
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
	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return domain.OrderResult{}, fmt.Errorf("kucoin api keys or passphrase are empty")
	}

	return domain.OrderResult{}, fmt.Errorf("kucoin live orders not implemented")
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
		return fmt.Errorf("kucoin GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("kucoin GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return fmt.Errorf("kucoin api key, secret or passphrase is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	query := q.Encode()
	requestPath := path
	if query != "" {
		requestPath += "?" + query
	}

	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	method := http.MethodGet
	body := ""

	payload := timestamp + method + requestPath + body
	signature := c.sign(payload)
	signedPassphrase := c.sign(c.passphrase)

	u := c.baseURL + requestPath

	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("KC-API-KEY", c.apiKey)
	req.Header.Set("KC-API-SIGN", signature)
	req.Header.Set("KC-API-TIMESTAMP", timestamp)
	req.Header.Set("KC-API-PASSPHRASE", signedPassphrase)
	req.Header.Set("KC-API-KEY-VERSION", "2")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("kucoin signed GET %s: %s", path, string(raw))
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("kucoin signed GET %s decode error: %w; body=%s", path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
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

	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
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

	v, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	if err != nil {
		*i = 0
		return nil
	}

	*i = flexibleInt(v)
	return nil
}

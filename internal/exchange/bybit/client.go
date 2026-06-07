package bybit

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
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
		http:      &http.Client{Timeout: 12 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeBybit
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Time    string `json:"time"`
		Result  struct {
			TimeSecond string `json:"timeSecond"`
			TimeNano   string `json:"timeNano"`
		} `json:"result"`
	}

	if err := c.getJSON("/v5/market/time", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.RetCode != 0 {
		return time.Time{}, fmt.Errorf("bybit server time retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	if out.Time != "" {
		ms, _ := strconv.ParseInt(out.Time, 10, 64)
		if ms > 0 {
			return time.UnixMilli(ms), nil
		}
	}

	sec, _ := strconv.ParseInt(out.Result.TimeSecond, 10, 64)
	if sec > 0 {
		return time.Unix(sec, 0), nil
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bybit API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	// Сначала пробуем UNIFIED, потому что на новых аккаунтах Bybit futures обычно находятся в UTA.
	balance, err := c.balanceByAccountType("UNIFIED")
	if err == nil && balance.PrivateOK {
		return balance, nil
	}

	// Для старых аккаунтов пробуем CONTRACT.
	contractBalance, contractErr := c.balanceByAccountType("CONTRACT")
	if contractErr == nil && contractBalance.PrivateOK {
		return contractBalance, nil
	}

	if err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bybit futures balance error: " + err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	if contractErr != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Bybit futures balance error: " + contractErr.Error(),
			UpdatedAt: time.Now(),
		}, contractErr
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "Bybit USDT futures balance not found",
		UpdatedAt: time.Now(),
	}, nil
}

func (c *Client) balanceByAccountType(accountType string) (domain.Balance, error) {
	q := url.Values{}
	q.Set("accountType", accountType)
	q.Set("coin", "USDT")

	var out struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			List []struct {
				AccountType           string        `json:"accountType"`
				TotalEquity           flexibleFloat `json:"totalEquity"`
				TotalWalletBalance    flexibleFloat `json:"totalWalletBalance"`
				TotalMarginBalance    flexibleFloat `json:"totalMarginBalance"`
				TotalAvailableBalance flexibleFloat `json:"totalAvailableBalance"`
				Coin                  []struct {
					Coin                string        `json:"coin"`
					Equity              flexibleFloat `json:"equity"`
					WalletBalance       flexibleFloat `json:"walletBalance"`
					AvailableToWithdraw flexibleFloat `json:"availableToWithdraw"`
					AvailableToBorrow   flexibleFloat `json:"availableToBorrow"`
					UsdValue            flexibleFloat `json:"usdValue"`
				} `json:"coin"`
			} `json:"list"`
		} `json:"result"`
	}

	if err := c.signedGET("/v5/account/wallet-balance", q, &out); err != nil {
		return domain.Balance{}, err
	}

	if out.RetCode != 0 {
		return domain.Balance{}, fmt.Errorf("Bybit balance accountType=%s retCode=%d retMsg=%s", accountType, out.RetCode, out.RetMsg)
	}

	for _, account := range out.Result.List {
		wallet := float64(account.TotalWalletBalance)
		if wallet <= 0 {
			wallet = float64(account.TotalEquity)
		}
		if wallet <= 0 {
			wallet = float64(account.TotalMarginBalance)
		}

		available := float64(account.TotalAvailableBalance)

		for _, coin := range account.Coin {
			if !strings.EqualFold(coin.Coin, "USDT") {
				continue
			}

			if wallet <= 0 {
				wallet = float64(coin.WalletBalance)
			}
			if wallet <= 0 {
				wallet = float64(coin.Equity)
			}
			if wallet <= 0 {
				wallet = float64(coin.UsdValue)
			}

			if available <= 0 {
				available = float64(coin.AvailableToWithdraw)
			}
			if available <= 0 {
				available = float64(coin.WalletBalance)
			}
			if available <= 0 {
				available = float64(coin.Equity)
			}

			return domain.Balance{
				Exchange:      c.Name(),
				WalletUSDT:    wallet,
				AvailableUSDT: available,
				PrivateOK:     true,
				Error:         "",
				UpdatedAt:     time.Now(),
			}, nil
		}

		// На UNIFIED Bybit иногда totalAvailableBalance есть, но coin list пустой при coin=USDT.
		if wallet > 0 || available > 0 {
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

	return domain.Balance{}, fmt.Errorf("Bybit USDT balance not found for accountType=%s", accountType)
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	intervalBySymbol := c.instrumentFundingIntervals()

	q := url.Values{}
	q.Set("category", "linear")

	var out struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
		Result  struct {
			List []struct {
				Symbol          string        `json:"symbol"`
				LastPrice       flexibleFloat `json:"lastPrice"`
				MarkPrice       flexibleFloat `json:"markPrice"`
				IndexPrice      flexibleFloat `json:"indexPrice"`
				FundingRate     flexibleFloat `json:"fundingRate"`
				NextFundingTime flexibleInt   `json:"nextFundingTime"`
				Volume24h       flexibleFloat `json:"volume24h"`
				Turnover24h     flexibleFloat `json:"turnover24h"`
				Bid1Price       flexibleFloat `json:"bid1Price"`
				Ask1Price       flexibleFloat `json:"ask1Price"`
			} `json:"list"`
		} `json:"result"`
	}

	if err := c.getJSON("/v5/market/tickers", q, &out); err != nil {
		return nil, err
	}

	if out.RetCode != 0 {
		return nil, fmt.Errorf("bybit tickers retCode=%d retMsg=%s", out.RetCode, out.RetMsg)
	}

	now := time.Now().UTC()
	res := make([]domain.Candidate, 0, len(out.Result.List))

	for _, t := range out.Result.List {
		if !strings.HasSuffix(t.Symbol, "USDT") {
			continue
		}

		price := float64(t.LastPrice)

		mark := float64(t.MarkPrice)
		if mark <= 0 {
			mark = float64(t.IndexPrice)
		}
		if mark <= 0 {
			mark = price
		}

		vol := float64(t.Turnover24h)
		if vol <= 0 {
			vol = float64(t.Volume24h) * price
		}

		bid := float64(t.Bid1Price)
		ask := float64(t.Ask1Price)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		intervalHours := intervalBySymbol[t.Symbol]
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextFunding := parseBybitFundingTime(int64(t.NextFundingTime))
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               t.Symbol,
			NativeSymbol:         t.Symbol,
			Price:                price,
			MarkPrice:            mark,
			FundingRate:          float64(t.FundingRate),
			FundingIntervalHours: intervalHours,
			NextFundingTime:      nextFunding,
			Volume24hUSDT:        vol,
			Bid:                  bid,
			Ask:                  ask,
			Spread:               spread,
			UpdatedAt:            now,
		})
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].FundingRate == res[j].FundingRate {
			return res[i].Volume24hUSDT > res[j].Volume24hUSDT
		}

		return res[i].FundingRate > res[j].FundingRate
	})

	return res, nil
}

func (c *Client) instrumentFundingIntervals() map[string]float64 {
	out := map[string]float64{}
	cursor := ""

	for {
		q := url.Values{}
		q.Set("category", "linear")
		q.Set("limit", "1000")

		if cursor != "" {
			q.Set("cursor", cursor)
		}

		var resp struct {
			RetCode int    `json:"retCode"`
			RetMsg  string `json:"retMsg"`
			Result  struct {
				NextPageCursor string `json:"nextPageCursor"`
				List           []struct {
					Symbol          string        `json:"symbol"`
					ContractType    string        `json:"contractType"`
					Status          string        `json:"status"`
					SettleCoin      string        `json:"settleCoin"`
					FundingInterval flexibleFloat `json:"fundingInterval"`
				} `json:"list"`
			} `json:"result"`
		}

		if err := c.getJSON("/v5/market/instruments-info", q, &resp); err != nil {
			fmt.Printf("Bybit instruments error: %v\n", err)
			return out
		}

		if resp.RetCode != 0 {
			fmt.Printf("Bybit instruments retCode=%d retMsg=%s\n", resp.RetCode, resp.RetMsg)
			return out
		}

		for _, item := range resp.Result.List {
			if item.Symbol == "" || !strings.HasSuffix(item.Symbol, "USDT") {
				continue
			}

			interval := extractIntervalHours(item.FundingInterval)
			if interval <= 0 {
				continue
			}

			out[item.Symbol] = interval
		}

		if resp.Result.NextPageCursor == "" || resp.Result.NextPageCursor == cursor {
			break
		}

		cursor = resp.Result.NextPageCursor
	}

	fmt.Printf("Bybit instrument intervals loaded: %d\n", len(out))

	return out
}

func parseBybitFundingTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}

	if v > 1_000_000_000_000 {
		return time.UnixMilli(v).UTC()
	}

	return time.Time{}
}

func extractIntervalHours(values ...flexibleFloat) float64 {
	for _, raw := range values {
		v := float64(raw)
		if v <= 0 {
			continue
		}

		// Bybit instruments-info обычно отдаёт fundingInterval в минутах: 60, 240, 480.
		if v >= 60 && v <= 1440 && int64(v)%60 == 0 {
			return v / 60
		}

		// Milliseconds.
		if v >= 3600000 && int64(v)%3600000 == 0 {
			return v / 3600000
		}

		// Seconds.
		if v >= 3600 && int64(v)%3600 == 0 {
			return v / 3600
		}

		// Hours.
		if v > 0 && v <= 24 {
			return v
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
	if c.apiKey == "" || c.apiSecret == "" {
		return domain.OrderResult{}, fmt.Errorf("bybit api keys are empty")
	}

	return domain.OrderResult{}, fmt.Errorf("live Bybit order placement is scaffolded; enable after validation")
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
		return fmt.Errorf("bybit GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("bybit GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("bybit api key or secret is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	recvWindow := "10000"
	query := q.Encode()
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)

	payload := timestamp + c.apiKey + recvWindow + query
	signature := c.sign(payload)

	u := c.baseURL + path
	if query != "" {
		u += "?" + query
	}

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("X-BAPI-API-KEY", c.apiKey)
	req.Header.Set("X-BAPI-SIGN", signature)
	req.Header.Set("X-BAPI-SIGN-TYPE", "2")
	req.Header.Set("X-BAPI-TIMESTAMP", timestamp)
	req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("bybit signed GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("bybit signed GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))

	return hex.EncodeToString(mac.Sum(nil))
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

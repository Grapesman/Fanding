package gate

import (
	"crypto/hmac"
	"crypto/sha512"
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

type gateContractInfo struct {
	Contract      string
	FundingRate   float64
	MarkPrice     float64
	IndexPrice    float64
	NextFunding   time.Time
	IntervalHours float64
	Volume24hUSDT float64
	FundingRateOK bool
	NextFundingOK bool
	IntervalOK    bool
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
	return domain.ExchangeGate
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var contract struct {
		Name     string `json:"name"`
		Contract string `json:"contract"`
	}

	if err := c.getJSON("/api/v4/futures/usdt/contracts/BTC_USDT", nil, &contract); err != nil {
		return time.Time{}, err
	}

	if contract.Name == "" && contract.Contract == "" {
		return time.Time{}, fmt.Errorf("gate connectivity check returned empty contract")
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Gate API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	var out struct {
		Total              flexibleFloat  `json:"total"`
		Available          flexibleFloat  `json:"available"`
		Currency           string         `json:"currency"`
		UnrealisedPNL      flexibleFloat  `json:"unrealised_pnl"`
		UnrealizedPNL      flexibleFloat  `json:"unrealized_pnl"`
		PositionMargin     flexibleFloat  `json:"position_margin"`
		OrderMargin        flexibleFloat  `json:"order_margin"`
		Point              flexibleFloat  `json:"point"`
		Bonus              flexibleFloat  `json:"bonus"`
		History            map[string]any `json:"history"`
		TotalInitial       flexibleFloat  `json:"total_initial_margin"`
		TotalMaint         flexibleFloat  `json:"total_maintenance_margin"`
		TotalMargin        flexibleFloat  `json:"total_margin_balance"`
		TotalAvailable     flexibleFloat  `json:"total_available_balance"`
		AvailableMargin    flexibleFloat  `json:"available_margin"`
		CrossAvailable     flexibleFloat  `json:"cross_available"`
		CrossMarginBalance flexibleFloat  `json:"cross_margin_balance"`
		PositionMode       string         `json:"position_mode"`
		MarginModeName     string         `json:"margin_mode_name"`
	}

	if err := c.signedGET("/api/v4/futures/usdt/accounts", nil, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "Gate futures balance error: " + err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	wallet := float64(out.Total)
	if wallet <= 0 {
		wallet = float64(out.TotalMargin)
	}
	if wallet <= 0 {
		wallet = float64(out.CrossMarginBalance)
	}
	if wallet <= 0 {
		wallet = float64(out.TotalAvailable)
	}

	available := float64(out.Available)
	if available <= 0 {
		available = float64(out.AvailableMargin)
	}
	if available <= 0 {
		available = float64(out.CrossAvailable)
	}
	if available <= 0 {
		available = float64(out.TotalAvailable)
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

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	infoByContract := c.contractInfoMap()

	var tickers []struct {
		Contract              string        `json:"contract"`
		Last                  flexibleFloat `json:"last"`
		MarkPrice             flexibleFloat `json:"mark_price"`
		IndexPrice            flexibleFloat `json:"index_price"`
		FundingRate           flexibleFloat `json:"funding_rate"`
		FundingRateIndicative flexibleFloat `json:"funding_rate_indicative"`
		FundingInterval       flexibleInt   `json:"funding_interval"`
		FundingNextApply      flexibleInt   `json:"funding_next_apply"`
		NextFundingTime       flexibleInt   `json:"next_funding_time"`
		Volume24h             flexibleFloat `json:"volume_24h"`
		Volume24hQuote        flexibleFloat `json:"volume_24h_quote"`
		QuantoBaseRate        flexibleFloat `json:"quanto_base_rate"`
		LowestAsk             flexibleFloat `json:"lowest_ask"`
		HighestBid            flexibleFloat `json:"highest_bid"`
	}

	if err := c.getJSON("/api/v4/futures/usdt/tickers", nil, &tickers); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	res := make([]domain.Candidate, 0, len(tickers))

	for _, t := range tickers {
		if t.Contract == "" || !strings.HasSuffix(t.Contract, "_USDT") {
			continue
		}

		info := infoByContract[t.Contract]

		price := float64(t.Last)

		mark := float64(t.MarkPrice)
		if mark <= 0 {
			mark = info.MarkPrice
		}
		if mark <= 0 {
			mark = float64(t.IndexPrice)
		}
		if mark <= 0 {
			mark = info.IndexPrice
		}
		if mark <= 0 {
			mark = price
		}

		fundingRate := float64(t.FundingRate)
		if fundingRate == 0 {
			fundingRate = float64(t.FundingRateIndicative)
		}
		if fundingRate == 0 && info.FundingRateOK {
			fundingRate = info.FundingRate
		}

		intervalHours := extractIntervalHoursFromSeconds(t.FundingInterval)
		if intervalHours <= 0 && info.IntervalOK {
			intervalHours = info.IntervalHours
		}
		if intervalHours <= 0 {
			intervalHours = 8
		}

		nextRaw := int64(t.FundingNextApply)
		if nextRaw <= 0 {
			nextRaw = int64(t.NextFundingTime)
		}

		nextFunding := parseGateFundingTime(nextRaw)
		if nextFunding.IsZero() && info.NextFundingOK {
			nextFunding = info.NextFunding
		}
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, int(intervalHours))
		}

		vol := float64(t.Volume24hQuote)
		if vol <= 0 && info.Volume24hUSDT > 0 {
			vol = info.Volume24hUSDT
		}
		if vol <= 0 {
			vol = float64(t.Volume24h) * price
		}

		bid := float64(t.HighestBid)
		ask := float64(t.LowestAsk)

		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               strings.ReplaceAll(t.Contract, "_", ""),
			NativeSymbol:         t.Contract,
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

func (c *Client) contractInfoMap() map[string]gateContractInfo {
	out := map[string]gateContractInfo{}

	var contracts []struct {
		Name                  string        `json:"name"`
		Contract              string        `json:"contract"`
		Type                  string        `json:"type"`
		InDelisting           bool          `json:"in_delisting"`
		MarkPrice             flexibleFloat `json:"mark_price"`
		IndexPrice            flexibleFloat `json:"index_price"`
		LastPrice             flexibleFloat `json:"last_price"`
		FundingRate           flexibleFloat `json:"funding_rate"`
		FundingRateIndicative flexibleFloat `json:"funding_rate_indicative"`
		FundingInterval       flexibleInt   `json:"funding_interval"`
		FundingNextApply      flexibleInt   `json:"funding_next_apply"`
		NextFundingTime       flexibleInt   `json:"next_funding_time"`
		Volume24h             flexibleFloat `json:"volume_24h"`
		Volume24hQuote        flexibleFloat `json:"volume_24h_quote"`
		QuantoBaseRate        flexibleFloat `json:"quanto_base_rate"`
	}

	if err := c.getJSON("/api/v4/futures/usdt/contracts", nil, &contracts); err != nil {
		fmt.Printf("Gate contracts error: %v\n", err)
		return out
	}

	for _, item := range contracts {
		contract := firstNonEmpty(item.Contract, item.Name)
		if contract == "" || !strings.HasSuffix(contract, "_USDT") {
			continue
		}

		fundingRate := float64(item.FundingRate)
		if fundingRate == 0 {
			fundingRate = float64(item.FundingRateIndicative)
		}

		intervalHours := extractIntervalHoursFromSeconds(item.FundingInterval)

		nextRaw := int64(item.FundingNextApply)
		if nextRaw <= 0 {
			nextRaw = int64(item.NextFundingTime)
		}

		nextFunding := parseGateFundingTime(nextRaw)

		vol := float64(item.Volume24hQuote)
		if vol <= 0 {
			vol = float64(item.Volume24h) * float64(item.LastPrice)
		}
		if vol <= 0 {
			vol = float64(item.Volume24h) * float64(item.MarkPrice)
		}

		out[contract] = gateContractInfo{
			Contract:      contract,
			FundingRate:   fundingRate,
			MarkPrice:     float64(item.MarkPrice),
			IndexPrice:    float64(item.IndexPrice),
			NextFunding:   nextFunding,
			IntervalHours: intervalHours,
			Volume24hUSDT: vol,
			FundingRateOK: fundingRate != 0,
			NextFundingOK: !nextFunding.IsZero(),
			IntervalOK:    intervalHours > 0,
		}
	}

	fmt.Printf("Gate contract info loaded: %d\n", len(out))

	return out
}

func parseGateFundingTime(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}

	// Unix milliseconds.
	if v > 1_000_000_000_000 {
		return time.UnixMilli(v).UTC()
	}

	// Unix seconds.
	if v > 1_000_000_000 {
		return time.Unix(v, 0).UTC()
	}

	return time.Time{}
}

func extractIntervalHoursFromSeconds(v flexibleInt) float64 {
	raw := int64(v)
	if raw <= 0 {
		return 0
	}

	// Gate usually returns funding_interval in seconds: 3600 / 14400 / 28800.
	if raw >= 3600 && raw%3600 == 0 {
		return float64(raw) / 3600
	}

	// Fallback if an API variant returns hours directly.
	if raw > 0 && raw <= 24 {
		return float64(raw)
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

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

func (c *Client) SetMarginAndLeverage(symbol string, leverage int, marginMode string) error {
	return nil
}

func (c *Client) PlaceOrder(req domain.OrderRequest) (domain.OrderResult, error) {
	if c.apiKey == "" || c.apiSecret == "" {
		return domain.OrderResult{}, fmt.Errorf("gate api keys are empty")
	}

	return domain.OrderResult{}, fmt.Errorf("live Gate order placement is scaffolded; enable after validation")
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

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("gate GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" {
		return fmt.Errorf("gate api key or secret is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	query := q.Encode()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	body := ""

	bodyHash := sha512.Sum512([]byte(body))
	bodyHashHex := hex.EncodeToString(bodyHash[:])

	payload := http.MethodGet + "\n" +
		path + "\n" +
		query + "\n" +
		bodyHashHex + "\n" +
		timestamp

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
	req.Header.Set("KEY", c.apiKey)
	req.Header.Set("Timestamp", timestamp)
	req.Header.Set("SIGN", signature)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("gate signed GET %s: %s", path, string(raw))
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("gate signed GET %s decode error: %w; body=%s", path, err, string(raw))
	}

	return nil
}

func (c *Client) sign(payload string) string {
	mac := hmac.New(sha512.New, []byte(c.apiSecret))
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

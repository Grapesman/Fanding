package htx

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
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

type htxFundingInfo struct {
	Rate          float64
	NextFunding   time.Time
	IntervalHours float64
	OK            bool
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{
		baseURL:   strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:    strings.TrimSpace(apiKey),
		apiSecret: strings.TrimSpace(apiSecret),
		http:      &http.Client{Timeout: 15 * time.Second},
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
		Status string      `json:"status"`
		TS     flexibleInt `json:"ts"`
	}

	if err := c.getJSON("/linear-swap-api/v1/swap_contract_info?contract_code=BTC-USDT", &out); err != nil {
		return time.Time{}, err
	}

	if out.Status != "" && out.Status != "ok" {
		return time.Time{}, fmt.Errorf("htx server time check status=%s", out.Status)
	}

	if out.TS > 0 {
		return time.UnixMilli(int64(out.TS)), nil
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "HTX API key or secret is empty",
			UpdatedAt: now,
		}, nil
	}

	endpoints := []string{
		"/linear-swap-api/v3/unified_account_info",
		"/linear-swap-api/v1/swap_cross_account_info",
		"/linear-swap-api/v1/swap_account_info",
	}

	var lastErr error

	for _, endpoint := range endpoints {
		balance, err := c.balanceFromEndpoint(endpoint)
		if err == nil && balance.PrivateOK {
			return balance, nil
		}

		if err != nil {
			lastErr = err
		}
	}

	if lastErr != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "HTX futures balance error: " + lastErr.Error(),
			UpdatedAt: time.Now(),
		}, lastErr
	}

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "HTX USDT futures balance not found",
		UpdatedAt: time.Now(),
	}, nil
}

func (c *Client) balanceFromEndpoint(endpoint string) (domain.Balance, error) {
	raw, err := c.signedPOSTRaw(endpoint, map[string]any{})
	if err != nil {
		return domain.Balance{}, err
	}

	var envelope struct {
		Status  string          `json:"status"`
		ErrMsg  string          `json:"err_msg"`
		ErrMsg2 string          `json:"err-msg"`
		ErrCode flexibleInt     `json:"err_code"`
		TS      flexibleInt     `json:"ts"`
		Data    json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		return domain.Balance{}, fmt.Errorf("HTX balance decode error endpoint=%s: %w; body=%s", endpoint, err, string(raw))
	}

	if envelope.Status != "" && envelope.Status != "ok" {
		msg := envelope.ErrMsg
		if msg == "" {
			msg = envelope.ErrMsg2
		}
		if msg == "" {
			msg = string(raw)
		}
		return domain.Balance{}, fmt.Errorf("HTX balance endpoint=%s status=%s code=%d msg=%s", endpoint, envelope.Status, envelope.ErrCode, msg)
	}

	wallet, available, ok := parseHTXUSDTBalance(envelope.Data)
	if !ok {
		return domain.Balance{}, fmt.Errorf("HTX USDT balance not found endpoint=%s body=%s", endpoint, string(raw))
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

func parseHTXUSDTBalance(data json.RawMessage) (float64, float64, bool) {
	if len(data) == 0 || string(data) == "null" {
		return 0, 0, false
	}

	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err == nil {
		walletSum := 0.0
		availableSum := 0.0
		found := false

		for _, row := range rows {
			if !isHTXUSDTBalanceRow(row) {
				continue
			}

			wallet := firstMapFloat(row,
				"margin_balance",
				"margin_static",
				"account_balance",
				"balance",
				"total",
				"total_balance",
				"total_margin_balance",
			)

			available := firstMapFloat(row,
				"withdraw_available",
				"margin_available",
				"available",
				"available_balance",
				"transferable",
				"total_available_balance",
			)

			// Если API не отдаёт отдельный available, безопаснее показать wallet,
			// но торговая логика позже всё равно будет проверять AvailableUSDT.
			if available <= 0 {
				available = firstMapFloat(row,
					"margin_available",
					"withdraw_available",
					"available",
				)
			}

			walletSum += wallet
			availableSum += available
			found = true
		}

		if found {
			return walletSum, availableSum, true
		}
	}

	var row map[string]any
	if err := json.Unmarshal(data, &row); err == nil {
		if nested, ok := row["list"]; ok {
			if raw, err := json.Marshal(nested); err == nil {
				return parseHTXUSDTBalance(raw)
			}
		}

		if nested, ok := row["assets"]; ok {
			if raw, err := json.Marshal(nested); err == nil {
				return parseHTXUSDTBalance(raw)
			}
		}

		if nested, ok := row["data"]; ok {
			if raw, err := json.Marshal(nested); err == nil {
				return parseHTXUSDTBalance(raw)
			}
		}

		if isHTXUSDTBalanceRow(row) {
			wallet := firstMapFloat(row,
				"margin_balance",
				"margin_static",
				"account_balance",
				"balance",
				"total",
				"total_balance",
				"total_margin_balance",
			)

			available := firstMapFloat(row,
				"withdraw_available",
				"margin_available",
				"available",
				"available_balance",
				"transferable",
				"total_available_balance",
			)

			return wallet, available, true
		}
	}

	return 0, 0, false
}

func isHTXUSDTBalanceRow(row map[string]any) bool {
	for _, key := range []string{"margin_account", "margin_asset", "currency", "asset", "symbol"} {
		v := strings.ToUpper(fmt.Sprint(row[key]))
		if v == "USDT" || v == "USD" {
			return true
		}
	}

	contractCode := strings.ToUpper(fmt.Sprint(row["contract_code"]))
	if strings.HasSuffix(contractCode, "-USDT") {
		return true
	}

	// unified_account_info иногда отдаёт только числовые account-поля без явного currency.
	if firstMapFloat(row, "margin_balance", "margin_static", "withdraw_available", "margin_available") > 0 {
		return true
	}

	return false
}

func firstMapFloat(row map[string]any, keys ...string) float64 {
	for _, key := range keys {
		v, ok := row[key]
		if !ok || v == nil {
			continue
		}

		switch x := v.(type) {
		case float64:
			if x != 0 {
				return x
			}
		case int:
			if x != 0 {
				return float64(x)
			}
		case int64:
			if x != 0 {
				return float64(x)
			}
		case string:
			n, _ := strconv.ParseFloat(strings.ReplaceAll(x, ",", ""), 64)
			if n != 0 {
				return n
			}
		case json.Number:
			n, _ := x.Float64()
			if n != 0 {
				return n
			}
		}
	}

	return 0
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

	fundingBySymbol := c.fundingInfoMap()

	now := time.Now().UTC()
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

		funding := fundingBySymbol[t.ContractCode]

		nextFunding := funding.NextFunding
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, 8)
		}

		intervalHours := funding.IntervalHours
		if intervalHours <= 0 {
			intervalHours = inferIntervalFromNextFunding(now, nextFunding)
		}
		if intervalHours <= 0 {
			intervalHours = 8
		}

		symbol := strings.ReplaceAll(t.ContractCode, "-", "")

		res = append(res, domain.Candidate{
			Exchange:             c.Name(),
			Symbol:               symbol,
			NativeSymbol:         t.ContractCode,
			Price:                price,
			MarkPrice:            price,
			FundingRate:          funding.Rate,
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

func (c *Client) fundingInfoMap() map[string]htxFundingInfo {
	out := map[string]htxFundingInfo{}

	var fund struct {
		Status string `json:"status"`
		Data   []struct {
			ContractCode         string        `json:"contract_code"`
			FundingRate          flexibleFloat `json:"funding_rate"`
			EstimatedRate        flexibleFloat `json:"estimated_rate"`
			FundingTime          flexibleInt   `json:"funding_time"`
			NextFundingTime      flexibleInt   `json:"next_funding_time"`
			SettlementTime       flexibleInt   `json:"settlement_time"`
			FundingIntervalHours flexibleFloat `json:"funding_interval_hours"`
			FundingInterval      flexibleFloat `json:"funding_interval"`
			FundingRateInterval  flexibleFloat `json:"funding_rate_interval"`
			Interval             flexibleFloat `json:"interval"`
		} `json:"data"`
	}

	if err := c.getJSON("/linear-swap-api/v1/swap_batch_funding_rate", &fund); err != nil {
		fmt.Printf("HTX funding batch error: %v\n", err)
		return out
	}

	if fund.Status != "" && fund.Status != "ok" {
		fmt.Printf("HTX funding batch status: %s\n", fund.Status)
		return out
	}

	now := time.Now().UTC()

	for _, f := range fund.Data {
		if f.ContractCode == "" {
			continue
		}

		rate := float64(f.FundingRate)
		if rate == 0 {
			rate = float64(f.EstimatedRate)
		}

		nextRaw := int64(f.NextFundingTime)
		if nextRaw <= 0 {
			nextRaw = int64(f.FundingTime)
		}
		if nextRaw <= 0 {
			nextRaw = int64(f.SettlementTime)
		}

		nextFunding := parseHTXFundingTime(nextRaw)
		if nextFunding.IsZero() {
			nextFunding = nextFundingByInterval(now, 8)
		}

		intervalHours := extractIntervalHours(
			f.FundingIntervalHours,
			f.FundingInterval,
			f.FundingRateInterval,
			f.Interval,
		)

		if intervalHours <= 0 {
			intervalHours = inferIntervalFromNextFunding(now, nextFunding)
		}
		if intervalHours <= 0 {
			intervalHours = 8
		}

		out[f.ContractCode] = htxFundingInfo{
			Rate:          rate,
			NextFunding:   nextFunding,
			IntervalHours: intervalHours,
			OK:            true,
		}
	}

	fmt.Printf("HTX funding info loaded: %d\n", len(out))

	return out
}

func parseHTXFundingTime(v int64) time.Time {
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

func extractIntervalHours(values ...flexibleFloat) float64 {
	for _, raw := range values {
		v := float64(raw)
		if v <= 0 {
			continue
		}

		if v >= 3600000 && int64(v)%3600000 == 0 {
			return v / 3600000
		}

		if v >= 3600 && int64(v)%3600 == 0 {
			return v / 3600
		}

		if v >= 60 && v <= 1440 && int64(v)%60 == 0 {
			return v / 60
		}

		if v > 0 && v <= 24 {
			return v
		}
	}

	return 0
}

func inferIntervalFromNextFunding(now time.Time, next time.Time) float64 {
	if next.IsZero() {
		return 0
	}

	next = next.UTC()

	if next.Minute() != 0 || next.Second() != 0 {
		return 0
	}

	h := next.Hour()

	if h%4 != 0 {
		return 1
	}

	if h%8 != 0 {
		return 4
	}

	return 8
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
		return domain.OrderResult{}, fmt.Errorf("htx api keys are empty")
	}

	return domain.OrderResult{}, fmt.Errorf("live HTX order placement is scaffolded; enable after validation")
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

func (c *Client) getJSON(pathWithQuery string, dst any) error {
	u := c.baseURL + pathWithQuery

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
		return fmt.Errorf("htx GET %s: %s", pathWithQuery, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("htx GET %s decode error: %w; body=%s", pathWithQuery, err, string(body))
	}

	return nil
}

func (c *Client) signedPOSTRaw(path string, body any) ([]byte, error) {
	if c.apiKey == "" || c.apiSecret == "" {
		return nil, fmt.Errorf("htx api key or secret is empty")
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	if len(bodyBytes) == 0 || string(bodyBytes) == "null" {
		bodyBytes = []byte("{}")
	}

	parsedBase, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, err
	}

	authParams := url.Values{}
	authParams.Set("AccessKeyId", c.apiKey)
	authParams.Set("SignatureMethod", "HmacSHA256")
	authParams.Set("SignatureVersion", "2")
	authParams.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05"))

	signature := c.signHTX(http.MethodPost, parsedBase.Host, path, authParams)
	authParams.Set("Signature", signature)

	u := c.baseURL + path + "?" + authParams.Encode()

	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("htx signed POST %s: %s", path, string(raw))
	}

	return raw, nil
}

func (c *Client) signHTX(method, host, path string, params url.Values) string {
	payload := strings.Join([]string{
		method,
		host,
		path,
		params.Encode(),
	}, "\n")

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

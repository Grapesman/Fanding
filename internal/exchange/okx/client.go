package okx

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
	"sort"
	"strconv"
	"strings"
	"sync"
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

type okxTickerRow struct {
	InstID    string
	Last      string
	VolCcy24h string
	Vol24h    string
	AskPx     string
	BidPx     string
}

type okxFundingInfo struct {
	InstID        string
	FundingRate   float64
	NextFunding   time.Time
	IntervalHours float64
	OK            bool
}

func New(baseURL, apiKey, apiSecret string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		apiSecret:  strings.TrimSpace(apiSecret),
		passphrase: strings.TrimSpace(os.Getenv("OKX_API_PASSPHRASE")),
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Name() domain.ExchangeName {
	return domain.ExchangeName("OKX")
}

func (c *Client) Connected() bool {
	_, err := c.ServerTime()
	return err == nil
}

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			Ts flexibleInt `json:"ts"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v5/public/time", nil, &out); err != nil {
		return time.Time{}, err
	}

	if out.Code != "" && out.Code != "0" {
		return time.Time{}, fmt.Errorf("okx server time code=%s msg=%s", out.Code, out.Msg)
	}

	if len(out.Data) > 0 && out.Data[0].Ts > 0 {
		return time.UnixMilli(int64(out.Data[0].Ts)), nil
	}

	return time.Now(), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	now := time.Now()

	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "OKX API key, secret or passphrase is empty",
			UpdatedAt: now,
		}, nil
	}

	q := url.Values{}
	q.Set("ccy", "USDT")

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			TotalEq flexibleFloat `json:"totalEq"`
			AdjEq   flexibleFloat `json:"adjEq"`
			Details []struct {
				Ccy       string        `json:"ccy"`
				Eq        flexibleFloat `json:"eq"`
				CashBal   flexibleFloat `json:"cashBal"`
				AvailBal  flexibleFloat `json:"availBal"`
				AvailEq   flexibleFloat `json:"availEq"`
				FrozenBal flexibleFloat `json:"frozenBal"`
				Upl       flexibleFloat `json:"upl"`
				EqUsd     flexibleFloat `json:"eqUsd"`
			} `json:"details"`
		} `json:"data"`
	}

	if err := c.signedGET("/api/v5/account/balance", q, &out); err != nil {
		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     "OKX futures balance error: " + err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	if out.Code != "" && out.Code != "0" {
		err := fmt.Errorf("OKX balance code=%s msg=%s", out.Code, out.Msg)

		return domain.Balance{
			Exchange:  c.Name(),
			PrivateOK: false,
			Error:     err.Error(),
			UpdatedAt: time.Now(),
		}, err
	}

	for _, account := range out.Data {
		wallet := float64(account.TotalEq)
		if wallet <= 0 {
			wallet = float64(account.AdjEq)
		}

		available := 0.0

		for _, d := range account.Details {
			if !strings.EqualFold(d.Ccy, "USDT") {
				continue
			}

			if wallet <= 0 {
				wallet = float64(d.Eq)
			}
			if wallet <= 0 {
				wallet = float64(d.EqUsd)
			}
			if wallet <= 0 {
				wallet = float64(d.CashBal)
			}

			available = float64(d.AvailBal)
			if available <= 0 {
				available = float64(d.AvailEq)
			}
			if available <= 0 {
				available = float64(d.CashBal)
			}
			if available <= 0 {
				available = float64(d.Eq)
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

	return domain.Balance{
		Exchange:  c.Name(),
		PrivateOK: false,
		Error:     "OKX USDT balance not found",
		UpdatedAt: time.Now(),
	}, nil
}

func (c *Client) FundingCandidates() ([]domain.Candidate, error) {
	q := url.Values{}
	q.Set("instType", "SWAP")

	var tick struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID    string `json:"instId"`
			Last      string `json:"last"`
			VolCcy24h string `json:"volCcy24h"`
			Vol24h    string `json:"vol24h"`
			AskPx     string `json:"askPx"`
			BidPx     string `json:"bidPx"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v5/market/tickers", q, &tick); err != nil {
		return nil, err
	}

	if tick.Code != "" && tick.Code != "0" {
		return nil, fmt.Errorf("okx tickers code=%s msg=%s", tick.Code, tick.Msg)
	}

	rows := make([]okxTickerRow, 0, len(tick.Data))

	for _, t := range tick.Data {
		if !strings.HasSuffix(t.InstID, "USDT-SWAP") {
			continue
		}

		rows = append(rows, okxTickerRow{
			InstID:    t.InstID,
			Last:      t.Last,
			VolCcy24h: t.VolCcy24h,
			Vol24h:    t.Vol24h,
			AskPx:     t.AskPx,
			BidPx:     t.BidPx,
		})
	}

	now := time.Now().UTC()
	fundingByInst := c.fetchFundingMap(rows, now)

	res := make([]domain.Candidate, 0, len(rows))

	for _, row := range rows {
		res = append(res, c.buildCandidate(row, now, fundingByInst))
	}

	sort.Slice(res, func(i, j int) bool {
		if res[i].FundingRate == res[j].FundingRate {
			return res[i].Volume24hUSDT > res[j].Volume24hUSDT
		}

		return res[i].FundingRate > res[j].FundingRate
	})

	return res, nil
}

func (c *Client) buildCandidate(row okxTickerRow, now time.Time, fundingByInst map[string]okxFundingInfo) domain.Candidate {
	price, _ := strconv.ParseFloat(row.Last, 64)

	vol, _ := strconv.ParseFloat(row.VolCcy24h, 64)
	if vol <= 0 {
		v, _ := strconv.ParseFloat(row.Vol24h, 64)
		vol = v * price
	}

	bid, _ := strconv.ParseFloat(row.BidPx, 64)
	ask, _ := strconv.ParseFloat(row.AskPx, 64)

	spread := 0.0
	if bid > 0 && ask > 0 {
		spread = (ask - bid) / ((ask + bid) / 2)
	}

	funding := fundingByInst[row.InstID]

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

	symbol := strings.ReplaceAll(strings.TrimSuffix(row.InstID, "-SWAP"), "-", "")

	return domain.Candidate{
		Exchange:             c.Name(),
		Symbol:               symbol,
		NativeSymbol:         row.InstID,
		Price:                price,
		MarkPrice:            price,
		FundingRate:          funding.FundingRate,
		FundingIntervalHours: intervalHours,
		NextFundingTime:      nextFunding,
		Volume24hUSDT:        vol,
		Bid:                  bid,
		Ask:                  ask,
		Spread:               spread,
		UpdatedAt:            now,
	}
}

func (c *Client) fetchFundingMap(rows []okxTickerRow, now time.Time) map[string]okxFundingInfo {
	out := map[string]okxFundingInfo{}

	type result struct {
		instID string
		info   okxFundingInfo
		ok     bool
	}

	jobs := make(chan string)
	results := make(chan result)

	workerCount := 20

	var wg sync.WaitGroup

	for i := 0; i < workerCount; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for instID := range jobs {
				info, ok := c.fetchFundingOne(instID, now)
				results <- result{
					instID: instID,
					info:   info,
					ok:     ok,
				}
			}
		}()
	}

	go func() {
		for _, row := range rows {
			jobs <- row.InstID
		}

		close(jobs)
		wg.Wait()
		close(results)
	}()

	for r := range results {
		if r.ok {
			out[r.instID] = r.info
		}
	}

	fmt.Printf("OKX funding info loaded: %d, rows: %d\n", len(out), len(rows))

	return out
}

func (c *Client) fetchFundingOne(instID string, now time.Time) (okxFundingInfo, bool) {
	q := url.Values{}
	q.Set("instId", instID)

	var out struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			InstID          string        `json:"instId"`
			FundingRate     flexibleFloat `json:"fundingRate"`
			NextFundingTime flexibleInt   `json:"nextFundingTime"`
			FundingTime     flexibleInt   `json:"fundingTime"`
			FundingInterval flexibleFloat `json:"fundingInterval"`
			SettFundingRate flexibleFloat `json:"settFundingRate"`
			MinFundingRate  flexibleFloat `json:"minFundingRate"`
			MaxFundingRate  flexibleFloat `json:"maxFundingRate"`
		} `json:"data"`
	}

	if err := c.getJSON("/api/v5/public/funding-rate", q, &out); err != nil {
		return okxFundingInfo{
			InstID:        instID,
			NextFunding:   nextFundingByInterval(now, 8),
			IntervalHours: 8,
			OK:            false,
		}, false
	}

	if out.Code != "" && out.Code != "0" {
		return okxFundingInfo{
			InstID:        instID,
			NextFunding:   nextFundingByInterval(now, 8),
			IntervalHours: 8,
			OK:            false,
		}, false
	}

	if len(out.Data) == 0 {
		return okxFundingInfo{
			InstID:        instID,
			NextFunding:   nextFundingByInterval(now, 8),
			IntervalHours: 8,
			OK:            false,
		}, false
	}

	d := out.Data[0]

	rate := float64(d.FundingRate)
	if rate == 0 {
		rate = float64(d.SettFundingRate)
	}

	nextFunding := parseOKXFundingTime(int64(d.NextFundingTime))
	if nextFunding.IsZero() {
		nextFunding = parseOKXFundingTime(int64(d.FundingTime))
	}

	intervalHours := extractIntervalHours(d.FundingInterval)

	if intervalHours <= 0 {
		// OKX часто не отдаёт явное поле interval, но fundingTime и nextFundingTime
		// позволяют вычислить интервал.
		fundingTime := parseOKXFundingTime(int64(d.FundingTime))
		if !fundingTime.IsZero() && !nextFunding.IsZero() && nextFunding.After(fundingTime) {
			diff := nextFunding.Sub(fundingTime).Hours()
			if diff > 0 && diff <= 24 {
				intervalHours = diff
			}
		}
	}

	if intervalHours <= 0 {
		intervalHours = inferIntervalFromNextFunding(now, nextFunding)
	}

	if intervalHours <= 0 {
		intervalHours = 8
	}

	if nextFunding.IsZero() {
		nextFunding = nextFundingByInterval(now, int(intervalHours))
	}

	return okxFundingInfo{
		InstID:        instID,
		FundingRate:   rate,
		NextFunding:   nextFunding,
		IntervalHours: intervalHours,
		OK:            true,
	}, true
}

func parseOKXFundingTime(v int64) time.Time {
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
	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return domain.OrderResult{}, fmt.Errorf("okx api keys or passphrase are empty")
	}

	return domain.OrderResult{}, fmt.Errorf("okx live orders not implemented")
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
		return fmt.Errorf("okx GET %s: %s", path, string(body))
	}

	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("okx GET %s decode error: %w; body=%s", path, err, string(body))
	}

	return nil
}

func (c *Client) signedGET(path string, q url.Values, dst any) error {
	if c.apiKey == "" || c.apiSecret == "" || c.passphrase == "" {
		return fmt.Errorf("okx api key, secret or passphrase is empty")
	}

	if q == nil {
		q = url.Values{}
	}

	query := q.Encode()

	requestPath := path
	if query != "" {
		requestPath += "?" + query
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	method := http.MethodGet
	body := ""

	payload := timestamp + method + requestPath + body
	signature := c.sign(payload)

	u := c.baseURL + requestPath

	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 funding-bot/1.0")
	req.Header.Set("OK-ACCESS-KEY", c.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", signature)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", c.passphrase)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		return fmt.Errorf("okx signed GET %s: %s", path, string(raw))
	}

	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("okx signed GET %s decode error: %w; body=%s", path, err, string(raw))
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

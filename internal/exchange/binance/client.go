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
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, apiSecret: apiSecret, http: &http.Client{Timeout: 8 * time.Second}}
}

func (c *Client) Name() domain.ExchangeName { return domain.ExchangeBinance }
func (c *Client) Connected() bool           { _, err := c.ServerTime(); return err == nil }

func (c *Client) ServerTime() (time.Time, error) {
	var out struct {
		ServerTime int64 `json:"serverTime"`
	}
	if err := c.getJSON("/fapi/v1/time", nil, &out); err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(out.ServerTime), nil
}

func (c *Client) Balance() (domain.Balance, error) {
	// Signed account endpoints are intentionally conservative. If keys are missing, return zero balance.
	if c.apiKey == "" || c.apiSecret == "" {
		return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
	}
	params := url.Values{}
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	var rows []struct {
		Asset            string `json:"asset"`
		Balance          string `json:"balance"`
		AvailableBalance string `json:"availableBalance"`
	}
	if err := c.signedGET("/fapi/v2/balance", params, &rows); err != nil {
		return domain.Balance{}, err
	}
	for _, r := range rows {
		if r.Asset == "USDT" {
			w, _ := strconv.ParseFloat(r.Balance, 64)
			a, _ := strconv.ParseFloat(r.AvailableBalance, 64)
			return domain.Balance{Exchange: c.Name(), WalletUSDT: w, AvailableUSDT: a, UpdatedAt: time.Now()}, nil
		}
	}
	return domain.Balance{Exchange: c.Name(), UpdatedAt: time.Now()}, nil
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
	var books []struct{ Symbol, BidPrice, AskPrice string }
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
		if price == 0 {
			price = mark
		}
		bid := bidBySymbol[p.Symbol]
		ask := askBySymbol[p.Symbol]
		spread := 0.0
		if bid > 0 && ask > 0 {
			spread = (ask - bid) / ((ask + bid) / 2)
		}
		out = append(out, domain.Candidate{
			Exchange: c.Name(), Symbol: p.Symbol, Price: price, MarkPrice: mark, FundingRate: fr,
			NextFundingTime: time.UnixMilli(p.NextFundingTime), Volume24hUSDT: volumeBySymbol[p.Symbol],
			Bid: bid, Ask: ask, Spread: spread, UpdatedAt: now,
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
	return domain.OrderResult{}, fmt.Errorf("live Binance order placement is scaffolded; enable after testnet validation")
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
		return fmt.Errorf("binance GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}

func (c *Client) signedGET(path string, params url.Values, dst any) error {
	// Binance signs the exact query string without the signature parameter.
	// Keep the signed payload unchanged and append signature at the end.
	// Re-encoding url.Values after adding signature can reorder parameters and
	// cause intermittent/consistent -1022 "Signature is not valid" errors.
	params.Set("recvWindow", "5000")
	rawQuery := params.Encode()
	sig := c.sign(rawQuery)
	u := c.baseURL + path + "?" + rawQuery + "&signature=" + sig
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("X-MBX-APIKEY", strings.TrimSpace(c.apiKey))
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("binance signed GET %s: %s", path, string(body))
	}
	return json.Unmarshal(body, dst)
}
func (c *Client) sign(payload string) string {
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

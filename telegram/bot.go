package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"funding-bot/internal/domain"
)

type Button struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}
type inlineKeyboard struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}

type Handlers struct {
	StartText     func() string
	ActiveText    func() string
	CountdownText func() string
	CloseAllText  func(confirm bool) string
	SkipText      func(exchange, symbol string, fundingUnix int64) string
}

type Bot struct {
	token        string
	chatID       string
	http         *http.Client
	enabled      bool
	dashboardURL string
}

func New(token, chatID, dashboardURL string) *Bot {
	token = strings.TrimSpace(strings.Trim(token, "'\""))
	chatID = strings.TrimSpace(strings.Trim(chatID, "'\""))
	return &Bot{token: token, chatID: chatID, dashboardURL: dashboardURL, http: &http.Client{Timeout: 15 * time.Second}, enabled: token != "" && chatID != ""}
}

func (b *Bot) Enabled() bool { return b.enabled }

func (b *Bot) Poll(ctx context.Context, h Handlers) {
	if !b.enabled {
		log.Println("telegram disabled: token or chat_id is empty")
		return
	}
	log.Println("telegram polling started")
	var offset int64
	for {
		select {
		case <-ctx.Done():
			log.Println("telegram polling stopped")
			return
		default:
		}
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			log.Printf("telegram getUpdates error: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, upd := range updates {
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
			}
			b.handleUpdate(upd, h)
		}
	}
}

func (b *Bot) handleUpdate(upd update, h Handlers) {
	if upd.Message != nil {
		text := strings.TrimSpace(upd.Message.Text)
		log.Printf("telegram update received: message chat=%d text=%q", upd.Message.Chat.ID, text)
		switch {
		case text == "/start" || text == "start":
			msg := "👋 <b>Funding Bot активен</b>"
			if h.StartText != nil {
				msg = h.StartText()
			}
			_ = b.Send(msg, b.mainButtons())
		case strings.HasPrefix(text, "/active"):
			_ = b.Send(callOrDefault(h.ActiveText, "📊 Активных сделок сейчас нет."), b.mainButtons())
		case strings.HasPrefix(text, "/countdown"):
			_ = b.Send(callOrDefault(h.CountdownText, countdownText()), b.mainButtons())
		default:
			_ = b.Send("Команда не распознана. Используй кнопки ниже.", b.mainButtons())
		}
	}
	if upd.CallbackQuery != nil {
		data := upd.CallbackQuery.Data
		log.Printf("telegram callback received: %s", data)
		_ = b.answerCallback(upd.CallbackQuery.ID)
		switch {
		case data == "active":
			_ = b.Send(callOrDefault(h.ActiveText, "📊 Активных сделок сейчас нет."), b.mainButtons())
		case data == "countdown":
			_ = b.Send(callOrDefault(h.CountdownText, countdownText()), b.mainButtons())
		case data == "close_all":
			_ = b.Send("⚠️ <b>Подтвердить закрытие всех активных сделок по рынку?</b>", []Button{{Text: "Да, закрыть все", CallbackData: "close_all_confirm"}, {Text: "Отмена", CallbackData: "cancel"}})
		case data == "close_all_confirm":
			_ = b.Send(callOrDefaultBool(h.CloseAllText, true, "Режим MONITOR/PAPER: реальных позиций для закрытия нет."), b.mainButtons())
		case data == "cancel":
			_ = b.Send("Операция отменена.", b.mainButtons())
		case strings.HasPrefix(data, "skip:"):
			parts := strings.Split(data, ":")
			if len(parts) >= 4 && h.SkipText != nil {
				var unix int64
				_, _ = fmt.Sscan(parts[3], &unix)
				_ = b.Send(h.SkipText(parts[1], parts[2], unix), b.mainButtons())
			} else {
				_ = b.Send("Сигнал пропущен.", b.mainButtons())
			}
		}
	}
}

func callOrDefault(fn func() string, def string) string {
	if fn != nil {
		return fn()
	}
	return def
}
func callOrDefaultBool(fn func(bool) string, arg bool, def string) string {
	if fn != nil {
		return fn(arg)
	}
	return def
}

func (b *Bot) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	payload := map[string]any{"timeout": 25, "allowed_updates": []string{"message", "callback_query"}}
	if offset > 0 {
		payload["offset"] = offset
	}
	var out struct {
		OK          bool     `json:"ok"`
		Description string   `json:"description"`
		Result      []update `json:"result"`
	}
	if err := b.postJSON(ctx, "getUpdates", payload, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram getUpdates: %s", out.Description)
	}
	return out.Result, nil
}

func (b *Bot) answerCallback(id string) error {
	if id == "" {
		return nil
	}
	payload := map[string]any{"callback_query_id": id}
	return b.postJSON(context.Background(), "answerCallbackQuery", payload, nil)
}

func (b *Bot) Send(text string, rows ...[]Button) error {
	if !b.enabled {
		return nil
	}
	payload := map[string]any{"chat_id": b.chatID, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}
	if len(rows) > 0 {
		payload["reply_markup"] = inlineKeyboard{InlineKeyboard: rows}
	}
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := b.postJSON(context.Background(), "sendMessage", payload, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("telegram sendMessage: %s", out.Description)
	}
	log.Println("telegram sendMessage ok")
	return nil
}

func (b *Bot) postJSON(ctx context.Context, method string, payload any, dst any) error {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+b.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram %s: %s", method, string(raw))
	}
	if dst != nil && len(raw) > 0 {
		return json.Unmarshal(raw, dst)
	}
	return nil
}

func (b *Bot) Startup(mode string, balances map[domain.ExchangeName]domain.Balance) error {
	msg := "✅ <b>Funding Bot запущен</b>\n\nРежим: <b>" + mode + "</b>\n\n"
	for _, ex := range []domain.ExchangeName{domain.ExchangeBinance, domain.ExchangeBybit} {
		bal := balances[ex]
		msg += fmt.Sprintf("%s: %.4f USDT available / %.4f wallet\n", ex, bal.AvailableUSDT, bal.WalletUSDT)
	}
	msg += "\nDashboard: " + b.dashboardURL
	return b.Send(msg, b.mainButtons())
}

func (b *Bot) PreFundingWarning(until time.Duration) error {
	return b.Send(fmt.Sprintf("🔔🔔🔔 <b>ВНИМАНИЕ! СКОРО FUNDING!</b> 🔔🔔🔔\n\nДо ближайшего 4h funding-окна: <b>%s</b>\n\nПроверяю Binance Futures и Bybit Futures.", fmtDuration(until)), b.mainButtons())
}

func (b *Bot) Planned(c domain.Candidate) error {
	msg := fmt.Sprintf("⚠️ <b>%s Futures: планируется вход</b>\n\nМонета: <b>%s</b>\nFunding: <b>+%.3f%%</b>\n24h Volume: %.0f USDT\nSpread: %.4f%%\n\nЦена сейчас: %.8f\nPlanned SHORT entry: %.8f\nTake Profit: %.8f\nFunding time: %s", c.Exchange, c.Symbol, c.FundingRate*100, c.Volume24hUSDT, c.Spread*100, c.Price, c.PlannedEntry, c.SafeTPPrice, c.NextFundingTime.Format(time.RFC3339))
	return b.Send(msg, []Button{{Text: "Пропустить сделку", CallbackData: fmt.Sprintf("skip:%s:%s:%d", c.Exchange, c.Symbol, c.NextFundingTime.Unix())}}, b.mainButtons())
}

func (b *Bot) mainButtons() []Button {
	return []Button{{Text: "Открыть Dashboard", URL: b.dashboardURL}, {Text: "Текущие сделки", CallbackData: "active"}, {Text: "Countdown до funding", CallbackData: "countdown"}, {Text: "Закрыть все", CallbackData: "close_all"}}
}

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func countdownText() string {
	now := time.Now().UTC().Add(3 * time.Hour)
	return fmt.Sprintf("⏳ <b>Countdown до funding</b>\n\nТекущее время UTC+3: %s\nДо ближайшего 4h funding: %s", now.Format("15:04:05"), fmtDuration(next4hFundingUTC().Sub(time.Now().UTC())))
}

func next4hFundingUTC() time.Time {
	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for t := base; ; t = t.Add(4 * time.Hour) {
		if t.After(now) {
			return t
		}
	}
}

type update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *message       `json:"message,omitempty"`
	CallbackQuery *callbackQuery `json:"callback_query,omitempty"`
}
type message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Chat      chat   `json:"chat"`
}
type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type callbackQuery struct {
	ID      string   `json:"id"`
	Data    string   `json:"data"`
	Message *message `json:"message,omitempty"`
}

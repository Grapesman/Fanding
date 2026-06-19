package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
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

	ManualCloseText func(tradeID string) string
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
	dashboardURL = strings.TrimSpace(strings.Trim(dashboardURL, "'\""))

	return &Bot{
		token:        token,
		chatID:       chatID,
		dashboardURL: dashboardURL,
		http:         &http.Client{Timeout: 60 * time.Second},
		enabled:      token != "" && chatID != "",
	}
}

func (b *Bot) Enabled() bool {
	return b.enabled
}

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
			if ctx.Err() != nil {
				log.Println("telegram polling stopped")
				return
			}

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
		chatID := upd.Message.Chat.ID
		text := strings.TrimSpace(upd.Message.Text)

		log.Printf("telegram update received: message chat=%d text=%q", chatID, text)

		if !b.allowedChat(chatID) {
			log.Printf("telegram ignored unauthorized chat=%d", chatID)
			return
		}

		command := normalizeCommand(text)

		switch command {
		case "/start", "/help":
			msg := "👋 <b>Funding Bot активен</b>\n\n" + helpText()

			if h.StartText != nil {
				msg = h.StartText() + "\n\n" + helpText()
			}

			b.mustSend(msg, b.mainButtons())

		case "/status":
			b.mustSend(callOrDefault(h.StartText, "✅ Funding Bot работает."), b.mainButtons())

		case "/scan", "/candidates":
			b.mustSend(callOrDefault(h.CountdownText, countdownText()), b.mainButtons())

		case "/balances":
			b.mustSend(callOrDefault(h.StartText, "Балансы пока недоступны."), b.mainButtons())

		case "/dashboard":
			b.sendDashboard()

		case "/active":
			b.mustSend(callOrDefault(h.ActiveText, "📊 Активных сделок сейчас нет."), b.mainButtons())

		case "/countdown":
			b.mustSend(callOrDefault(h.CountdownText, countdownText()), b.mainButtons())

		case "/close_all":
			b.mustSend(
				"⚠️ <b>Подтвердить закрытие всех активных сделок по рынку?</b>",
				[]Button{
					{Text: "Да, закрыть все", CallbackData: "close_all_confirm"},
					{Text: "Отмена", CallbackData: "cancel"},
				},
			)

		default:
			b.mustSend("Команда не распознана.\n\n"+helpText(), b.mainButtons())
		}
	}

	if upd.CallbackQuery != nil {
		data := strings.TrimSpace(upd.CallbackQuery.Data)

		log.Printf("telegram callback received: %s", data)

		_ = b.answerCallback(upd.CallbackQuery.ID)

		if upd.CallbackQuery.Message != nil && !b.allowedChat(upd.CallbackQuery.Message.Chat.ID) {
			log.Printf("telegram callback ignored unauthorized chat=%d", upd.CallbackQuery.Message.Chat.ID)
			return
		}

		switch {
		case data == "active":
			b.mustSend(callOrDefault(h.ActiveText, "📊 Активных сделок сейчас нет."), b.mainButtons())

		case data == "countdown":
			b.mustSend(callOrDefault(h.CountdownText, countdownText()), b.mainButtons())

		case data == "dashboard":
			b.sendDashboard()

		case data == "close_all":
			b.mustSend(
				"⚠️ <b>Подтвердить закрытие всех активных сделок по рынку?</b>",
				[]Button{
					{Text: "Да, закрыть все", CallbackData: "close_all_confirm"},
					{Text: "Отмена", CallbackData: "cancel"},
				},
			)

		case data == "close_all_confirm":
			b.mustSend(
				callOrDefaultBool(h.CloseAllText, true, "Режим MONITOR/PAPER: реальных позиций для закрытия нет."),
				b.mainButtons(),
			)

		case data == "cancel":
			b.mustSend("Операция отменена.", b.mainButtons())

		case strings.HasPrefix(data, "skip:"):
			parts := strings.Split(data, ":")

			if len(parts) >= 4 && h.SkipText != nil {
				fundingUnix, _ := strconv.ParseInt(parts[3], 10, 64)
				b.mustSend(h.SkipText(parts[1], parts[2], fundingUnix), b.mainButtons())
			} else {
				b.mustSend("Сигнал пропущен.", b.mainButtons())
			}

		case strings.HasPrefix(data, "manual_close:"):
			tradeID := strings.TrimPrefix(data, "manual_close:")

			if tradeID == "" {
				b.mustSend("Не удалось определить сделку для закрытия.", b.mainButtons())
				return
			}

			if h.ManualCloseText != nil {
				b.mustSend(h.ManualCloseText(tradeID), b.mainButtons())
				return
			}

			b.mustSend(
				"⚠️ Команда ручного закрытия получена, но обработчик manual close ещё не подключён в engine.",
				b.mainButtons(),
			)

		default:
			b.mustSend("Неизвестная кнопка: "+esc(data), b.mainButtons())
		}
	}
}

func (b *Bot) allowedChat(chatID int64) bool {
	if b.chatID == "" {
		return true
	}

	return strconv.FormatInt(chatID, 10) == b.chatID
}

func normalizeCommand(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}

	fields := strings.Fields(text)
	cmd := strings.ToLower(fields[0])

	if !strings.HasPrefix(cmd, "/") {
		return cmd
	}

	if at := strings.Index(cmd, "@"); at >= 0 {
		cmd = cmd[:at]
	}

	return cmd
}

func helpText() string {
	return "<b>Команды:</b>\n" +
		"/status — статус бота\n" +
		"/scan — ближайшие funding-события\n" +
		"/candidates — кандидаты\n" +
		"/balances — балансы\n" +
		"/dashboard — dashboard\n" +
		"/active — активные сделки\n" +
		"/countdown — countdown\n" +
		"/close_all — закрыть все активные сделки"
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
	payload := map[string]any{
		"timeout":         25,
		"allowed_updates": []string{"message", "callback_query"},
	}

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

	payload := map[string]any{
		"callback_query_id": id,
	}

	return b.postJSON(context.Background(), "answerCallbackQuery", payload, nil)
}

func (b *Bot) mustSend(text string, rows ...[]Button) {
	if err := b.Send(text, rows...); err != nil {
		log.Printf("telegram send failed: %v", err)
	}
}

func (b *Bot) Send(text string, rows ...[]Button) error {
	if !b.enabled {
		return nil
	}

	text = trimTelegramText(text)

	payload := map[string]any{
		"chat_id":                  b.chatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}

	if len(rows) > 0 {
		cleanRows := sanitizeButtonRows(rows)
		if len(cleanRows) > 0 {
			payload["reply_markup"] = inlineKeyboard{InlineKeyboard: cleanRows}
		}
	}

	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}

	if err := b.postJSON(context.Background(), "sendMessage", payload, &out); err != nil {
		log.Printf("telegram sendMessage error: %v", err)
		return err
	}

	if !out.OK {
		err := fmt.Errorf("telegram sendMessage: %s", out.Description)
		log.Printf("telegram sendMessage error: %v", err)
		return err
	}

	log.Println("telegram sendMessage ok")

	return nil
}

func (b *Bot) postJSON(ctx context.Context, method string, payload any, dst any) error {
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://api.telegram.org/bot"+b.token+"/"+method,
		bytes.NewReader(body),
	)
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
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("telegram %s decode error: %w; body=%s", method, err, string(raw))
		}
	}

	return nil
}

func (b *Bot) Startup(mode string, balances map[domain.ExchangeName]domain.Balance) error {
	msg := "✅ <b>Funding Bot запущен</b>\n\n"
	msg += "Режим: <b>" + esc(strings.ToUpper(mode)) + "</b>\n"
	msg += "Стратегия: <b>LIVE-ready funding execution</b>\n"
	msg += "Вход: <b>MARKET SHORT ровно в момент funding</b>\n"
	msg += "Размер сделки: <b>5 USDT</b>\n"
	msg += "Плечо: <b>1x isolated</b>\n"
	msg += "Минимальный funding: <b>+0.50%</b>\n\n"

	msg += formatBalances(balances)

	if b.dashboardURL != "" {
		msg += "\nDashboard: <code>" + esc(b.dashboardURL) + "</code>"
	}

	return b.Send(msg, b.mainButtons())
}

func (b *Bot) BalanceSummary(args ...any) error {
	msg := "💰 <b>Баланс futures</b>\n\n"

	if len(args) == 0 {
		msg += "Нет данных по балансам."
		return b.Send(msg, b.mainButtons())
	}

	if balances, ok := args[0].(map[domain.ExchangeName]domain.Balance); ok {
		msg += formatBalances(balances)
		return b.Send(msg, b.mainButtons())
	}

	msg += formatArgs(args...)

	return b.Send(msg, b.mainButtons())
}

func (b *Bot) TradePlanSummary(args ...any) error {
	msg := "📋 <b>План сделок</b>\n\n"

	if len(args) == 0 {
		msg += "Плановых сделок сейчас нет."
	} else {
		msg += formatArgs(args...)
	}

	return b.Send(msg, b.mainButtons())
}

func (b *Bot) TradeClosed(args ...any) error {
	msg := "✅ <b>Сделка закрыта</b>\n\n"

	if len(args) > 0 {
		msg += formatArgs(args...)
	}

	return b.Send(msg, b.mainButtons())
}

func (b *Bot) StillOpenAlert(args ...any) error {
	msg := "⚠️ <b>Позиция всё ещё открыта</b>\n\n"

	if len(args) > 0 {
		msg += formatArgs(args...)
	}

	tradeID := findFieldString(args, "ID", "TradeID", "TradeId")
	if tradeID != "" {
		return b.Send(
			msg,
			[]Button{{Text: "Закрыть по рынку", CallbackData: "manual_close:" + tradeID}},
			b.mainButtons(),
		)
	}

	return b.Send(msg, b.mainButtons())
}

func (b *Bot) TradeError(args ...any) error {
	msg := "❌ <b>Ошибка сделки</b>\n\n"

	if len(args) > 0 {
		msg += formatArgs(args...)
	}

	return b.Send(msg, b.mainButtons())
}

func (b *Bot) AutoMarketClosed(args ...any) error {
	msg := "🧯 <b>Позиция закрыта автоматически по рынку</b>\n\n"

	if len(args) > 0 {
		msg += formatArgs(args...)
	}

	return b.Send(msg, b.mainButtons())
}

func (b *Bot) PreFundingWarning(until time.Duration) error {
	msg := fmt.Sprintf(
		"🔔🔔🔔 <b>ВНИМАНИЕ! СКОРО FUNDING!</b> 🔔🔔🔔\n\n"+
			"До ближайшего funding-окна: <b>%s</b>\n\n"+
			"Бот проверяет доступные futures funding-события.",
		fmtDuration(until),
	)

	return b.Send(msg, b.mainButtons())
}

func (b *Bot) Planned(c domain.Candidate) error {
	fundingLocal := c.NextFundingTime.UTC().Add(3 * time.Hour).Format("2006-01-02 15:04:05 UTC+3")

	msg := fmt.Sprintf(
		"⚠️ <b>%s Futures: планируется вход</b>\n\n"+
			"Монета: <b>%s</b>\n"+
			"Native: <code>%s</code>\n"+
			"Funding: <b>%+.4f%%</b>\n"+
			"Interval: %.0fh\n"+
			"24h Volume: %.0f USDT\n"+
			"Spread: %.4f%%\n\n"+
			"Price: %.8f\n"+
			"Mark: %.8f\n"+
			"Planned SHORT entry: %.8f\n"+
			"Take Profit: %.8f\n"+
			"Funding time: %s",
		esc(string(c.Exchange)),
		esc(c.Symbol),
		esc(c.NativeSymbol),
		c.FundingRate*100,
		c.FundingIntervalHours,
		c.Volume24hUSDT,
		c.Spread*100,
		c.Price,
		c.MarkPrice,
		c.PlannedEntry,
		c.SafeTPPrice,
		fundingLocal,
	)

	return b.Send(
		msg,
		[]Button{
			{
				Text:         "Пропустить сделку",
				CallbackData: fmt.Sprintf("skip:%s:%s:%d", c.Exchange, c.Symbol, c.NextFundingTime.Unix()),
			},
		},
		b.mainButtons(),
	)
}

func (b *Bot) sendDashboard() {
	if b.dashboardURL == "" {
		b.mustSend("Dashboard URL не задан.", b.mainButtons())
		return
	}

	if isPublicHTTPURL(b.dashboardURL) {
		b.mustSend(
			"🌐 <b>Dashboard</b>\n"+esc(b.dashboardURL),
			[]Button{{Text: "Открыть dashboard", URL: b.dashboardURL}},
			b.mainButtons(),
		)
		return
	}

	b.mustSend(
		"🌐 <b>Dashboard</b>\n\n"+
			"Локальный адрес нельзя открыть кнопкой Telegram:\n"+
			"<code>"+esc(b.dashboardURL)+"</code>\n\n"+
			"Открой его в браузере на компьютере.",
		b.mainButtons(),
	)
}

func (b *Bot) mainButtons() []Button {
	buttons := []Button{
		{Text: "Текущие сделки", CallbackData: "active"},
		{Text: "Countdown до funding", CallbackData: "countdown"},
		{Text: "Dashboard", CallbackData: "dashboard"},
		{Text: "Закрыть все", CallbackData: "close_all"},
	}

	if isPublicHTTPURL(b.dashboardURL) {
		buttons[2] = Button{Text: "Открыть Dashboard", URL: b.dashboardURL}
	}

	return buttons
}

func sanitizeButtonRows(rows [][]Button) [][]Button {
	cleanRows := make([][]Button, 0, len(rows))

	for _, row := range rows {
		cleanRow := make([]Button, 0, len(row))

		for _, btn := range row {
			btn.Text = strings.TrimSpace(btn.Text)
			btn.URL = strings.TrimSpace(btn.URL)
			btn.CallbackData = strings.TrimSpace(btn.CallbackData)

			if btn.Text == "" {
				continue
			}

			if btn.URL != "" && !isPublicHTTPURL(btn.URL) {
				btn.URL = ""
			}

			if btn.URL == "" && btn.CallbackData == "" {
				continue
			}

			cleanRow = append(cleanRow, btn)
		}

		if len(cleanRow) > 0 {
			cleanRows = append(cleanRows, cleanRow)
		}
	}

	return cleanRows
}

func isPublicHTTPURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return false
	}

	host := strings.ToLower(u.Hostname())

	return (u.Scheme == "http" || u.Scheme == "https") &&
		host != "" &&
		host != "localhost" &&
		host != "127.0.0.1" &&
		host != "::1"
}

func formatBalances(balances map[domain.ExchangeName]domain.Balance) string {
	if len(balances) == 0 {
		return "<b>Балансы futures:</b>\nНет данных.\n"
	}

	keys := make([]string, 0, len(balances))
	byName := make(map[string]domain.Balance, len(balances))

	for ex, bal := range balances {
		name := string(ex)
		keys = append(keys, name)
		byName[name] = bal
	}

	sort.Strings(keys)

	msg := "<b>Балансы futures:</b>\n"

	for _, name := range keys {
		bal := byName[name]

		status := "❌"
		if bal.PrivateOK && bal.AvailableUSDT >= 5 {
			status = "✅"
		} else if bal.PrivateOK {
			status = "⚠️"
		}

		errText := ""
		if bal.Error != "" {
			errText = " — " + esc(bal.Error)
		}

		msg += fmt.Sprintf(
			"%s <b>%s</b>: available %.4f / wallet %.4f USDT%s\n",
			status,
			esc(name),
			bal.AvailableUSDT,
			bal.WalletUSDT,
			errText,
		)
	}

	return msg
}

func formatArgs(args ...any) string {
	if len(args) == 0 {
		return ""
	}

	parts := make([]string, 0, len(args))

	for _, arg := range args {
		parts = append(parts, formatAny(arg))
	}

	return strings.Join(parts, "\n\n")
}

func formatAny(v any) string {
	if v == nil {
		return "<code>nil</code>"
	}

	switch x := v.(type) {
	case string:
		return esc(x)
	case error:
		return esc(x.Error())
	case fmt.Stringer:
		return esc(x.String())
	case map[domain.ExchangeName]domain.Balance:
		return formatBalances(x)
	}

	rv := reflect.ValueOf(v)
	rt := reflect.TypeOf(v)

	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "<code>nil</code>"
		}

		rv = rv.Elem()
		rt = rt.Elem()
	}

	if rv.Kind() != reflect.Struct {
		return "<code>" + esc(fmt.Sprintf("%+v", v)) + "</code>"
	}

	lines := make([]string, 0, rv.NumField())

	for i := 0; i < rv.NumField(); i++ {
		field := rt.Field(i)

		if field.PkgPath != "" {
			continue
		}

		name := field.Name
		value := rv.Field(i)

		lines = append(lines, fmt.Sprintf("<b>%s:</b> %s", esc(name), esc(fmt.Sprint(value.Interface()))))
	}

	if len(lines) == 0 {
		return "<code>" + esc(fmt.Sprintf("%+v", v)) + "</code>"
	}

	return strings.Join(lines, "\n")
}

func findFieldString(args []any, names ...string) string {
	wanted := map[string]struct{}{}
	for _, name := range names {
		wanted[name] = struct{}{}
	}

	for _, arg := range args {
		if arg == nil {
			continue
		}

		rv := reflect.ValueOf(arg)
		rt := reflect.TypeOf(arg)

		if rv.Kind() == reflect.Pointer {
			if rv.IsNil() {
				continue
			}

			rv = rv.Elem()
			rt = rt.Elem()
		}

		if rv.Kind() != reflect.Struct {
			continue
		}

		for i := 0; i < rv.NumField(); i++ {
			field := rt.Field(i)
			if _, ok := wanted[field.Name]; !ok {
				continue
			}

			if field.PkgPath != "" {
				continue
			}

			value := rv.Field(i)
			if value.Kind() == reflect.String {
				return value.String()
			}
		}
	}

	return ""
}

func trimTelegramText(text string) string {
	const maxRunes = 3900

	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}

	return string(runes[:maxRunes]) + "\n\n…сообщение обрезано."
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
	nowUTC := time.Now().UTC()
	nowLocal := nowUTC.Add(3 * time.Hour)
	next := next4hFundingUTC()

	return fmt.Sprintf(
		"⏳ <b>Countdown до funding</b>\n\n"+
			"Текущее время UTC+3: <b>%s</b>\n"+
			"Ближайшее 4h funding-окно UTC: <b>%s</b>\n"+
			"До funding: <b>%s</b>",
		nowLocal.Format("15:04:05"),
		next.Format("15:04:05"),
		fmtDuration(next.Sub(nowUTC)),
	)
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

func esc(s string) string {
	return html.EscapeString(s)
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

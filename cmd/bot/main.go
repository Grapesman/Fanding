package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"funding-bot/internal/config"
	"funding-bot/internal/dashboard"
	"funding-bot/internal/domain"
	"funding-bot/internal/engine"
	"funding-bot/internal/exchange/binance"
	"funding-bot/internal/exchange/bingx"
	"funding-bot/internal/exchange/bitget"
	"funding-bot/internal/exchange/bybit"
	"funding-bot/internal/exchange/gate"
	"funding-bot/internal/exchange/htx"
	"funding-bot/internal/exchange/kucoin"
	"funding-bot/internal/exchange/mexc"
	"funding-bot/internal/exchange/okx"
	"funding-bot/internal/storage"
	"funding-bot/internal/telegram"
)

func main() {
	envPath := flag.String("env", ".env", "path to env file")
	flag.Parse()

	cfg, err := config.Load(*envPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store := storage.NewStore()

	pg, err := openPostgresWithRetry(cfg.PostgresDSN(), 30, 2*time.Second)
	if err != nil {
		store.AddLog("postgres unavailable, using memory fallback: " + err.Error())
		log.Printf("postgres unavailable: %v", err)
	} else {
		defer pg.Close()

		if err := pg.Migrate(); err != nil {
			store.AddLog("postgres migrate error: " + err.Error())
			log.Printf("postgres migrate: %v", err)
		} else {
			store.SetPostgres(pg)
			store.AddLog("postgres connected and migrations applied")
			log.Printf("postgres connected")
		}
	}

	exchanges := buildExchanges(cfg)

	tg := telegram.New(cfg.TelegramBotToken, cfg.TelegramChatID, cfg.DashboardPublicURL)
	dash := dashboard.New(cfg, store)

	go func() {
		log.Printf("dashboard listening on http://%s", dash.Addr())
		if err := dash.Run(); err != nil {
			log.Printf("dashboard: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go tg.Poll(ctx, telegram.Handlers{
		StartText: func() string {
			return startupText(cfg, store)
		},
		ActiveText: func() string {
			return activeText(store)
		},
		CountdownText: countdownText,
		CloseAllText: func(confirm bool) string {
			if !confirm {
				return "Закрытие всех сделок отменено."
			}

			store.AddLog("telegram close-all requested")

			return "⚠️ Запрос на закрытие всех активных сделок получен.\n\n" +
				"Сейчас обработчик close-all подготовлен, но реальное закрытие будет подключено в engine на следующем этапе."
		},
		SkipText: func(exchange, symbol string, fundingUnix int64) string {
			store.MarkSkipped(domain.ExchangeName(exchange), symbol, fundingUnix)
			store.AddLog("telegram skip signal: " + exchange + " " + symbol)

			return fmt.Sprintf(
				"Сделка по %s на %s Futures пропущена вручную для funding time %s.",
				symbol,
				exchange,
				time.Unix(fundingUnix, 0).UTC().Add(3*time.Hour).Format("2006-01-02 15:04:05 UTC+3"),
			)
		},
		ManualCloseText: func(tradeID string) string {
			store.AddLog("telegram manual close requested: " + tradeID)

			t, ok := store.FindActiveTrade(tradeID)
			if !ok {
				return "Не нашёл активную сделку для ручного закрытия: " + tradeID
			}

			return fmt.Sprintf(
				"⚠️ Запрос ручного закрытия по рынку получен.\n\n"+
					"Биржа: %s\n"+
					"Символ: %s\n"+
					"Сценарий: %s\n"+
					"Текущий статус: %s\n"+
					"Unrealized PnL: %+.6f USDT\n\n"+
					"Реальное закрытие market-ордером будет подключено в engine на следующем этапе.",
				t.Exchange,
				displaySymbol(t.Symbol, t.NativeSymbol),
				t.Scenario,
				t.Status,
				t.UnrealizedPNL,
			)
		},
	})

	eng := engine.New(cfg, exchanges, store, tg)

	log.Printf("funding bot started mode=%s exchanges=%d", cfg.BotMode, len(exchanges))

	if err := eng.Run(ctx); err != nil {
		log.Fatalf("engine: %v", err)
	}
}

func buildExchanges(cfg config.Config) []domain.Exchange {
	exchanges := []domain.Exchange{}

	if cfg.Binance.Enabled {
		exchanges = append(exchanges, binance.New(cfg.Binance.BaseURL, cfg.Binance.APIKey, cfg.Binance.APISecret))
	}
	if cfg.Bybit.Enabled {
		exchanges = append(exchanges, bybit.New(cfg.Bybit.BaseURL, cfg.Bybit.APIKey, cfg.Bybit.APISecret))
	}
	if cfg.BingX.Enabled {
		exchanges = append(exchanges, bingx.New(cfg.BingX.BaseURL, cfg.BingX.APIKey, cfg.BingX.APISecret))
	}
	if cfg.Bitget.Enabled {
		exchanges = append(exchanges, bitget.New(cfg.Bitget.BaseURL, cfg.Bitget.APIKey, cfg.Bitget.APISecret))
	}
	if cfg.OKX.Enabled {
		exchanges = append(exchanges, okx.New(cfg.OKX.BaseURL, cfg.OKX.APIKey, cfg.OKX.APISecret))
	}
	if cfg.Gate.Enabled {
		exchanges = append(exchanges, gate.New(cfg.Gate.BaseURL, cfg.Gate.APIKey, cfg.Gate.APISecret))
	}
	if cfg.MEXC.Enabled {
		exchanges = append(exchanges, mexc.New(cfg.MEXC.BaseURL, cfg.MEXC.APIKey, cfg.MEXC.APISecret))
	}
	if cfg.KuCoin.Enabled {
		exchanges = append(exchanges, kucoin.New(cfg.KuCoin.BaseURL, cfg.KuCoin.APIKey, cfg.KuCoin.APISecret))
	}
	if cfg.HTX.Enabled {
		exchanges = append(exchanges, htx.New(cfg.HTX.BaseURL, cfg.HTX.APIKey, cfg.HTX.APISecret))
	}

	return exchanges
}

func startupText(cfg config.Config, store *storage.Store) string {
	balances := store.Balances()
	statuses := store.ExchangeStatuses()
	errors := store.ExchangeErrors()

	msg := "✅ <b>Funding Bot активен</b>\n\n"

	msg += fmt.Sprintf("Режим: <b>%s</b>\n", strings.ToUpper(string(cfg.BotMode)))
	msg += "Стратегия: <b>funding live-ready execution</b>\n"
	msg += "Вход: <b>MARKET SHORT ровно в момент funding</b>\n"
	msg += fmt.Sprintf("Планирование: <b>каждый час в минуту %02d UTC+3</b>\n", cfg.PlanMinuteUTC3)
	msg += fmt.Sprintf("Min funding: <b>+%.2f%%</b>\n", cfg.MinFundingRate*100)
	msg += fmt.Sprintf("Размер сделки: <b>%.2f USDT</b>\n", cfg.USDTPerTrade)
	msg += fmt.Sprintf("Плечо: <b>%dx</b>\n", cfg.Leverage)
	msg += fmt.Sprintf("Margin: <b>%s</b>\n\n", cfg.MarginMode)

	msg += "<b>Сценарии:</b>\n"
	msg += "1) short подтвердился быстро → LIMIT BUY по fair price\n"
	msg += "2) funding списан или short подтвердился поздно → LIMIT BUY ниже на funding/2\n"
	msg += "3) short не подтвердился за 1.5 сек → две лимитки вокруг fair price с отклонением funding/3\n\n"

	msg += "<b>Балансы futures:</b>\n"

	for _, ex := range domain.AllExchangeNames {
		bal := balances[ex]

		publicStatus := "❌ public"
		if statuses[ex] {
			publicStatus = "✅ public"
		}

		privateStatus := "❌ balance"
		if bal.PrivateOK {
			privateStatus = "✅ balance"
		}

		capacity := 0
		if bal.PrivateOK && cfg.USDTPerTrade > 0 {
			capacity = int(bal.AvailableUSDT / cfg.USDTPerTrade)
		}

		msg += fmt.Sprintf(
			"%s <b>%s</b>: %s · %s · available %.4f / wallet %.4f USDT · сделок: <b>%d</b>\n",
			exStatusEmoji(statuses[ex], bal),
			ex,
			publicStatus,
			privateStatus,
			bal.AvailableUSDT,
			bal.WalletUSDT,
			capacity,
		)

		if errText := strings.TrimSpace(errors[ex]); errText != "" {
			msg += "   error: <code>" + escapeTelegramShort(errText, 120) + "</code>\n"
		}
	}

	msg += "\nDashboard: " + cfg.DashboardPublicURL

	return msg
}

func activeText(store *storage.Store) string {
	planned := store.PlannedTrades()
	trades := store.ActiveTrades()

	if len(planned) == 0 && len(trades) == 0 {
		return "📊 <b>Текущие сделки</b>\n\nАктивных и запланированных сделок сейчас нет."
	}

	msg := "📊 <b>Текущие сделки</b>\n\n"

	if len(planned) > 0 {
		msg += "<b>Запланированные:</b>\n"

		for _, p := range planned {
			msg += fmt.Sprintf(
				"• <b>%s</b> %s\n"+
					"  Funding: %+.3f%% / %.0fh\n"+
					"  Time: %s\n"+
					"  Size: %.2f USDT · %dx %s\n"+
					"  Status: <code>%s</code>\n\n",
				p.Exchange,
				displaySymbol(p.Symbol, p.NativeSymbol),
				p.FundingRate*100,
				nonZero(p.FundingIntervalHours, 8),
				formatUTC3(p.FundingTime),
				p.PositionUSDT,
				p.Leverage,
				p.MarginMode,
				p.Status,
			)
		}
	}

	if len(trades) > 0 {
		msg += "<b>Активные:</b>\n"

		for _, t := range trades {
			entry := firstNonZero(t.EntryAvgPrice, t.EntryPrice)
			current := firstNonZero(t.MarkPrice, t.CurrentPrice)

			msg += fmt.Sprintf(
				"• <b>%s</b> %s\n"+
					"  Scenario: <code>%s</code>\n"+
					"  Status: <code>%s</code>\n"+
					"  Entry: %.12f\n"+
					"  Current: %.12f\n"+
					"  TP: %.12f\n"+
					"  Qty: %.8f\n"+
					"  Unrealized PnL: <b>%+.6f USDT</b>\n\n",
				t.Exchange,
				displaySymbol(t.Symbol, t.NativeSymbol),
				t.Scenario,
				t.Status,
				entry,
				current,
				t.TakeProfitPrice,
				t.Qty,
				t.UnrealizedPNL,
			)
		}
	}

	return msg
}

func countdownText() string {
	nowUTC := time.Now().UTC()
	nowUTC3 := nowUTC.Add(3 * time.Hour)

	return fmt.Sprintf(
		"⏳ <b>Countdown до funding</b>\n\n"+
			"Текущее время UTC+3: <b>%s</b>\n\n"+
			"До ближайшего 1h funding: <b>%s</b>\n"+
			"До ближайшего 4h funding: <b>%s</b>\n"+
			"До ближайшего 8h funding: <b>%s</b>\n\n"+
			"План сделок формируется каждый час в минуту <b>:45 UTC+3</b>.",
		nowUTC3.Format("15:04:05"),
		fmtDuration(nextFundingUTC(nowUTC, 1).Sub(nowUTC)),
		fmtDuration(nextFundingUTC(nowUTC, 4).Sub(nowUTC)),
		fmtDuration(nextFundingUTC(nowUTC, 8).Sub(nowUTC)),
	)
}

func nextFundingUTC(now time.Time, hours int) time.Time {
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

func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}

	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60

	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

func formatUTC3(t time.Time) string {
	if t.IsZero() {
		return "—"
	}

	return t.UTC().Add(3*time.Hour).Format("2006-01-02 15:04:05") + " UTC+3"
}

func exStatusEmoji(publicOK bool, b domain.Balance) string {
	if publicOK && b.PrivateOK && b.AvailableUSDT > 0 {
		return "✅"
	}
	if publicOK || b.PrivateOK {
		return "⚠️"
	}

	return "❌"
}

func displaySymbol(symbol, native string) string {
	if strings.TrimSpace(native) != "" {
		return native
	}

	return symbol
}

func firstNonZero(values ...float64) float64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}

	return 0
}

func nonZero(v, fallback float64) float64 {
	if v != 0 {
		return v
	}

	return fallback
}

func escapeTelegramShort(s string, max int) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")

	if max > 0 && len([]rune(s)) > max {
		r := []rune(s)
		return string(r[:max]) + "..."
	}

	return s
}

func openPostgresWithRetry(dsn string, attempts int, delay time.Duration) (*storage.Postgres, error) {
	var lastErr error

	for i := 1; i <= attempts; i++ {
		pg, err := storage.OpenPostgres(dsn)
		if err == nil {
			return pg, nil
		}

		lastErr = err
		log.Printf("postgres connection attempt %d/%d failed: %v", i, attempts, err)
		time.Sleep(delay)
	}

	return nil, fmt.Errorf("postgres connection failed after %d attempts: %w", attempts, lastErr)
}

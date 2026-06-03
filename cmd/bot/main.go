package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
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

	mem := storage.NewStore()
	if pg, err := storage.OpenPostgres(cfg.PostgresDSN()); err == nil {
		defer pg.Close()
		if err := pg.Migrate(); err != nil {
			log.Printf("postgres migrate: %v", err)
		}
		mem.AddLog("postgres connected")
	} else {
		mem.AddLog("postgres unavailable, using memory store only: " + err.Error())
		log.Printf("postgres unavailable: %v", err)
	}

	exchanges := buildExchanges(cfg)

	tg := telegram.New(cfg.TelegramBotToken, cfg.TelegramChatID, cfg.DashboardPublicURL)
	dash := dashboard.New(cfg, mem)
	go func() {
		log.Printf("dashboard listening on http://%s", dash.Addr())
		if err := dash.Run(); err != nil {
			log.Printf("dashboard: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go tg.Poll(ctx, telegram.Handlers{
		StartText:     func() string { return startupText(cfg, mem) },
		ActiveText:    func() string { return activeText(mem) },
		CountdownText: countdownText,
		CloseAllText: func(confirm bool) string {
			return "Режим " + string(cfg.BotMode) + ": endpoint закрытия всех позиций подготовлен. Реальная реализация подключается по биржам поэтапно."
		},
		SkipText: func(exchange, symbol string, fundingUnix int64) string {
			mem.MarkSkipped(domain.ExchangeName(exchange), symbol, fundingUnix)
			mem.AddLog("telegram skip signal: " + exchange + " " + symbol)
			return "Сделка по " + symbol + " на " + exchange + " Futures пропущена вручную."
		},
	})

	eng := engine.New(cfg, exchanges, mem, tg)
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
	msg := fmt.Sprintf("👋 <b>Funding Bot активен</b>\n\nРежим: <b>%s</b>\n\n", cfg.BotMode)
	for _, ex := range domain.AllExchangeNames {
		bal := balances[ex]
		status := "Not Connected"
		if statuses[ex] {
			status = "Connected"
		}
		msg += fmt.Sprintf("%s: <b>%s</b>\nBalance: %.2f available / %.2f wallet USDT\n\n", ex, status, bal.AvailableUSDT, bal.WalletUSDT)
	}
	msg += fmt.Sprintf("Min funding: +%.2f%%\nMin 24h volume: %.0f USDT\nLeverage: isolated %dx\nUSDT per trade: %.2f\n\nDashboard: %s", cfg.MinFundingRate*100, cfg.Min24hVolumeUSDT, cfg.Leverage, cfg.USDTPerTrade, cfg.DashboardPublicURL)
	return msg
}

func activeText(store *storage.Store) string {
	trades := store.ActiveTrades()
	if len(trades) == 0 {
		return "📊 <b>Текущие сделки</b>\n\nАктивных сделок сейчас нет."
	}
	msg := "📊 <b>Текущие сделки</b>\n\n"
	for _, t := range trades {
		msg += fmt.Sprintf("%s %s | %s | %s\nEntry: %.8f\nTP: %.8f\nPnL: %.4f USDT\nStatus: %s\n\n", t.Exchange, t.Symbol, t.Side, t.Scenario, t.EntryPrice, t.TakeProfitPrice, t.UnrealizedPNL, t.Status)
	}
	return msg
}

func countdownText() string {
	nowUTC := time.Now().UTC()
	nowPlus3 := time.Now()
	return fmt.Sprintf("⏳ <b>Countdown до funding</b>\n\nТекущее время UTC+3: %s\n1h: %s\n4h: %s\n8h: %s", nowPlus3.Format("15:04:05"), fmtDuration(nextFundingUTC(nowUTC, 1).Sub(nowUTC)), fmtDuration(nextFundingUTC(nowUTC, 4).Sub(nowUTC)), fmtDuration(nextFundingUTC(nowUTC, 8).Sub(nowUTC)))
}

func nextFundingUTC(now time.Time, hours int) time.Time {
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

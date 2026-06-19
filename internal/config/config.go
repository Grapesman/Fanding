package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModeMonitor Mode = "monitor"
	ModePaper   Mode = "paper"
	ModeLive    Mode = "live"
)

type ExchangeConfig struct {
	Enabled    bool
	BaseURL    string
	APIKey     string
	APISecret  string
	Passphrase string
}

type Config struct {
	BotMode          Mode
	AllowMainnetLive bool

	Binance ExchangeConfig
	Bybit   ExchangeConfig
	BingX   ExchangeConfig
	Bitget  ExchangeConfig
	OKX     ExchangeConfig
	Gate    ExchangeConfig
	MEXC    ExchangeConfig
	KuCoin  ExchangeConfig
	HTX     ExchangeConfig

	OnlyPositiveFunding bool
	MinFundingRate      float64 // decimal, 0.005 = 0.5%
	Min24hVolumeUSDT    float64
	UseMaxVolumeFilter  bool
	Max24hVolumeUSDT    float64
	UseSpreadFilter     bool
	MaxSpread           float64 // decimal

	// Legacy fields kept for compatibility with current engine/dashboard.
	// New live strategy does not enter before funding.
	EntryBeforeFunding time.Duration
	EntryOffset        float64 // decimal
	TakeFundingMove    float64 // decimal
	Scenario2ExtraTP   float64 // decimal, legacy; new formula is funding_rate / 2
	Scenario3Grid      float64 // decimal, legacy; new formula is funding_rate / 3
	Scenario3OneShot   bool

	// Live strategy scheduler.
	// The plan cycle runs every hour at minute 45 in UTC+3.
	PlanMinuteUTC3 int

	// At T - PreFundingMarkCapture the bot captures mark price and calculates:
	// fair_price = pre_funding_mark_price * (1 - funding_rate)
	PreFundingMarkCapture time.Duration

	// At funding time the bot sends MARKET SHORT and waits this long for fill confirmation.
	EntryConfirmTimeout time.Duration

	// If a trade is still open T + ManualAlertAfterFunding after funding,
	// Telegram gets an alert with unrealized PnL and a "Close by market" button.
	ManualAlertAfterFunding time.Duration

	// If a trade is still open T + AutoCloseAfterFunding after funding,
	// the bot cancels limit orders and closes by market automatically.
	AutoCloseAfterFunding time.Duration

	PositionSizeMode         string
	USDTPerTrade             float64
	PositionPercentOfBalance float64

	// For the new live strategy:
	// USDTPerTrade = 5
	// Leverage = 1
	// MarginMode = isolated
	Leverage   int
	MarginMode string

	// 0 means no artificial cap; capacity is floor(available_usdt / USDTPerTrade).
	MaxActiveTradesPerExchange int
	AutoScaleTradesByBalance   bool

	PreFundingWarning    time.Duration
	StuckAlertInterval   time.Duration
	ScanInterval         time.Duration
	TimeSyncInterval     time.Duration
	MaxAllowedTimeOffset time.Duration

	TelegramBotToken   string
	TelegramChatID     string
	DashboardPublicURL string

	DashboardHost string
	DashboardPort string

	PostgresHost     string
	PostgresPort     string
	PostgresUser     string
	PostgresPassword string
	PostgresDB       string

	LogLevel string
}

func Load(path string) (Config, error) {
	if path != "" {
		_ = loadDotEnv(path)
	}

	c := Config{
		BotMode:          Mode(strings.ToLower(getenv("BOT_MODE", "monitor"))),
		AllowMainnetLive: getBool("ALLOW_MAINNET_LIVE", false),

		Binance: loadExchange("BINANCE", true, "https://fapi.binance.com"),
		Bybit:   loadExchange("BYBIT", true, "https://api.bybit.com"),
		BingX:   loadExchange("BINGX", true, "https://open-api.bingx.com"),
		Bitget:  loadExchange("BITGET", true, "https://api.bitget.com"),
		OKX:     loadExchange("OKX", true, "https://www.okx.com"),
		Gate:    loadExchange("GATE", true, "https://api.gateio.ws"),
		MEXC:    loadExchange("MEXC", true, "https://contract.mexc.com"),
		KuCoin:  loadExchange("KUCOIN", true, "https://api-futures.kucoin.com"),
		HTX:     loadExchange("HTX", true, "https://api.hbdm.com"),

		OnlyPositiveFunding: getBool("ONLY_POSITIVE_FUNDING", true),

		// New live defaults:
		// funding >= +0.50%
		// position size = 5 USDT
		// no leverage: 1x isolated
		MinFundingRate: percentEnv("MIN_FUNDING_RATE_PERCENT", 0.50),

		Min24hVolumeUSDT:   getFloat("MIN_24H_VOLUME_USDT", 500000),
		UseMaxVolumeFilter: getBool("USE_MAX_VOLUME_FILTER", false),
		Max24hVolumeUSDT:   getFloat("MAX_24H_VOLUME_USDT", 50000000),
		UseSpreadFilter:    getBool("USE_SPREAD_FILTER", false),
		MaxSpread:          percentEnv("MAX_SPREAD_PERCENT", 0.30),

		// Legacy compatibility. Do not use this for live entry timing anymore.
		EntryBeforeFunding: time.Duration(getInt("ENTRY_BEFORE_FUNDING_SEC", 0)) * time.Second,
		EntryOffset:        percentEnv("ENTRY_OFFSET_PERCENT", 0.20),
		TakeFundingMove:    percentEnv("TAKE_FUNDING_MOVE_PERCENT", 100),
		Scenario2ExtraTP:   percentEnv("SCENARIO_2_EXTRA_TP_PERCENT", 0),
		Scenario3Grid:      percentEnv("SCENARIO_3_GRID_FROM_FAIR_PERCENT", 0),
		Scenario3OneShot:   getBool("SCENARIO_3_ONE_SHOT", true),

		PlanMinuteUTC3:          getInt("PLAN_MINUTE_UTC3", 45),
		PreFundingMarkCapture:   time.Duration(getInt("PRE_FUNDING_MARK_CAPTURE_SEC", 1)) * time.Second,
		EntryConfirmTimeout:     time.Duration(getInt("ENTRY_CONFIRM_TIMEOUT_MS", 1500)) * time.Millisecond,
		ManualAlertAfterFunding: time.Duration(getInt("MANUAL_ALERT_AFTER_FUNDING_SEC", 120)) * time.Second,
		AutoCloseAfterFunding:   time.Duration(getInt("AUTO_CLOSE_AFTER_FUNDING_SEC", 300)) * time.Second,

		PositionSizeMode:         strings.ToLower(getenv("POSITION_SIZE_MODE", "fixed")),
		USDTPerTrade:             getFloat("USDT_PER_TRADE", 5),
		PositionPercentOfBalance: percentEnv("POSITION_PERCENT_OF_BALANCE", 100),

		Leverage:   getInt("LEVERAGE", 1),
		MarginMode: strings.ToLower(getenv("MARGIN_MODE", "isolated")),

		MaxActiveTradesPerExchange: getInt("MAX_ACTIVE_TRADES_PER_EXCHANGE", 0),
		AutoScaleTradesByBalance:   getBool("AUTO_SCALE_TRADES_BY_BALANCE", true),

		PreFundingWarning:    time.Duration(getInt("PRE_FUNDING_WARNING_MINUTES", 15)) * time.Minute,
		StuckAlertInterval:   time.Duration(getInt("STUCK_POSITION_ALERT_INTERVAL_SEC", 30)) * time.Second,
		ScanInterval:         time.Duration(getInt("SCAN_INTERVAL_SEC", 15)) * time.Second,
		TimeSyncInterval:     time.Duration(getInt("TIME_SYNC_INTERVAL_SEC", 10)) * time.Second,
		MaxAllowedTimeOffset: time.Duration(getInt("MAX_ALLOWED_TIME_OFFSET_MS", 500)) * time.Millisecond,

		TelegramBotToken:   getenv("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:     getenv("TELEGRAM_CHAT_ID", ""),
		DashboardPublicURL: getenv("DASHBOARD_PUBLIC_URL", "http://localhost:8095"),

		DashboardHost: getenv("DASHBOARD_HOST", "0.0.0.0"),
		DashboardPort: getenv("DASHBOARD_PORT", "8095"),

		PostgresHost:     getenv("POSTGRES_HOST", "postgres"),
		PostgresPort:     getenv("POSTGRES_PORT", "5432"),
		PostgresUser:     getenv("POSTGRES_USER", "funding_bot"),
		PostgresPassword: getenv("POSTGRES_PASSWORD", "funding_bot"),
		PostgresDB:       getenv("POSTGRES_DB", "funding_bot"),

		LogLevel: strings.ToLower(getenv("LOG_LEVEL", "info")),
	}

	if err := c.Validate(); err != nil {
		return c, err
	}

	return c, nil
}

func loadExchange(prefix string, defaultEnabled bool, defaultBaseURL string) ExchangeConfig {
	return ExchangeConfig{
		Enabled:    getBool("ENABLE_"+prefix, defaultEnabled),
		BaseURL:    strings.TrimRight(getenv(prefix+"_BASE_URL", defaultBaseURL), "/"),
		APIKey:     getenv(prefix+"_API_KEY", ""),
		APISecret:  getenv(prefix+"_API_SECRET", ""),
		Passphrase: getenv(prefix+"_API_PASSPHRASE", ""),
	}
}

func (c Config) Validate() error {
	switch c.BotMode {
	case ModeMonitor, ModePaper, ModeLive:
	default:
		return fmt.Errorf("invalid BOT_MODE %q", c.BotMode)
	}

	if c.BotMode == ModeLive && !c.AllowMainnetLive {
		return fmt.Errorf("LIVE mode is blocked: set ALLOW_MAINNET_LIVE=true to trade on mainnet")
	}

	if c.MinFundingRate <= 0 {
		return fmt.Errorf("MIN_FUNDING_RATE_PERCENT must be positive")
	}

	if c.USDTPerTrade <= 0 {
		return fmt.Errorf("USDT_PER_TRADE must be positive")
	}

	if c.Leverage <= 0 {
		return fmt.Errorf("LEVERAGE must be positive")
	}

	if c.Leverage != 1 {
		return fmt.Errorf("LEVERAGE must be 1 for the current live funding strategy")
	}

	if c.MarginMode != "isolated" {
		return fmt.Errorf("MARGIN_MODE must be isolated for the current live funding strategy")
	}

	if c.PlanMinuteUTC3 < 0 || c.PlanMinuteUTC3 > 59 {
		return fmt.Errorf("PLAN_MINUTE_UTC3 must be between 0 and 59")
	}

	if c.PreFundingMarkCapture <= 0 {
		return fmt.Errorf("PRE_FUNDING_MARK_CAPTURE_SEC must be positive")
	}

	if c.EntryConfirmTimeout <= 0 {
		return fmt.Errorf("ENTRY_CONFIRM_TIMEOUT_MS must be positive")
	}

	if c.ManualAlertAfterFunding <= 0 {
		return fmt.Errorf("MANUAL_ALERT_AFTER_FUNDING_SEC must be positive")
	}

	if c.AutoCloseAfterFunding <= c.ManualAlertAfterFunding {
		return fmt.Errorf("AUTO_CLOSE_AFTER_FUNDING_SEC must be greater than MANUAL_ALERT_AFTER_FUNDING_SEC")
	}

	if c.ScanInterval <= 0 {
		return fmt.Errorf("SCAN_INTERVAL_SEC must be positive")
	}

	return nil
}

func (c Config) PostgresDSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		c.PostgresHost,
		c.PostgresPort,
		c.PostgresUser,
		c.PostgresPassword,
		c.PostgresDB,
	)
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	s := bufio.NewScanner(f)

	for s.Scan() {
		line := strings.TrimSpace(s.Text())

		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)

		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"'`)

		_ = os.Setenv(key, val)
	}

	return s.Err()
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return strings.TrimSpace(v)
	}

	return def
}

func getBool(k string, def bool) bool {
	v := strings.ToLower(getenv(k, ""))

	if v == "" {
		return def
	}

	return v == "1" || v == "true" || v == "yes" || v == "y" || v == "on"
}

func getInt(k string, def int) int {
	v := getenv(k, "")

	if v == "" {
		return def
	}

	i, err := strconv.Atoi(strings.ReplaceAll(v, "_", ""))
	if err != nil {
		return def
	}

	return i
}

func getFloat(k string, def float64) float64 {
	v := getenv(k, "")

	if v == "" {
		return def
	}

	f, err := strconv.ParseFloat(strings.ReplaceAll(v, "_", ""), 64)
	if err != nil {
		return def
	}

	return f
}

func percentEnv(k string, defPercent float64) float64 {
	return getFloat(k, defPercent) / 100.0
}

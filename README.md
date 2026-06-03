# Fanding / Funding Bot

Mainnet-only funding scanner and live-ready architecture for USDT perpetual futures.

## Exchanges

The project is structured for 9 exchanges:

1. Binance
2. Bybit
3. BingX
4. Bitget
5. OKX
6. Gate
7. MEXC
8. KuCoin
9. HTX

Current archive focuses on monitor/paper infrastructure and public REST funding scanners. Live order methods are present in the unified interface and will be implemented/tested exchange-by-exchange.

## Strategy scope

- Positive funding only.
- Funding filter is applied to the next funding event itself, not normalized per hour.
- Display format: `+0.80% / 8h`, `+0.80% / 4h`, `+0.80% / 1h`.
- REST scanner is used for funding, volume, balances/status.
- WebSocket is planned for selected candidates only during the pre-funding/trade-control window.

## Safety

No testnet mode is used. The project is mainnet-only, protected by:

```env
BOT_MODE=monitor
ALLOW_MAINNET_LIVE=false
```

Live mode must be enabled explicitly:

```env
BOT_MODE=live
ALLOW_MAINNET_LIVE=true
```

## Dashboard

The frontend is now a separate file:

```text
web/index.html
```

Dashboard URL:

```text
http://localhost:8095
```

It includes:

- dynamic tabs for all enabled exchanges;
- compact connection/balance cards for 9 exchanges;
- 1h / 4h / 8h funding countdowns;
- UTC+3 local strategy clock;
- Apple-style light/dark UI;
- sortable funding tables;
- row limit and search;
- editable Planned Entry and Safe TP fields via `/api/candidate/override`.

## Run

```bash
cp .env.example .env
# fill Telegram and exchange keys if needed
docker compose up --build
```

Or locally:

```bash
go run ./cmd/bot -env .env
```

## Notes

This portable archive intentionally uses memory-store fallback for now, so it compiles without external Go dependencies. PostgreSQL persistence will be re-enabled in a later step when we add the repository layer and migrations for journal/overrides.

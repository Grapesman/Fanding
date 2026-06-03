# Project Status

## Current version

Unified mainnet-only, 9-exchange funding scanner scaffold.

## Implemented

- Universal `domain.Exchange` interface.
- Mainnet-only configuration for 9 exchanges.
- Universal `/api/state` with `exchanges: []`.
- Separate `web/index.html` dashboard.
- Compact status/balance cards for 9 exchanges.
- Dynamic exchange tabs.
- Positive funding display as `+0.80% / 8h`.
- Funding filter by next funding event rate.
- Editable candidate overrides in memory: `planned_entry`, `safe_tp_price`.
- Telegram polling retained from v2.
- Live-ready order interfaces retained.

## Important limitations

- New exchange adapters are public REST scanner scaffolds and must be validated against real exchange responses.
- Balances for new exchanges currently return zero until signed APIs are implemented.
- Live order methods for new exchanges return `not implemented`.
- PostgreSQL driver is disabled in this portable archive to keep builds dependency-free; memory store is used.
- WebSocket layer is planned but not yet wired into the funding strategy.

## Next steps

1. Validate each exchange public scanner with live API responses.
2. Re-enable PostgreSQL repository and migrations.
3. Implement private balance per exchange.
4. Implement Binance live orders first, then Bybit, then the remaining exchanges in agreed order.
5. Add WS only for selected candidates during pre-funding/trade-control window.

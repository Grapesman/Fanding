package engine

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"funding-bot/internal/config"
	"funding-bot/internal/domain"
	"funding-bot/internal/storage"
	"funding-bot/internal/telegram"
)

type Engine struct {
	cfg       config.Config
	exchanges []domain.Exchange
	store     *storage.Store
	tg        *telegram.Bot

	warned map[int64]bool

	lastPlanCycleKey string

	capturedMark map[string]bool
	entryStarted map[string]bool
	manualAlert  map[string]bool
	autoClosed   map[string]bool
}

func New(cfg config.Config, exchanges []domain.Exchange, store *storage.Store, tg *telegram.Bot) *Engine {
	return &Engine{
		cfg:       cfg,
		exchanges: exchanges,
		store:     store,
		tg:        tg,

		warned: map[int64]bool{},

		capturedMark: map[string]bool{},
		entryStarted: map[string]bool{},
		manualAlert:  map[string]bool{},
		autoClosed:   map[string]bool{},
	}
}

func (e *Engine) Run(ctx context.Context) error {
	e.refreshBalances()

	_ = e.tg.Startup(string(e.cfg.BotMode), e.store.Balances())

	scanTicker := time.NewTicker(e.cfg.ScanInterval)
	defer scanTicker.Stop()

	// Fast ticker is needed because entry must be triggered exactly at funding time.
	execTicker := time.NewTicker(200 * time.Millisecond)
	defer execTicker.Stop()

	if err := e.scanOnce(ctx); err != nil {
		log.Println("scan:", err)
		e.store.AddLog("scan error: " + err.Error())
	}

	e.maybeRunPlanningCycle(time.Now())

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-scanTicker.C:
			if err := e.scanOnce(ctx); err != nil {
				log.Println("scan:", err)
				e.store.AddLog("scan error: " + err.Error())
			}

			e.maybeRunPlanningCycle(time.Now())
			e.cleanupOldPlans()

		case <-execTicker.C:
			e.processPlannedTrades(time.Now())
			e.processActiveTrades(time.Now())
		}
	}
}

func (e *Engine) refreshBalances() {
	for _, ex := range e.exchanges {
		connected := ex.Connected()
		e.store.SetExchangeStatus(ex.Name(), connected, "")

		b, err := ex.Balance()
		if err != nil {
			e.store.SetExchangeStatus(ex.Name(), connected, err.Error())
			e.store.AddLog(fmt.Sprintf("%s balance error: %v", ex.Name(), err))
			continue
		}

		e.store.SetBalance(b)
	}
}

func (e *Engine) scanOnce(ctx context.Context) error {
	e.refreshBalances()

	all := []domain.Candidate{}

	for _, ex := range e.exchanges {
		candidates, err := e.fetchCandidatesWithTimeout(ex, e.exchangeScanTimeout(ex))
		if err != nil {
			connected := ex.Connected()

			if connected {
				e.store.SetExchangeStatus(ex.Name(), true, "")
			} else {
				e.store.SetExchangeStatus(ex.Name(), false, err.Error())
			}

			e.store.AddLog(fmt.Sprintf("%s candidates error: %v", ex.Name(), err))
			continue
		}

		e.store.SetExchangeStatus(ex.Name(), true, "")

		processed := e.processCandidates(candidates)
		all = append(all, processed...)
	}

	e.markCurrentlyPlannedCandidates(all)
	e.store.SetCandidates(all)

	return nil
}

func (e *Engine) processCandidates(in []domain.Candidate) []domain.Candidate {
	out := make([]domain.Candidate, 0, len(in))

	for _, c := range in {
		if c.NativeSymbol == "" {
			c.NativeSymbol = c.Symbol
		}

		if c.FundingIntervalHours <= 0 {
			c.FundingIntervalHours = 8
		}

		c.PassFunding = c.FundingRate >= e.cfg.MinFundingRate

		if e.cfg.OnlyPositiveFunding && c.FundingRate <= 0 {
			c.PassFunding = false
		}

		c.PassVolume = c.Volume24hUSDT >= e.cfg.Min24hVolumeUSDT

		if e.cfg.UseMaxVolumeFilter && c.Volume24hUSDT > e.cfg.Max24hVolumeUSDT {
			c.PassVolume = false
		}

		c.PassSpread = true
		if e.cfg.UseSpreadFilter && c.Spread > e.cfg.MaxSpread {
			c.PassSpread = false
		}

		ref := c.MarkPrice
		if ref <= 0 {
			ref = c.Price
		}

		c.RawFairPrice = ref * (1 - c.FundingRate)
		c.SafeTPPrice = c.RawFairPrice

		// Legacy dashboard field. The live strategy enters at funding time by market.
		if c.Price > 0 {
			c.PlannedEntry = c.Price
		}

		c.CanTradeLive = true
		c.LiveRejectReason = ""

		out = append(out, c)
	}

	return out
}

func (e *Engine) markCurrentlyPlannedCandidates(candidates []domain.Candidate) {
	plans := e.store.PlannedTrades()
	active := e.store.ActiveTrades()

	keys := map[string]bool{}

	for _, p := range plans {
		if p.Status == domain.TradePlanned ||
			p.Status == domain.TradeWaitingEntryMark ||
			p.Status == domain.TradeEntrySending ||
			p.Status == domain.TradeEntrySent {
			keys[tradeKey(p.Exchange, p.Symbol, p.FundingTime)] = true
		}
	}

	for _, t := range active {
		if t.Status != domain.TradeClosed &&
			t.Status != domain.TradeFinish &&
			t.Status != domain.TradeCancelled &&
			t.Status != domain.TradeError {
			keys[tradeKey(t.Exchange, t.Symbol, t.FundingTime)] = true
		}
	}

	for i := range candidates {
		if keys[tradeKey(candidates[i].Exchange, candidates[i].Symbol, candidates[i].NextFundingTime)] {
			candidates[i].Selected = true
		}
	}
}

func (e *Engine) maybeRunPlanningCycle(now time.Time) {
	utc3 := now.UTC().Add(3 * time.Hour)

	if utc3.Minute() != e.cfg.PlanMinuteUTC3 {
		return
	}

	cycleKey := utc3.Format("2006-01-02 15")

	if cycleKey == e.lastPlanCycleKey {
		return
	}

	e.lastPlanCycleKey = cycleKey

	e.store.AddLog("planning cycle started: " + cycleKey + " UTC+3")

	e.refreshBalances()

	_ = e.tg.BalanceSummary(e.store.Balances(), e.cfg.USDTPerTrade)

	summary := e.buildTradePlanSummary(now, cycleKey)

	e.store.SetPlannedTrades(flattenPlans(summary.ByExchange))

	_ = e.tg.TradePlanSummary(summary)

	e.store.AddLog(fmt.Sprintf(
		"planning cycle finished: %s total_planned=%d",
		cycleKey,
		summary.TotalPlanned,
	))
}

func (e *Engine) buildTradePlanSummary(now time.Time, cycleKey string) domain.TradePlanSummary {
	balances := e.store.Balances()
	candidates := e.store.Candidates()

	byExchangeCandidates := map[domain.ExchangeName][]domain.Candidate{}

	for _, c := range candidates {
		byExchangeCandidates[c.Exchange] = append(byExchangeCandidates[c.Exchange], c)
	}

	byExchangePlans := map[domain.ExchangeName][]domain.PlannedTrade{}
	total := 0

	for _, ex := range domain.AllExchangeNames {
		balance := balances[ex]

		capacity := e.tradeCapacity(balance)
		if capacity <= 0 {
			continue
		}

		selected := e.chooseTopN(ex, byExchangeCandidates[ex], balance, capacity)

		for _, c := range selected {
			plan := e.candidateToPlan(c, now, cycleKey)

			byExchangePlans[ex] = append(byExchangePlans[ex], plan)
			total++
		}
	}

	return domain.TradePlanSummary{
		CreatedAt:    now.UTC(),
		CycleKey:     cycleKey,
		TotalPlanned: total,
		ByExchange:   byExchangePlans,
		Balances:     balances,
	}
}

func (e *Engine) tradeCapacity(balance domain.Balance) int {
	if !balance.PrivateOK {
		return 0
	}

	if e.cfg.USDTPerTrade <= 0 {
		return 0
	}

	capacity := int(math.Floor(balance.AvailableUSDT / e.cfg.USDTPerTrade))

	if capacity < 0 {
		capacity = 0
	}

	if e.cfg.MaxActiveTradesPerExchange > 0 && capacity > e.cfg.MaxActiveTradesPerExchange {
		capacity = e.cfg.MaxActiveTradesPerExchange
	}

	return capacity
}

func (e *Engine) chooseTopN(
	exchange domain.ExchangeName,
	candidates []domain.Candidate,
	balance domain.Balance,
	limit int,
) []domain.Candidate {
	if limit <= 0 {
		return nil
	}

	filtered := []domain.Candidate{}

	for _, c := range candidates {
		if c.Exchange != exchange {
			continue
		}

		if !c.PassFunding || !c.PassVolume || !c.PassSpread {
			continue
		}

		if c.NextFundingTime.IsZero() || c.NextFundingTime.Before(time.Now().UTC()) {
			continue
		}

		if e.store.IsSkipped(c.Exchange, c.Symbol, c.NextFundingTime.Unix()) {
			continue
		}

		if e.store.HasPlannedOrActive(c.Exchange, c.Symbol, c.NextFundingTime) {
			continue
		}

		liveOK, reason := e.liveEligibility(c, balance)
		c.CanTradeLive = liveOK
		c.LiveRejectReason = reason

		// In monitor/paper mode we still show candidates even before all live exchange clients are implemented.
		// In live mode we require the live eligibility check to pass.
		if e.cfg.BotMode == config.ModeLive && !liveOK {
			continue
		}

		filtered = append(filtered, c)
	}

	if len(filtered) == 0 {
		return nil
	}

	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].FundingRate == filtered[j].FundingRate {
			return filtered[i].Volume24hUSDT > filtered[j].Volume24hUSDT
		}

		return filtered[i].FundingRate > filtered[j].FundingRate
	})

	if len(filtered) > limit {
		filtered = filtered[:limit]
	}

	return filtered
}

func (e *Engine) liveEligibility(c domain.Candidate, balance domain.Balance) (bool, string) {
	if !balance.PrivateOK {
		return false, "private balance is not OK"
	}

	if balance.AvailableUSDT < e.cfg.USDTPerTrade {
		return false, "insufficient available balance"
	}

	ex := e.exchangeByName(c.Exchange)
	if ex == nil {
		return false, "exchange client not found"
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		// During migration this is not a blocker unless BOT_MODE=live.
		if e.cfg.BotMode == config.ModeLive {
			return false, "exchange does not implement LiveExchange yet"
		}

		return true, ""
	}

	rules, err := liveEx.SymbolRules(symbolForExchange(c))
	if err != nil {
		return false, "symbol rules error: " + err.Error()
	}

	refPrice := c.MarkPrice
	if refPrice <= 0 {
		refPrice = c.Price
	}
	if refPrice <= 0 {
		return false, "price is zero"
	}

	estimatedQty := e.cfg.USDTPerTrade / refPrice
	estimatedNotional := e.cfg.USDTPerTrade

	if rules.MinNotional > 0 && estimatedNotional < rules.MinNotional {
		return false, fmt.Sprintf("min notional %.4f USDT > position %.4f USDT", rules.MinNotional, estimatedNotional)
	}

	if rules.MinQty > 0 && estimatedQty < rules.MinQty && !rules.SupportsNotionalMarketOrder {
		return false, fmt.Sprintf("min qty %.12f > estimated qty %.12f", rules.MinQty, estimatedQty)
	}

	return true, ""
}

func (e *Engine) candidateToPlan(c domain.Candidate, now time.Time, cycleKey string) domain.PlannedTrade {
	fundingRate := c.FundingRate

	ref := c.MarkPrice
	if ref <= 0 {
		ref = c.Price
	}

	fair := ref * (1 - fundingRate)
	scenario2Shift := fundingRate / 2
	scenario3Offset := fundingRate / 3

	id := makePlanID(c.Exchange, c.Symbol, c.NextFundingTime)

	return domain.PlannedTrade{
		ID:           id,
		Exchange:     c.Exchange,
		Symbol:       c.Symbol,
		NativeSymbol: c.NativeSymbol,

		Scenario: domain.ScenarioUnknown,
		Status:   domain.TradePlanned,

		FundingRate:          fundingRate,
		FundingIntervalHours: nonZero(c.FundingIntervalHours, 8),
		FundingTime:          c.NextFundingTime.UTC(),

		PositionUSDT: e.cfg.USDTPerTrade,
		Leverage:     e.cfg.Leverage,
		MarginMode:   e.cfg.MarginMode,

		PreFundingMarkPrice: 0,
		FairPrice:           fair,

		Scenario2ExtraShift: scenario2Shift,
		Scenario2TPPrice:    fair * (1 - scenario2Shift),
		Scenario3Offset:     scenario3Offset,
		Scenario3BuyPrice:   fair * (1 - scenario3Offset),
		Scenario3SellPrice:  fair * (1 + scenario3Offset),

		EstimatedQty:      estimatedQty(c, e.cfg.USDTPerTrade),
		EstimatedNotional: e.cfg.USDTPerTrade,
		MinOrderUSDT:      c.MinOrderUSDT,
		LiveRejectReason:  c.LiveRejectReason,
		CanTradeLive:      c.CanTradeLive,
		PlanningCycleKey:  cycleKey,
		IdempotencyKey:    id,
		CreatedFromScanAt: c.UpdatedAt,
		PlannedAt:         now.UTC(),
		EntryAt:           c.NextFundingTime.UTC(),
		ManualAlertAt:     c.NextFundingTime.UTC().Add(e.cfg.ManualAlertAfterFunding),
		AutoCloseAt:       c.NextFundingTime.UTC().Add(e.cfg.AutoCloseAfterFunding),
		UpdatedAt:         now.UTC(),
	}
}

func (e *Engine) processPlannedTrades(now time.Time) {
	plans := e.store.PlannedTrades()

	for _, plan := range plans {
		if plan.ID == "" {
			continue
		}

		if plan.Status == domain.TradeCancelled ||
			plan.Status == domain.TradeSkipped ||
			plan.Status == domain.TradeError ||
			plan.Status == domain.TradeClosed {
			continue
		}

		// T - 1 sec: capture pre-funding mark price and calculate fair price.
		if !e.capturedMark[plan.ID] &&
			!plan.FundingTime.IsZero() &&
			now.UTC().After(plan.FundingTime.Add(-e.cfg.PreFundingMarkCapture)) &&
			now.UTC().Before(plan.FundingTime.Add(2*time.Second)) {
			e.capturePreFundingMark(plan)
			e.capturedMark[plan.ID] = true
			continue
		}

		// T: exact funding time entry.
		if !e.entryStarted[plan.ID] &&
			!plan.FundingTime.IsZero() &&
			!now.UTC().Before(plan.FundingTime) {
			e.entryStarted[plan.ID] = true
			go e.executeFundingEntry(plan)
		}
	}
}

func (e *Engine) capturePreFundingMark(plan domain.PlannedTrade) {
	c := e.latestCandidate(plan.Exchange, plan.Symbol)

	ref := c.MarkPrice
	if ref <= 0 {
		ref = c.Price
	}
	if ref <= 0 {
		ref = plan.FairPrice / (1 - plan.FundingRate)
	}

	if ref <= 0 {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "could not capture pre-funding mark price")
		e.store.AddLog(fmt.Sprintf("capture mark failed %s %s", plan.Exchange, plan.Symbol))
		return
	}

	plan.PreFundingMarkPrice = ref
	plan.FairPrice = ref * (1 - plan.FundingRate)

	plan.Scenario2ExtraShift = plan.FundingRate / 2
	plan.Scenario2TPPrice = plan.FairPrice * (1 - plan.Scenario2ExtraShift)

	plan.Scenario3Offset = plan.FundingRate / 3
	plan.Scenario3BuyPrice = plan.FairPrice * (1 - plan.Scenario3Offset)
	plan.Scenario3SellPrice = plan.FairPrice * (1 + plan.Scenario3Offset)

	plan.Status = domain.TradeWaitingEntryMark
	plan.UpdatedAt = time.Now().UTC()

	e.store.AddPlannedTrade(plan)

	e.store.AddLog(fmt.Sprintf(
		"captured pre-funding mark %s %s mark=%.12f fair=%.12f",
		plan.Exchange,
		plan.Symbol,
		plan.PreFundingMarkPrice,
		plan.FairPrice,
	))
}

func (e *Engine) executeFundingEntry(plan domain.PlannedTrade) {
	if e.cfg.BotMode != config.ModeLive {
		e.store.AddLog(fmt.Sprintf(
			"entry reached for %s %s, mode=%s: live order not sent",
			plan.Exchange,
			plan.Symbol,
			e.cfg.BotMode,
		))
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeSkipped, "not live mode")
		return
	}

	if !e.cfg.AllowMainnetLive {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "ALLOW_MAINNET_LIVE is false")
		e.store.AddLog(fmt.Sprintf("live blocked for %s %s: ALLOW_MAINNET_LIVE=false", plan.Exchange, plan.Symbol))
		return
	}

	if !e.silentPreEntryCheck(plan) {
		return
	}

	ex := e.exchangeByName(plan.Exchange)
	if ex == nil {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "exchange client not found")
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "exchange does not implement LiveExchange")
		e.store.AddLog(fmt.Sprintf("%s %s cannot enter: LiveExchange not implemented", plan.Exchange, plan.Symbol))
		return
	}

	if err := liveEx.SetMarginAndLeverage(symbolForPlan(plan), plan.Leverage, plan.MarginMode); err != nil {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "set margin/leverage error: "+err.Error())
		e.store.AddLog(fmt.Sprintf("%s %s set margin/leverage error: %v", plan.Exchange, plan.Symbol, err))
		return
	}

	e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeEntrySending, "")

	req := domain.OrderRequest{
		Exchange:      plan.Exchange,
		Symbol:        plan.Symbol,
		NativeSymbol:  symbolForPlan(plan),
		Side:          string(domain.OrderSideSell),
		PositionSide:  string(domain.SideShort),
		Type:          string(domain.OrderTypeMarket),
		MarginUSDT:    plan.PositionUSDT,
		NotionalUSDT:  plan.PositionUSDT,
		Leverage:      plan.Leverage,
		MarginMode:    plan.MarginMode,
		ReduceOnly:    false,
		ClientOrderID: plan.ID + ":entry",
		TradeID:       plan.ID,
		PlanID:        plan.ID,
		Scenario:      domain.ScenarioOne,
		CreatedAt:     time.Now().UTC(),
	}

	entrySentAt := time.Now().UTC()

	resultCh := make(chan domain.OrderResult, 1)
	errCh := make(chan error, 1)

	go func() {
		res, err := liveEx.PlaceMarketShort(req)
		if err != nil {
			errCh <- err
			return
		}

		resultCh <- res
	}()

	select {
	case res := <-resultCh:
		e.handleEntryConfirmed(plan, res, entrySentAt, false)

	case err := <-errCh:
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeEntryFailed, err.Error())
		e.store.AddLog(fmt.Sprintf("%s %s entry market error: %v", plan.Exchange, plan.Symbol, err))
		e.startScenario3(plan)

	case <-time.After(e.cfg.EntryConfirmTimeout):
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeEntrySent, "entry confirmation timeout")
		e.store.AddLog(fmt.Sprintf(
			"%s %s entry confirmation timeout after %s, starting scenario 3",
			plan.Exchange,
			plan.Symbol,
			e.cfg.EntryConfirmTimeout,
		))

		e.startScenario3(plan)

		// Late confirmation watcher.
		go e.waitLateEntryConfirmation(plan, liveEx, req.ClientOrderID, entrySentAt)
	}
}

func (e *Engine) silentPreEntryCheck(plan domain.PlannedTrade) bool {
	balance, ok := e.store.Balance(plan.Exchange)
	if !ok || !balance.PrivateOK {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "private balance is not OK before entry")
		return false
	}

	if balance.AvailableUSDT < plan.PositionUSDT {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "insufficient available balance before entry")
		return false
	}

	// User decision: if funding drops below 0.50% during the last seconds,
	// bot still enters if the plan was already created.
	c := e.latestCandidate(plan.Exchange, plan.Symbol)

	if !c.NextFundingTime.IsZero() && !sameSecond(c.NextFundingTime, plan.FundingTime) {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "funding time changed before entry")
		return false
	}

	if e.cfg.UseSpreadFilter && c.Spread > e.cfg.MaxSpread {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "spread too high before entry")
		return false
	}

	return true
}

func (e *Engine) handleEntryConfirmed(
	plan domain.PlannedTrade,
	res domain.OrderResult,
	entrySentAt time.Time,
	late bool,
) {
	filledQty := res.FilledQty
	if filledQty <= 0 {
		filledQty = res.Qty
	}

	filledUSDT := res.FilledNotional
	if filledUSDT <= 0 && filledQty > 0 {
		filledUSDT = filledQty * firstNonZero(res.AvgPrice, res.Price, plan.PreFundingMarkPrice)
	}

	entryPrice := firstNonZero(res.AvgPrice, res.Price, plan.PreFundingMarkPrice)

	scenario := domain.ScenarioOne
	status := domain.TradeWaitingTP
	tpPrice := plan.FairPrice

	if late {
		scenario = domain.ScenarioTwo
		status = domain.TradeWaitingTP
		tpPrice = plan.Scenario2TPPrice
	}

	active := domain.ActiveTrade{
		PlanID:       plan.ID,
		TradeID:      plan.ID,
		Exchange:     plan.Exchange,
		Symbol:       plan.Symbol,
		NativeSymbol: plan.NativeSymbol,

		Scenario: scenario,
		Side:     domain.SideShort,
		Status:   status,

		FundingRate:          plan.FundingRate,
		FundingIntervalHours: plan.FundingIntervalHours,
		FundingTime:          plan.FundingTime,

		PositionUSDT: plan.PositionUSDT,
		Leverage:     plan.Leverage,
		MarginMode:   plan.MarginMode,

		PreFundingMarkPrice: plan.PreFundingMarkPrice,
		FairPrice:           plan.FairPrice,

		Scenario2ExtraShift: plan.Scenario2ExtraShift,
		Scenario2TPPrice:    plan.Scenario2TPPrice,
		Scenario3Offset:     plan.Scenario3Offset,
		Scenario3BuyPrice:   plan.Scenario3BuyPrice,
		Scenario3SellPrice:  plan.Scenario3SellPrice,

		EntryOrderID:       res.ExchangeOrderID,
		EntryClientOrderID: res.ClientOrderID,
		EntryOrderStatus:   res.OrderStatus,
		EntryPrice:         entryPrice,
		EntryAvgPrice:      entryPrice,
		EntryRequestedQty:  res.Qty,
		EntryFilledQty:     filledQty,
		EntryFilledUSDT:    filledUSDT,
		EntrySentAt:        entrySentAt,
		EntryConfirmedAt:   time.Now().UTC(),

		TakeProfitPrice: tpPrice,

		Qty:        filledQty,
		Commission: res.Fee,
		OpenedAt:   time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
	}

	e.store.PromotePlannedToActive(plan, active)

	e.store.AddLog(fmt.Sprintf(
		"entry confirmed %s %s scenario=%s qty=%.12f entry=%.12f tp=%.12f late=%v",
		plan.Exchange,
		plan.Symbol,
		active.Scenario,
		active.Qty,
		active.EntryAvgPrice,
		active.TakeProfitPrice,
		late,
	))

	e.placeTakeProfit(active)
}

func (e *Engine) placeTakeProfit(active domain.ActiveTrade) {
	ex := e.exchangeByName(active.Exchange)
	if ex == nil {
		e.store.UpdateActiveTradeStatus(active.TradeID, domain.TradeError, "exchange client not found for TP")
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		e.store.UpdateActiveTradeStatus(active.TradeID, domain.TradeError, "exchange does not implement LiveExchange for TP")
		return
	}

	req := domain.OrderRequest{
		Exchange:      active.Exchange,
		Symbol:        active.Symbol,
		NativeSymbol:  symbolForActive(active),
		Side:          string(domain.OrderSideBuy),
		PositionSide:  string(domain.SideShort),
		Type:          string(domain.OrderTypeLimit),
		Price:         active.TakeProfitPrice,
		Qty:           active.Qty,
		MarginUSDT:    active.EntryFilledUSDT,
		NotionalUSDT:  active.EntryFilledUSDT,
		Leverage:      active.Leverage,
		MarginMode:    active.MarginMode,
		ReduceOnly:    true,
		ClientOrderID: active.TradeID + ":tp",
		TradeID:       active.TradeID,
		PlanID:        active.PlanID,
		Scenario:      active.Scenario,
		TimeInForce:   "GTC",
		CreatedAt:     time.Now().UTC(),
	}

	res, err := liveEx.PlaceLimitReduceOnly(req)
	if err != nil {
		active.LastError = "place TP error: " + err.Error()
		active.UpdatedAt = time.Now().UTC()
		_ = e.store.UpdateActiveTrade(active)
		e.store.AddLog(fmt.Sprintf("%s %s TP place error: %v", active.Exchange, active.Symbol, err))
		return
	}

	active.TPOrderID = res.ExchangeOrderID
	active.TPClientOrderID = res.ClientOrderID
	active.TPOrderStatus = res.OrderStatus
	active.Status = domain.TradeWaitingTP
	active.UpdatedAt = time.Now().UTC()

	_ = e.store.UpdateActiveTrade(active)

	e.store.AddLog(fmt.Sprintf(
		"TP placed %s %s price=%.12f qty=%.12f",
		active.Exchange,
		active.Symbol,
		active.TakeProfitPrice,
		active.Qty,
	))
}

func (e *Engine) startScenario3(plan domain.PlannedTrade) {
	ex := e.exchangeByName(plan.Exchange)
	if ex == nil {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "exchange client not found for scenario 3")
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "exchange does not implement LiveExchange for scenario 3")
		return
	}

	qty := plan.EstimatedQty
	if qty <= 0 {
		qty = estimatedQtyFromPrice(plan.PositionUSDT, plan.PreFundingMarkPrice)
	}

	if qty <= 0 {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "cannot estimate scenario 3 qty")
		return
	}

	buyReq := domain.OrderRequest{
		Exchange:      plan.Exchange,
		Symbol:        plan.Symbol,
		NativeSymbol:  symbolForPlan(plan),
		Side:          string(domain.OrderSideBuy),
		Type:          string(domain.OrderTypeLimit),
		Price:         plan.Scenario3BuyPrice,
		Qty:           qty,
		MarginUSDT:    plan.PositionUSDT,
		NotionalUSDT:  plan.PositionUSDT,
		Leverage:      plan.Leverage,
		MarginMode:    plan.MarginMode,
		ReduceOnly:    false,
		ClientOrderID: plan.ID + ":s3buy",
		TradeID:       plan.ID,
		PlanID:        plan.ID,
		Scenario:      domain.ScenarioThree,
		TimeInForce:   "GTC",
		CreatedAt:     time.Now().UTC(),
	}

	sellReq := buyReq
	sellReq.Side = string(domain.OrderSideSell)
	sellReq.Price = plan.Scenario3SellPrice
	sellReq.ClientOrderID = plan.ID + ":s3sell"

	buyRes, buyErr := liveEx.PlaceLimitReduceOnly(buyReq)
	if buyErr != nil {
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "scenario 3 BUY place error: "+buyErr.Error())
		return
	}

	sellRes, sellErr := liveEx.PlaceLimitReduceOnly(sellReq)
	if sellErr != nil {
		_ = liveEx.CancelOrder(symbolForPlan(plan), buyRes.ExchangeOrderID)
		e.store.UpdatePlannedTradeStatus(plan.ID, domain.TradeError, "scenario 3 SELL place error: "+sellErr.Error())
		return
	}

	active := domain.ActiveTrade{
		PlanID:       plan.ID,
		TradeID:      plan.ID,
		Exchange:     plan.Exchange,
		Symbol:       plan.Symbol,
		NativeSymbol: plan.NativeSymbol,

		Scenario: domain.ScenarioThree,
		Status:   domain.TradeScenario3BracketPlaced,

		FundingRate:          plan.FundingRate,
		FundingIntervalHours: plan.FundingIntervalHours,
		FundingTime:          plan.FundingTime,

		PositionUSDT: plan.PositionUSDT,
		Leverage:     plan.Leverage,
		MarginMode:   plan.MarginMode,

		PreFundingMarkPrice: plan.PreFundingMarkPrice,
		FairPrice:           plan.FairPrice,

		Scenario2ExtraShift: plan.Scenario2ExtraShift,
		Scenario2TPPrice:    plan.Scenario2TPPrice,
		Scenario3Offset:     plan.Scenario3Offset,
		Scenario3BuyPrice:   plan.Scenario3BuyPrice,
		Scenario3SellPrice:  plan.Scenario3SellPrice,

		Scenario3BuyOrderID:  buyRes.ExchangeOrderID,
		Scenario3SellOrderID: sellRes.ExchangeOrderID,
		Scenario3BuyStatus:   buyRes.OrderStatus,
		Scenario3SellStatus:  sellRes.OrderStatus,

		Qty:       qty,
		OpenedAt:  time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}

	e.store.PromotePlannedToActive(plan, active)

	e.store.AddLog(fmt.Sprintf(
		"scenario 3 bracket placed %s %s buy=%.12f sell=%.12f qty=%.12f",
		plan.Exchange,
		plan.Symbol,
		plan.Scenario3BuyPrice,
		plan.Scenario3SellPrice,
		qty,
	))
}

func (e *Engine) waitLateEntryConfirmation(
	plan domain.PlannedTrade,
	liveEx domain.LiveExchange,
	clientOrderID string,
	entrySentAt time.Time,
) {
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)

		res, err := liveEx.GetOrderStatus(symbolForPlan(plan), clientOrderID)
		if err != nil {
			continue
		}

		if res.OrderStatus == domain.OrderStatusFilled ||
			res.OrderStatus == domain.OrderStatusPartiallyFill ||
			strings.EqualFold(res.Status, "FILLED") ||
			strings.EqualFold(res.Status, "PARTIALLY_FILLED") {
			e.cancelScenario3OrdersIfAny(plan.ID)
			e.handleEntryConfirmed(plan, res, entrySentAt, true)
			return
		}
	}
}

func (e *Engine) cancelScenario3OrdersIfAny(tradeID string) {
	active, ok := e.store.FindActiveTrade(tradeID)
	if !ok {
		return
	}

	ex := e.exchangeByName(active.Exchange)
	if ex == nil {
		return
	}

	if active.Scenario3BuyOrderID != "" {
		_ = ex.CancelOrder(symbolForActive(active), active.Scenario3BuyOrderID)
	}

	if active.Scenario3SellOrderID != "" {
		_ = ex.CancelOrder(symbolForActive(active), active.Scenario3SellOrderID)
	}

	e.store.UpdateActiveTradeStatus(tradeID, domain.TradeEntryLateConfirm, "late entry confirmed; scenario 3 orders cancelled")
}

func (e *Engine) processActiveTrades(now time.Time) {
	trades := e.store.ActiveTrades()

	for _, t := range trades {
		if t.TradeID == "" {
			continue
		}

		e.refreshActivePNL(t)

		e.checkFundingFeeAndMaybeScenario2(t)
		e.checkLimitClose(t)
		e.checkScenario3Orders(t)

		if !e.manualAlert[t.TradeID] && now.UTC().After(t.FundingTime.Add(e.cfg.ManualAlertAfterFunding)) {
			e.manualAlert[t.TradeID] = true
			e.sendManualAlert(t)
		}

		if !e.autoClosed[t.TradeID] && now.UTC().After(t.FundingTime.Add(e.cfg.AutoCloseAfterFunding)) {
			e.autoClosed[t.TradeID] = true
			e.autoCloseTrade(t)
		}
	}
}

func (e *Engine) refreshActivePNL(t domain.ActiveTrade) {
	ex := e.exchangeByName(t.Exchange)
	if ex == nil {
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		return
	}

	pnl, err := liveEx.GetUnrealizedPNL(symbolForActive(t))
	if err != nil {
		return
	}

	_ = e.store.UpdateActiveTradePNL(
		t.TradeID,
		0,
		pnl.MarkPrice,
		pnl.UnrealizedPNL,
	)
}

func (e *Engine) checkFundingFeeAndMaybeScenario2(t domain.ActiveTrade) {
	if t.FundingFeeChecked || t.Status == domain.TradeClosed {
		return
	}

	// Architecture for Scenario A is here.
	// Current agreed behavior: Scenario B/V — first place scenario 1 TP,
	// then if funding fee is detected, move TP lower by funding_rate/2.
	ex := e.exchangeByName(t.Exchange)
	if ex == nil {
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		return
	}

	from := t.FundingTime.Add(-30 * time.Second)
	to := time.Now().UTC().Add(10 * time.Second)

	fees, err := liveEx.FundingFees(symbolForActive(t), from, to)
	if err != nil {
		return
	}

	if len(fees) == 0 {
		return
	}

	feeSum := 0.0
	asset := "USDT"
	feeTime := time.Now().UTC()

	for _, f := range fees {
		feeSum += f.Amount
		if f.Asset != "" {
			asset = f.Asset
		}
		if !f.FeeTime.IsZero() {
			feeTime = f.FeeTime
		}
	}

	t.FundingFeeDetected = true
	t.FundingFeeChecked = true
	t.FundingFee = feeSum
	t.FundingFeeAsset = asset
	t.FundingFeeAt = &feeTime

	if t.Scenario == domain.ScenarioOne && t.Status == domain.TradeWaitingTP {
		e.moveTPToScenario2(t)
		return
	}

	_ = e.store.UpdateActiveTrade(t)
}

func (e *Engine) moveTPToScenario2(t domain.ActiveTrade) {
	ex := e.exchangeByName(t.Exchange)
	if ex == nil {
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		return
	}

	if t.TPOrderID != "" {
		_ = liveEx.CancelOrder(symbolForActive(t), t.TPOrderID)
	}

	t.Scenario = domain.ScenarioTwo
	t.TakeProfitPrice = t.Scenario2TPPrice
	t.Status = domain.TradeWaitingTP
	t.UpdatedAt = time.Now().UTC()

	_ = e.store.UpdateActiveTrade(t)

	e.placeTakeProfit(t)

	e.store.AddLog(fmt.Sprintf(
		"scenario 2 TP moved %s %s new_tp=%.12f funding_fee=%.8f",
		t.Exchange,
		t.Symbol,
		t.Scenario2TPPrice,
		t.FundingFee,
	))
}

func (e *Engine) checkLimitClose(t domain.ActiveTrade) {
	if t.TPOrderID == "" {
		return
	}

	ex := e.exchangeByName(t.Exchange)
	if ex == nil {
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		return
	}

	res, err := liveEx.GetOrderStatus(symbolForActive(t), t.TPOrderID)
	if err != nil {
		return
	}

	if !orderFilled(res) {
		return
	}

	closed := e.closedTradeFromActive(t, res)

	switch t.Scenario {
	case domain.ScenarioTwo:
		closed.CloseReason = domain.CloseReasonScenario2LimitFilled
	case domain.ScenarioThree:
		closed.CloseReason = domain.CloseReasonScenario3ClosedAtFair
	default:
		closed.CloseReason = domain.CloseReasonScenario1LimitFilled
	}

	_ = e.store.CloseActiveTrade(t.TradeID, closed)
	_ = e.tg.TradeClosed(closed)
}

func (e *Engine) checkScenario3Orders(t domain.ActiveTrade) {
	if t.Scenario != domain.ScenarioThree || t.Status != domain.TradeScenario3BracketPlaced {
		return
	}

	ex := e.exchangeByName(t.Exchange)
	if ex == nil {
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		return
	}

	buyFilled := false
	sellFilled := false

	var buyRes domain.OrderResult
	var sellRes domain.OrderResult

	if t.Scenario3BuyOrderID != "" {
		if res, err := liveEx.GetOrderStatus(symbolForActive(t), t.Scenario3BuyOrderID); err == nil {
			buyRes = res
			buyFilled = orderFilled(res)
		}
	}

	if t.Scenario3SellOrderID != "" {
		if res, err := liveEx.GetOrderStatus(symbolForActive(t), t.Scenario3SellOrderID); err == nil {
			sellRes = res
			sellFilled = orderFilled(res)
		}
	}

	if buyFilled && sellFilled {
		e.store.UpdateActiveTradeStatus(t.TradeID, domain.TradeScenario3Conflict, "both scenario 3 orders filled")
		e.marketCloseConflict(t)
		return
	}

	if buyFilled {
		if t.Scenario3SellOrderID != "" {
			_ = liveEx.CancelOrder(symbolForActive(t), t.Scenario3SellOrderID)
		}

		t.Status = domain.TradeScenario3BuyFilled
		t.Side = domain.SideLong
		t.EntryOrderID = buyRes.ExchangeOrderID
		t.EntryAvgPrice = firstNonZero(buyRes.AvgPrice, buyRes.Price, t.Scenario3BuyPrice)
		t.Qty = firstNonZero(buyRes.FilledQty, buyRes.Qty, t.Qty)
		t.TakeProfitPrice = t.FairPrice
		t.UpdatedAt = time.Now().UTC()

		_ = e.store.UpdateActiveTrade(t)

		e.placeScenario3FairClose(t, domain.OrderSideSell)
		return
	}

	if sellFilled {
		if t.Scenario3BuyOrderID != "" {
			_ = liveEx.CancelOrder(symbolForActive(t), t.Scenario3BuyOrderID)
		}

		t.Status = domain.TradeScenario3SellFilled
		t.Side = domain.SideShort
		t.EntryOrderID = sellRes.ExchangeOrderID
		t.EntryAvgPrice = firstNonZero(sellRes.AvgPrice, sellRes.Price, t.Scenario3SellPrice)
		t.Qty = firstNonZero(sellRes.FilledQty, sellRes.Qty, t.Qty)
		t.TakeProfitPrice = t.FairPrice
		t.UpdatedAt = time.Now().UTC()

		_ = e.store.UpdateActiveTrade(t)

		e.placeScenario3FairClose(t, domain.OrderSideBuy)
		return
	}
}

func (e *Engine) placeScenario3FairClose(t domain.ActiveTrade, side domain.OrderSide) {
	ex := e.exchangeByName(t.Exchange)
	if ex == nil {
		return
	}

	liveEx, ok := ex.(domain.LiveExchange)
	if !ok {
		return
	}

	req := domain.OrderRequest{
		Exchange:      t.Exchange,
		Symbol:        t.Symbol,
		NativeSymbol:  symbolForActive(t),
		Side:          string(side),
		Type:          string(domain.OrderTypeLimit),
		Price:         t.FairPrice,
		Qty:           t.Qty,
		MarginUSDT:    t.PositionUSDT,
		NotionalUSDT:  t.PositionUSDT,
		Leverage:      t.Leverage,
		MarginMode:    t.MarginMode,
		ReduceOnly:    true,
		ClientOrderID: t.TradeID + ":s3fair",
		TradeID:       t.TradeID,
		PlanID:        t.PlanID,
		Scenario:      domain.ScenarioThree,
		TimeInForce:   "GTC",
		CreatedAt:     time.Now().UTC(),
	}

	res, err := liveEx.PlaceLimitReduceOnly(req)
	if err != nil {
		t.LastError = "scenario 3 fair close error: " + err.Error()
		t.UpdatedAt = time.Now().UTC()
		_ = e.store.UpdateActiveTrade(t)
		return
	}

	t.TPOrderID = res.ExchangeOrderID
	t.TPClientOrderID = res.ClientOrderID
	t.TPOrderStatus = res.OrderStatus
	t.Status = domain.TradeWaitingTP
	t.UpdatedAt = time.Now().UTC()

	_ = e.store.UpdateActiveTrade(t)
}

func (e *Engine) sendManualAlert(t domain.ActiveTrade) {
	active, ok := e.store.FindActiveTrade(t.TradeID)
	if ok {
		t = active
	}

	e.store.MarkManualAlertSent(t.TradeID)
	_ = e.tg.StillOpenAlert(t)
}

func (e *Engine) autoCloseTrade(t domain.ActiveTrade) {
	active, ok := e.store.FindActiveTrade(t.TradeID)
	if ok {
		t = active
	}

	closed, err := e.cancelOrdersAndCloseMarket(t, domain.CloseReasonAutoMarketClose)
	if err != nil {
		e.store.UpdateActiveTradeStatus(t.TradeID, domain.TradeError, "auto close error: "+err.Error())
		_ = e.tg.TradeError(t, "auto close error: "+err.Error())
		return
	}

	_ = e.store.CloseActiveTrade(t.TradeID, closed)
	_ = e.tg.AutoMarketClosed(closed)
}

func (e *Engine) marketCloseConflict(t domain.ActiveTrade) {
	closed, err := e.cancelOrdersAndCloseMarket(t, domain.CloseReasonScenario3ConflictMarket)
	if err != nil {
		e.store.UpdateActiveTradeStatus(t.TradeID, domain.TradeError, "scenario 3 conflict close error: "+err.Error())
		_ = e.tg.TradeError(t, "scenario 3 conflict close error: "+err.Error())
		return
	}

	_ = e.store.CloseActiveTrade(t.TradeID, closed)
	_ = e.tg.TradeClosed(closed)
}

func (e *Engine) cancelOrdersAndCloseMarket(
	t domain.ActiveTrade,
	reason domain.CloseReason,
) (domain.ClosedTrade, error) {
	ex := e.exchangeByName(t.Exchange)
	if ex == nil {
		return domain.ClosedTrade{}, fmt.Errorf("exchange client not found")
	}

	if t.TPOrderID != "" {
		_ = ex.CancelOrder(symbolForActive(t), t.TPOrderID)
	}
	if t.Scenario3BuyOrderID != "" {
		_ = ex.CancelOrder(symbolForActive(t), t.Scenario3BuyOrderID)
	}
	if t.Scenario3SellOrderID != "" {
		_ = ex.CancelOrder(symbolForActive(t), t.Scenario3SellOrderID)
	}

	if err := ex.ClosePositionMarket(symbolForActive(t)); err != nil {
		return domain.ClosedTrade{}, err
	}

	// The next stage will improve this by reading actual close order / position history.
	exitPrice := firstNonZero(t.MarkPrice, t.CurrentPrice, t.TakeProfitPrice, t.FairPrice)

	closed := domain.ClosedTrade{
		ActiveTrade:  t,
		ExitPrice:    exitPrice,
		ExitAvgPrice: exitPrice,
		ExitQty:      t.Qty,
		ExitUSDT:     exitPrice * t.Qty,
		ClosedAt:     time.Now().UTC(),
		CloseReason:  reason,
	}

	closed.GrossPNL = calculateGrossPNL(t, exitPrice)
	closed.NetPNL = closed.GrossPNL + t.FundingFee - t.Commission

	return closed, nil
}

func (e *Engine) closedTradeFromActive(t domain.ActiveTrade, exit domain.OrderResult) domain.ClosedTrade {
	exitPrice := firstNonZero(exit.AvgPrice, exit.Price, t.TakeProfitPrice, t.FairPrice)

	closed := domain.ClosedTrade{
		ActiveTrade: t,

		ExitOrderID:     exit.ExchangeOrderID,
		ExitOrderStatus: exit.OrderStatus,
		ExitPrice:       exitPrice,
		ExitAvgPrice:    exitPrice,
		ExitQty:         firstNonZero(exit.FilledQty, exit.Qty, t.Qty),
		ExitUSDT:        firstNonZero(exit.FilledNotional, exitPrice*t.Qty),

		ClosedAt: time.Now().UTC(),
	}

	closed.Commission += exit.Fee
	closed.GrossPNL = calculateGrossPNL(t, exitPrice)
	closed.NetPNL = closed.GrossPNL + t.FundingFee - closed.Commission

	return closed
}

func calculateGrossPNL(t domain.ActiveTrade, exitPrice float64) float64 {
	entry := firstNonZero(t.EntryAvgPrice, t.EntryPrice)

	if entry <= 0 || exitPrice <= 0 || t.Qty <= 0 {
		return 0
	}

	switch t.Side {
	case domain.SideLong:
		return (exitPrice - entry) * t.Qty
	default:
		return (entry - exitPrice) * t.Qty
	}
}

func (e *Engine) latestCandidate(exchange domain.ExchangeName, symbol string) domain.Candidate {
	rows := e.store.CandidatesByExchange(exchange)

	for _, c := range rows {
		if c.Symbol == symbol || c.NativeSymbol == symbol {
			return c
		}
	}

	return domain.Candidate{}
}

func (e *Engine) exchangeByName(name domain.ExchangeName) domain.Exchange {
	for _, ex := range e.exchanges {
		if ex.Name() == name {
			return ex
		}
	}

	return nil
}

func (e *Engine) cleanupOldPlans() {
	removed := e.store.CleanupOldPlans(time.Now().UTC().Add(-30 * time.Minute))
	if removed > 0 {
		e.store.AddLog(fmt.Sprintf("old planned trades cleaned: %d", removed))
	}
}

type candidateResult struct {
	candidates []domain.Candidate
	err        error
}

func (e *Engine) exchangeScanTimeout(ex domain.Exchange) time.Duration {
	switch ex.Name() {
	case domain.ExchangeBingX:
		return 180 * time.Second
	case domain.ExchangeOKX:
		return 90 * time.Second
	default:
		return 30 * time.Second
	}
}

func (e *Engine) fetchCandidatesWithTimeout(ex domain.Exchange, timeout time.Duration) ([]domain.Candidate, error) {
	ch := make(chan candidateResult, 1)

	go func() {
		candidates, err := ex.FundingCandidates()
		ch <- candidateResult{
			candidates: candidates,
			err:        err,
		}
	}()

	select {
	case res := <-ch:
		return res.candidates, res.err
	case <-time.After(timeout):
		return nil, fmt.Errorf("%s funding candidates timeout after %s", ex.Name(), timeout)
	}
}

func flattenPlans(m map[domain.ExchangeName][]domain.PlannedTrade) []domain.PlannedTrade {
	out := []domain.PlannedTrade{}

	for _, rows := range m {
		out = append(out, rows...)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].FundingTime.Equal(out[j].FundingTime) {
			if out[i].Exchange == out[j].Exchange {
				return out[i].FundingRate > out[j].FundingRate
			}

			return out[i].Exchange < out[j].Exchange
		}

		return out[i].FundingTime.Before(out[j].FundingTime)
	})

	return out
}

func makePlanID(exchange domain.ExchangeName, symbol string, fundingTime time.Time) string {
	return fmt.Sprintf(
		"%s:%s:%d",
		exchange,
		symbol,
		fundingTime.UTC().Unix(),
	)
}

func tradeKey(exchange domain.ExchangeName, symbol string, fundingTime time.Time) string {
	return makePlanID(exchange, symbol, fundingTime)
}

func symbolForExchange(c domain.Candidate) string {
	if c.NativeSymbol != "" {
		return c.NativeSymbol
	}

	return c.Symbol
}

func symbolForPlan(p domain.PlannedTrade) string {
	if p.NativeSymbol != "" {
		return p.NativeSymbol
	}

	return p.Symbol
}

func symbolForActive(t domain.ActiveTrade) string {
	if t.NativeSymbol != "" {
		return t.NativeSymbol
	}

	return t.Symbol
}

func estimatedQty(c domain.Candidate, usdt float64) float64 {
	ref := c.MarkPrice
	if ref <= 0 {
		ref = c.Price
	}

	return estimatedQtyFromPrice(usdt, ref)
}

func estimatedQtyFromPrice(usdt, price float64) float64 {
	if usdt <= 0 || price <= 0 {
		return 0
	}

	return usdt / price
}

func orderFilled(r domain.OrderResult) bool {
	if r.OrderStatus == domain.OrderStatusFilled {
		return true
	}

	return strings.EqualFold(r.Status, "FILLED") ||
		strings.EqualFold(r.Status, "closed") ||
		strings.EqualFold(r.Status, "filled")
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

func sameSecond(a, b time.Time) bool {
	return a.UTC().Unix() == b.UTC().Unix()
}

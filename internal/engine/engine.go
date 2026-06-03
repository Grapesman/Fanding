package engine

import (
	"context"
	"fmt"
	"log"
	"sort"
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
	warned    map[int64]bool
	planned   map[string]bool
}

func New(cfg config.Config, exchanges []domain.Exchange, store *storage.Store, tg *telegram.Bot) *Engine {
	return &Engine{cfg: cfg, exchanges: exchanges, store: store, tg: tg, warned: map[int64]bool{}, planned: map[string]bool{}}
}

func (e *Engine) Run(ctx context.Context) error {
	e.refreshBalances()
	_ = e.tg.Startup(string(e.cfg.BotMode), e.store.Balances())

	ticker := time.NewTicker(e.cfg.ScanInterval)
	defer ticker.Stop()
	if err := e.scanOnce(ctx); err != nil {
		log.Println("scan:", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := e.scanOnce(ctx); err != nil {
				log.Println("scan:", err)
				e.store.AddLog("scan error: " + err.Error())
			}
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
	selectedByExchange := map[domain.ExchangeName]domain.Candidate{}

	for _, ex := range e.exchanges {
		candidates, err := ex.FundingCandidates()
		if err != nil {
			e.store.SetExchangeStatus(ex.Name(), false, err.Error())
			e.store.AddLog(fmt.Sprintf("%s candidates error: %v", ex.Name(), err))
			continue
		}
		e.store.SetExchangeStatus(ex.Name(), true, "")
		processed := e.processCandidates(candidates)
		all = append(all, processed...)
		best, ok := e.chooseBest(processed)
		if ok {
			selectedByExchange[ex.Name()] = best
		}
	}

	for i := range all {
		if best, ok := selectedByExchange[all[i].Exchange]; ok && best.Symbol == all[i].Symbol {
			all[i].Selected = true
		}
	}
	e.store.SetCandidates(all)

	e.handleWarningsAndPlans(selectedByExchange)
	return nil
}

func (e *Engine) processCandidates(in []domain.Candidate) []domain.Candidate {
	out := make([]domain.Candidate, 0, len(in))
	for _, c := range in {
		c.PassFunding = c.FundingRate >= e.cfg.MinFundingRate
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
		c.SafeTPPrice = ref - ref*c.FundingRate*e.cfg.TakeFundingMove
		c.PlannedEntry = c.Price * (1 + e.cfg.EntryOffset)
		out = append(out, c)
	}
	return out
}

func (e *Engine) chooseBest(candidates []domain.Candidate) (domain.Candidate, bool) {
	filtered := []domain.Candidate{}
	for _, c := range candidates {
		if c.PassFunding && c.PassVolume && c.PassSpread && !e.store.IsSkipped(c.Exchange, c.Symbol, c.NextFundingTime.Unix()) {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) == 0 {
		return domain.Candidate{}, false
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].FundingRate == filtered[j].FundingRate {
			return filtered[i].Volume24hUSDT > filtered[j].Volume24hUSDT
		}
		return filtered[i].FundingRate > filtered[j].FundingRate
	})
	return filtered[0], true
}

func (e *Engine) handleWarningsAndPlans(selected map[domain.ExchangeName]domain.Candidate) {
	now := time.Now()
	nearest := time.Time{}
	for _, c := range selected {
		if nearest.IsZero() || c.NextFundingTime.Before(nearest) {
			nearest = c.NextFundingTime
		}
	}
	if !nearest.IsZero() {
		until := time.Until(nearest)
		bucket := nearest.Unix()
		if until <= e.cfg.PreFundingWarning && until > 0 && !e.warned[bucket] {
			_ = e.tg.PreFundingWarning(until)
			e.warned[bucket] = true
		}
	}

	for _, c := range selected {
		key := string(c.Exchange) + ":" + c.Symbol + ":" + fmt.Sprint(c.NextFundingTime.Unix())
		until := c.NextFundingTime.Sub(now)
		if until <= e.cfg.PreFundingWarning && until > 0 && !e.planned[key] {
			_ = e.tg.Planned(c)
			e.store.AddLog(fmt.Sprintf("planned %s %s funding %.4f%%", c.Exchange, c.Symbol, c.FundingRate*100))
			e.planned[key] = true
		}
		// In paper/live this is where state-machine scheduling will place primary SELL at ENTRY_BEFORE_FUNDING_SEC.
		if until <= e.cfg.EntryBeforeFunding && until > 0 {
			e.store.AddLog(fmt.Sprintf("entry window reached for %s %s mode=%s", c.Exchange, c.Symbol, e.cfg.BotMode))
		}
	}
}

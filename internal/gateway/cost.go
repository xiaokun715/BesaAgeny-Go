package gateway

import (
	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/shopspring/decimal"
)

// 价格单位固定为**每 1K token**。
var priceUnit = decimal.NewFromInt(1000)

type Price struct {
	Input       decimal.Decimal
	Output      decimal.Decimal
	CachedInput decimal.Decimal
	HasCached   bool
}

type CostSheet struct {
	Currency string
	Prices   map[string]Price
}

func (c *CostSheet) PriceOf(modelKey string) (Price, bool) {
	p, ok := c.Prices[modelKey]
	return p, ok
}

// Calc：模型无价格或用量未知 → Cost.Amount=nil（不是 0）。
func (c *CostSheet) Calc(modelKey string, usage domain.Usage) domain.Cost {
	price, ok := c.Prices[modelKey]
	if !ok {
		return domain.Cost{Currency: c.Currency}
	}
	if !usage.Known() {
		return domain.Cost{Currency: c.Currency}
	}
	amount := decimal.Zero
	if usage.InputTokens != nil {
		cached := 0
		if usage.CachedInputTokens != nil {
			cached = *usage.CachedInputTokens
		}
		if cached > *usage.InputTokens {
			cached = *usage.InputTokens
		}
		regular := *usage.InputTokens - cached
		amount = amount.Add(decimal.NewFromInt(int64(regular)).Mul(price.Input).Div(priceUnit))
		if cached > 0 {
			cachedPrice := price.Input
			if price.HasCached {
				cachedPrice = price.CachedInput
			}
			amount = amount.Add(decimal.NewFromInt(int64(cached)).Mul(cachedPrice).Div(priceUnit))
		}
	}
	if usage.OutputTokens != nil {
		amount = amount.Add(decimal.NewFromInt(int64(*usage.OutputTokens)).Mul(price.Output).Div(priceUnit))
	}
	return domain.Cost{Currency: c.Currency, Amount: &amount}
}

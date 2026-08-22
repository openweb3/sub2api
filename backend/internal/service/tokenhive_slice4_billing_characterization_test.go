package service

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const tokenHiveSlice4FormulaVersion = "sub2api@bb7c9722b9ddefde52138aaa52c93034e1bb32b3"

type billingGolden struct {
	Name           string                            `json:"name"`
	FormulaVersion string                            `json:"formula_version"`
	Components     []tokenHiveSlice4BillingComponent `json:"components"`
	Total          string                            `json:"total"`
}

type tokenHiveSlice4BillingGoldenCase struct {
	billingGolden
	SourceFacts    []tokenHiveSlice4SourceFact `json:"source_facts"`
	ResolverInputs any                         `json:"resolver_inputs"`
}

type tokenHiveSlice4SourceFact struct {
	Commit     string `json:"commit"`
	File       string `json:"file"`
	Symbol     string `json:"symbol"`
	SourceTest string `json:"source_test"`
}

type tokenHiveSlice4BillingComponent struct {
	Name      string `json:"name"`
	Selector  string `json:"selector"`
	Quantity  string `json:"quantity"`
	UnitPrice string `json:"unit_price"`
	Subtotal  string `json:"subtotal"`
}

type tokenHiveSlice4ComponentInput struct {
	name     string
	selector string
	quantity float64
	unit     float64
	subtotal float64
}

func tokenHiveSlice4BillingSource(file, symbol, sourceTest string) []tokenHiveSlice4SourceFact {
	return []tokenHiveSlice4SourceFact{{
		Commit:     "bb7c9722b9ddefde52138aaa52c93034e1bb32b3",
		File:       file,
		Symbol:     symbol,
		SourceTest: sourceTest,
	}}
}

func tokenHiveSlice4GoldenCase(name string, source []tokenHiveSlice4SourceFact, inputs any, total float64, components ...tokenHiveSlice4ComponentInput) tokenHiveSlice4BillingGoldenCase {
	goldenComponents := make([]tokenHiveSlice4BillingComponent, 0, len(components))
	for _, component := range components {
		goldenComponents = append(goldenComponents, tokenHiveSlice4BillingComponent{
			Name:      component.name,
			Selector:  component.selector,
			Quantity:  tokenHiveSlice4Decimal(component.quantity),
			UnitPrice: tokenHiveSlice4Decimal(component.unit),
			Subtotal:  tokenHiveSlice4Decimal(component.subtotal),
		})
	}
	return tokenHiveSlice4BillingGoldenCase{
		billingGolden: billingGolden{
			Name:           name,
			FormulaVersion: tokenHiveSlice4FormulaVersion,
			Components:     goldenComponents,
			Total:          tokenHiveSlice4Decimal(total),
		},
		SourceFacts:    source,
		ResolverInputs: inputs,
	}
}

func tokenHiveSlice4Decimal(value float64) string {
	formatted := strconv.FormatFloat(value, 'f', 12, 64)
	formatted = strings.TrimRight(strings.TrimRight(formatted, "0"), ".")
	if formatted == "-0" || formatted == "" {
		return "0"
	}
	return formatted
}

func tokenHiveSlice4Float(value float64) *float64 { return &value }
func tokenHiveSlice4Int(value int) *int           { return &value }

func tokenHiveSlice4CalculateTokenCost(t *testing.T, model string, tokens UsageTokens, resolved *ResolvedPricing) *CostBreakdown {
	t.Helper()
	billing := &BillingService{}
	resolver := &ModelPricingResolver{}
	cost, err := billing.CalculateCostUnified(CostInput{
		Ctx:            context.Background(),
		Model:          model,
		Tokens:         tokens,
		RateMultiplier: 1,
		Resolver:       resolver,
		Resolved:       resolved,
	})
	require.NoError(t, err)
	return cost
}

func tokenHiveSlice4BillingGoldenCases(t *testing.T) []tokenHiveSlice4BillingGoldenCase {
	t.Helper()
	formulaSource := tokenHiveSlice4BillingSource(
		"backend/internal/service/billing_service.go",
		"BillingService.CalculateCostUnified",
		"TestCalculateCostUnified_TokenMode",
	)
	intervalSource := tokenHiveSlice4BillingSource(
		"backend/internal/service/channel.go",
		"FindMatchingInterval",
		"TestGetRequestTierPriceByContext_ExactBoundary",
	)
	imageSource := tokenHiveSlice4BillingSource(
		"backend/internal/service/image_billing_size.go",
		"ResolveImageBillingSize",
		"TestResolveImageBillingSize",
	)

	basePrice := &ModelPricing{InputPricePerToken: 0.001}
	intervalPrice := 0.002
	intervalMax := tokenHiveSlice4Int(200)
	intervalResolved := &ResolvedPricing{
		Mode:        BillingModeToken,
		BasePricing: basePrice,
		Intervals: []PricingInterval{{
			MinTokens:  100,
			MaxTokens:  intervalMax,
			InputPrice: &intervalPrice,
		}},
	}
	lowerTokens := UsageTokens{InputTokens: 100}
	lowerCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-interval", lowerTokens, intervalResolved)
	upperTokens := UsageTokens{InputTokens: 200}
	upperCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-interval", upperTokens, intervalResolved)

	standardTokens := UsageTokens{CacheCreationTokens: 10}
	standardResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{CacheCreationPricePerToken: 0.003}}
	standardCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-cache", standardTokens, standardResolved)

	aggregateTokens := UsageTokens{CacheCreationTokens: 10}
	aggregateResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{
		SupportsCacheBreakdown: true,
		CacheCreation5mPrice:   0.004,
		CacheCreation1hPrice:   0.006,
	}}
	aggregateCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-cache", aggregateTokens, aggregateResolved)

	splitTokens := UsageTokens{CacheCreationTokens: 10, CacheCreation5mTokens: 4, CacheCreation1hTokens: 6}
	splitCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-cache", splitTokens, aggregateResolved)

	explicitZeroCacheTokens := UsageTokens{CacheCreationTokens: 10}
	explicitZeroCacheResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{
		InputPricePerToken:         0.008,
		CacheCreationPriceExplicit: true,
	}}
	explicitZeroCacheCost := tokenHiveSlice4CalculateTokenCost(t, "gpt-5.6-sol", explicitZeroCacheTokens, explicitZeroCacheResolved)
	fallbackCacheResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{InputPricePerToken: 0.008}}
	fallbackCacheCost := tokenHiveSlice4CalculateTokenCost(t, "gpt-5.6-sol", explicitZeroCacheTokens, fallbackCacheResolved)

	imageOutputTokens := UsageTokens{OutputTokens: 5, ImageOutputTokens: 5}
	explicitZeroImageResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{
		OutputPricePerToken:      0.02,
		ImageOutputPriceExplicit: true,
	}}
	explicitZeroImageCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-image-token", imageOutputTokens, explicitZeroImageResolved)
	fallbackImageResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{OutputPricePerToken: 0.02}}
	fallbackImageCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-image-token", imageOutputTokens, fallbackImageResolved)

	prices := &ImagePriceConfig{
		Price1K: tokenHiveSlice4Float(0.1),
		Price2K: tokenHiveSlice4Float(0.2),
		Price4K: tokenHiveSlice4Float(0.4),
	}
	billing := &BillingService{}
	imageCases := []struct {
		name        string
		inputSize   string
		outputSizes []string
		wantTier    string
		unit        float64
	}{
		{"image_1k", "1K", nil, ImageBillingSize1K, 0.1},
		{"image_2k", "2K", nil, ImageBillingSize2K, 0.2},
		{"image_4k", "4K", nil, ImageBillingSize4K, 0.4},
		{"image_auto_defaults_to_2k", "auto", nil, ImageBillingSize2K, 0.2},
	}

	cases := []tokenHiveSlice4BillingGoldenCase{
		tokenHiveSlice4GoldenCase("interval_lower_bound_open", append(formulaSource, intervalSource...), map[string]any{"model": "fixture-interval", "tokens": lowerTokens, "resolved": intervalResolved}, lowerCost.TotalCost,
			tokenHiveSlice4ComponentInput{"input", "base_pricing because total_context == interval.min", 100, 0.001, lowerCost.InputCost}),
		tokenHiveSlice4GoldenCase("interval_upper_bound_closed", append(formulaSource, intervalSource...), map[string]any{"model": "fixture-interval", "tokens": upperTokens, "resolved": intervalResolved}, upperCost.TotalCost,
			tokenHiveSlice4ComponentInput{"input", "first interval because min < total_context <= max", 200, 0.002, upperCost.InputCost}),
		tokenHiveSlice4GoldenCase("cache_aggregate_standard", formulaSource, map[string]any{"model": "fixture-cache", "tokens": standardTokens, "resolved": standardResolved}, standardCost.TotalCost,
			tokenHiveSlice4ComponentInput{"cache_creation", "aggregate cache_creation_tokens at standard price", 10, 0.003, standardCost.CacheCreationCost}),
		tokenHiveSlice4GoldenCase("cache_aggregate_falls_back_to_5m", formulaSource, map[string]any{"model": "fixture-cache", "tokens": aggregateTokens, "resolved": aggregateResolved}, aggregateCost.TotalCost,
			tokenHiveSlice4ComponentInput{"cache_creation_5m", "aggregate tokens at 5m when both detail counters are zero", 10, 0.004, aggregateCost.CacheCreationCost}),
		tokenHiveSlice4GoldenCase("cache_5m_1h_split", formulaSource, map[string]any{"model": "fixture-cache", "tokens": splitTokens, "resolved": aggregateResolved}, splitCost.TotalCost,
			tokenHiveSlice4ComponentInput{"cache_creation_5m", "cache_creation_5m_tokens", 4, 0.004, 4 * 0.004},
			tokenHiveSlice4ComponentInput{"cache_creation_1h", "cache_creation_1h_tokens", 6, 0.006, 6 * 0.006}),
		tokenHiveSlice4GoldenCase("cache_explicit_zero", formulaSource, map[string]any{"model": "gpt-5.6-sol", "tokens": explicitZeroCacheTokens, "resolved": explicitZeroCacheResolved}, explicitZeroCacheCost.TotalCost,
			tokenHiveSlice4ComponentInput{"cache_creation", "explicit zero suppresses model fallback", 10, 0, explicitZeroCacheCost.CacheCreationCost}),
		tokenHiveSlice4GoldenCase("cache_zero_falls_back_for_gpt_5_6", formulaSource, map[string]any{"model": "gpt-5.6-sol", "tokens": explicitZeroCacheTokens, "resolved": fallbackCacheResolved}, fallbackCacheCost.TotalCost,
			tokenHiveSlice4ComponentInput{"cache_creation", "non-explicit zero falls back to input_price * 1.25", 10, 0.01, fallbackCacheCost.CacheCreationCost}),
		tokenHiveSlice4GoldenCase("image_output_explicit_zero", formulaSource, map[string]any{"model": "fixture-image-token", "tokens": imageOutputTokens, "resolved": explicitZeroImageResolved}, explicitZeroImageCost.TotalCost,
			tokenHiveSlice4ComponentInput{"image_output", "explicit zero image output price", 5, 0, explicitZeroImageCost.ImageOutputCost}),
		tokenHiveSlice4GoldenCase("image_output_zero_falls_back", formulaSource, map[string]any{"model": "fixture-image-token", "tokens": imageOutputTokens, "resolved": fallbackImageResolved}, fallbackImageCost.TotalCost,
			tokenHiveSlice4ComponentInput{"image_output", "non-explicit zero falls back to output price", 5, 0.02, fallbackImageCost.ImageOutputCost}),
	}

	for _, imageCase := range imageCases {
		resolved := ResolveImageBillingSize(imageCase.inputSize, imageCase.outputSizes)
		require.Equal(t, imageCase.wantTier, resolved.BillingSize)
		cost := billing.CalculateImageCost("fixture-image", resolved.BillingSize, 2, prices, 1)
		cases = append(cases, tokenHiveSlice4GoldenCase(imageCase.name, imageSource, map[string]any{
			"model": "fixture-image", "input_size": imageCase.inputSize, "output_sizes": imageCase.outputSizes,
			"resolution": resolved, "image_count": 2, "group_prices": prices, "rate_multiplier": 1,
		}, cost.TotalCost, tokenHiveSlice4ComponentInput{"image", "resolved billing size selects group unit price", 2, imageCase.unit, cost.TotalCost}))
	}

	perRequestResolved := &ResolvedPricing{Mode: BillingModePerRequest, DefaultPerRequestPrice: 0.07}
	resolver := &ModelPricingResolver{}
	perRequestCost, err := billing.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "fixture-request", RequestCount: 3, RateMultiplier: 1,
		Resolver: resolver, Resolved: perRequestResolved,
	})
	require.NoError(t, err)
	cases = append(cases, tokenHiveSlice4GoldenCase("per_request_count", formulaSource,
		map[string]any{"model": "fixture-request", "request_count": 3, "resolved": perRequestResolved, "rate_multiplier": 1}, perRequestCost.TotalCost,
		tokenHiveSlice4ComponentInput{"request", "default per-request price", 3, 0.07, perRequestCost.TotalCost}))

	alphaCost := billing.CalculateWebSearchCost(1, nil, 1)
	cases = append(cases, tokenHiveSlice4GoldenCase("alpha_search_fixed_one_call",
		tokenHiveSlice4BillingSource("backend/internal/service/openai_alpha_search.go", "OpenAIGatewayService.ForwardAlphaSearch", "TestForwardAlphaSearchOAuthPreservesWire"),
		map[string]any{"web_search_calls": 1, "group_price": nil, "rate_multiplier": 1}, alphaCost.TotalCost,
		tokenHiveSlice4ComponentInput{"web_search", "ForwardAlphaSearch result fixes WebSearchCalls to one; default price", 1, 0.01, alphaCost.TotalCost}))

	return cases
}

func TestTokenHiveSlice4BillingGolden(t *testing.T) {
	cases := tokenHiveSlice4BillingGoldenCases(t)
	require.Len(t, cases, 15)

	for _, goldenCase := range cases {
		t.Run(goldenCase.Name, func(t *testing.T) {
			require.Equal(t, tokenHiveSlice4FormulaVersion, goldenCase.FormulaVersion)
			require.NotEmpty(t, goldenCase.SourceFacts)
			require.NotNil(t, goldenCase.ResolverInputs)
			var componentTotal float64
			for _, component := range goldenCase.Components {
				quantity, err := strconv.ParseFloat(component.Quantity, 64)
				require.NoError(t, err)
				unit, err := strconv.ParseFloat(component.UnitPrice, 64)
				require.NoError(t, err)
				subtotal, err := strconv.ParseFloat(component.Subtotal, 64)
				require.NoError(t, err)
				require.InDelta(t, quantity*unit, subtotal, 1e-12)
				componentTotal += subtotal
			}
			total, err := strconv.ParseFloat(goldenCase.Total, 64)
			require.NoError(t, err)
			require.InDelta(t, componentTotal, total, 1e-12)
		})
	}

	if output := strings.TrimSpace(os.Getenv("TOKENHIVE_SLICE4_GOLDEN_OUT")); output != "" {
		payload, err := json.MarshalIndent(cases, "", "  ")
		require.NoError(t, err)
		payload = append(payload, '\n')
		require.NoError(t, os.WriteFile(output, payload, 0o644))
	}
}

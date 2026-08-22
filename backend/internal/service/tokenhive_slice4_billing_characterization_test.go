package service

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const (
	tokenHiveSlice4FormulaVersion = "sub2api@bb7c9722b9ddefde52138aaa52c93034e1bb32b3"
	tokenHiveSlice4FormulaCommit  = "bb7c9722b9ddefde52138aaa52c93034e1bb32b3"
	tokenHiveSlice4HandoffCommit  = "a8b6b53eac2159b3c16402ae3dc69c9ccea884a9"
)

type billingGolden struct {
	Name                  string                            `json:"name"`
	FormulaVersion        string                            `json:"formula_version"`
	Components            []tokenHiveSlice4BillingComponent `json:"components"`
	Total                 string                            `json:"total"`
	RuntimeCanonicalTotal string                            `json:"runtime_canonical_total"`
}

type tokenHiveSlice4BillingGoldenCase struct {
	billingGolden
	SourceFacts    []tokenHiveSlice4SourceFact `json:"source_facts"`
	ResolverInputs any                         `json:"resolver_inputs"`
}

type tokenHiveSlice4SourceFact struct {
	Scope        string `json:"scope"`
	Commit       string `json:"commit"`
	File         string `json:"file"`
	Symbol       string `json:"symbol"`
	SourceTest   string `json:"source_test,omitempty"`
	EvidenceNote string `json:"evidence_note,omitempty"`
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
	quantity string
	unit     string
	subtotal string
}

func tokenHiveSlice4Source(scope, commit, file, symbol, sourceTest string) tokenHiveSlice4SourceFact {
	return tokenHiveSlice4SourceFact{
		Scope: scope, Commit: commit, File: file, Symbol: symbol, SourceTest: sourceTest,
	}
}

func tokenHiveSlice4TokenFormulaSource(sourceTest string) tokenHiveSlice4SourceFact {
	return tokenHiveSlice4Source(
		"formula", tokenHiveSlice4FormulaCommit,
		"backend/internal/service/billing_service.go", "BillingService.calculateTokenCost", sourceTest,
	)
}

func tokenHiveSlice4GoldenCase(name string, source []tokenHiveSlice4SourceFact, inputs any, total string, components ...tokenHiveSlice4ComponentInput) tokenHiveSlice4BillingGoldenCase {
	return tokenHiveSlice4GoldenCaseWithRuntimeTotal(name, source, inputs, total, total, components...)
}

func tokenHiveSlice4GoldenCaseWithRuntimeTotal(name string, source []tokenHiveSlice4SourceFact, inputs any, total, runtimeCanonicalTotal string, components ...tokenHiveSlice4ComponentInput) tokenHiveSlice4BillingGoldenCase {
	goldenComponents := make([]tokenHiveSlice4BillingComponent, 0, len(components))
	for _, component := range components {
		goldenComponents = append(goldenComponents, tokenHiveSlice4BillingComponent{
			Name: component.name, Selector: component.selector, Quantity: component.quantity,
			UnitPrice: component.unit, Subtotal: component.subtotal,
		})
	}
	return tokenHiveSlice4BillingGoldenCase{
		billingGolden: billingGolden{
			Name: name, FormulaVersion: tokenHiveSlice4FormulaVersion,
			Components: goldenComponents, Total: total, RuntimeCanonicalTotal: runtimeCanonicalTotal,
		},
		SourceFacts: source, ResolverInputs: inputs,
	}
}

func tokenHiveSlice4DecimalFromString(t *testing.T, value string) decimal.Decimal {
	t.Helper()
	parsed, err := decimal.NewFromString(value)
	require.NoError(t, err)
	require.Equal(t, parsed.String(), value, "golden decimals must use canonical string form")
	return parsed
}

// Runtime prices cross the source API boundary as binary floats. Golden expectations
// stay decimal strings; this is the only conversion into the runtime representation.
func tokenHiveSlice4RuntimePrice(t *testing.T, sourceDecimal string) float64 {
	t.Helper()
	value, _ := tokenHiveSlice4DecimalFromString(t, sourceDecimal).Float64()
	return value
}

func tokenHiveSlice4RuntimePricePointer(t *testing.T, sourceDecimal string) *float64 {
	t.Helper()
	value := tokenHiveSlice4RuntimePrice(t, sourceDecimal)
	return &value
}

// CostBreakdown exposes binary floats. At this source boundary, use Go's shortest
// non-exponent decimal representation and parse it back through decimal. This does
// not simulate or claim any database storage quantization.
func tokenHiveSlice4RequireRuntimeAmount(t *testing.T, expectedCanonical string, runtimeAmount float64) {
	t.Helper()
	runtimeString := strconv.FormatFloat(runtimeAmount, 'f', -1, 64)
	got := tokenHiveSlice4DecimalFromString(t, runtimeString).String()
	require.Equal(t, expectedCanonical, got, "runtime canonical amount")
}

func tokenHiveSlice4Int(value int) *int { return &value }

func tokenHiveSlice4CalculateTokenCost(t *testing.T, model string, tokens UsageTokens, resolved *ResolvedPricing) *CostBreakdown {
	t.Helper()
	cost, err := (&BillingService{}).CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: model, Tokens: tokens, RateMultiplier: 1,
		Resolver: &ModelPricingResolver{}, Resolved: resolved,
	})
	require.NoError(t, err)
	return cost
}

func tokenHiveSlice4TokenResolverInputs(model string, tokens map[string]int, base map[string]any, intervals []map[string]any) map[string]any {
	fullTokens := map[string]int{
		"input_tokens": 0, "image_input_tokens": 0, "output_tokens": 0,
		"cache_creation_tokens": 0, "cache_read_tokens": 0,
		"cache_creation_5m_tokens": 0, "cache_creation_1h_tokens": 0, "image_output_tokens": 0,
	}
	for name, value := range tokens {
		fullTokens[name] = value
	}
	fullBase := map[string]any{
		"input_price_per_token": "0", "input_price_per_token_priority": "0", "image_input_price_per_token": "0",
		"output_price_per_token": "0", "output_price_per_token_priority": "0",
		"cache_creation_price_per_token": "0", "cache_creation_price_per_token_priority": "0", "cache_creation_price_explicit": false,
		"cache_read_price_per_token": "0", "cache_read_price_per_token_priority": "0",
		"cache_creation_5m_price": "0", "cache_creation_1h_price": "0", "supports_cache_breakdown": false,
		"long_context_input_threshold": 0, "long_context_input_multiplier": "0", "long_context_output_multiplier": "0",
		"image_output_price_per_token": "0", "image_output_price_explicit": false,
	}
	for name, value := range base {
		fullBase[name] = value
	}
	var fullIntervals []map[string]any
	if intervals != nil {
		fullIntervals = make([]map[string]any, 0, len(intervals))
		for _, interval := range intervals {
			fullInterval := map[string]any{
				"id": 0, "pricing_id": 0, "min_tokens": 0, "max_tokens": nil, "tier_label": "",
				"input_price": nil, "output_price": nil, "cache_write_price": nil, "cache_read_price": nil,
				"per_request_price": nil, "sort_order": 0,
			}
			for name, value := range interval {
				fullInterval[name] = value
			}
			fullIntervals = append(fullIntervals, fullInterval)
		}
	}
	return map[string]any{
		"model":  model,
		"tokens": fullTokens,
		"resolved": map[string]any{
			"mode": "token", "base_pricing": fullBase, "intervals": fullIntervals,
			"request_tiers": nil, "default_per_request_price": "0", "source": "", "supports_cache_breakdown": false,
		},
		"rate_multiplier": "1",
	}
}

func tokenHiveSlice4BillingGoldenCases(t *testing.T) []tokenHiveSlice4BillingGoldenCase {
	t.Helper()
	intervalSelector := []tokenHiveSlice4SourceFact{
		tokenHiveSlice4Source("selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/model_pricing_resolver.go", "ModelPricingResolver.GetIntervalPricing", "TestGetIntervalPricing_MatchesInterval"),
		tokenHiveSlice4Source("boundary", tokenHiveSlice4FormulaCommit, "backend/internal/service/channel.go", "FindMatchingInterval", "TestGetRequestTierPriceByContext_ExactBoundary"),
	}
	standardCacheSelector := tokenHiveSlice4Source("selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.computeCacheCreationCost", "TestCalculateCost_WithCacheTokens")
	breakdownCacheSelector := tokenHiveSlice4Source("selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.computeCacheCreationCost", "TestCalculateCost_SupportsCacheBreakdown")
	zeroDetailFallbackSelector := tokenHiveSlice4SourceFact{
		Scope: "selector_without_bound_source_test", Commit: tokenHiveSlice4FormulaCommit,
		File: "backend/internal/service/billing_service.go", Symbol: "BillingService.computeCacheCreationCost",
		EvidenceNote: "No independent source test at the bound commit; see characterization_fixture fact.",
	}
	zeroDetailFallbackFixture := tokenHiveSlice4Source("characterization_fixture", "aaf39ab61fec7d2810b33541300419ab5b36ad8c", "backend/internal/service/tokenhive_slice4_billing_characterization_test.go", "TestTokenHiveSlice4BillingGolden/cache_aggregate_falls_back_to_5m", "TestTokenHiveSlice4BillingGolden/cache_aggregate_falls_back_to_5m")
	imageOutputSelector := tokenHiveSlice4Source("selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.computeTokenBreakdown", "TestComputeTokenBreakdown_ExplicitZeroImagePrice_NoFallback")

	basePrice := map[string]any{"input_price_per_token": "0.001"}
	intervalInputs := []map[string]any{{"min_tokens": 100, "max_tokens": 200, "input_price": "0.002"}}
	intervalRuntime := tokenHiveSlice4RuntimePrice(t, "0.002")
	intervalResolved := &ResolvedPricing{
		Mode:        BillingModeToken,
		BasePricing: &ModelPricing{InputPricePerToken: tokenHiveSlice4RuntimePrice(t, "0.001")},
		Intervals:   []PricingInterval{{MinTokens: 100, MaxTokens: tokenHiveSlice4Int(200), InputPrice: &intervalRuntime}},
	}
	lowerCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-interval", UsageTokens{InputTokens: 100}, intervalResolved)
	upperCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-interval", UsageTokens{InputTokens: 200}, intervalResolved)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.1", lowerCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.1", lowerCost.InputCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.4", upperCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.4", upperCost.InputCost)

	standardTokens := UsageTokens{CacheCreationTokens: 10}
	standardResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{CacheCreationPricePerToken: tokenHiveSlice4RuntimePrice(t, "0.003")}}
	standardCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-cache", standardTokens, standardResolved)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.03", standardCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.03", standardCost.CacheCreationCost)

	aggregateTokens := UsageTokens{CacheCreationTokens: 10}
	aggregateResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{
		SupportsCacheBreakdown: true,
		CacheCreation5mPrice:   tokenHiveSlice4RuntimePrice(t, "0.004"),
		CacheCreation1hPrice:   tokenHiveSlice4RuntimePrice(t, "0.006"),
	}}
	aggregateCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-cache", aggregateTokens, aggregateResolved)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.04", aggregateCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.04", aggregateCost.CacheCreationCost)

	splitTokens := UsageTokens{CacheCreationTokens: 10, CacheCreation5mTokens: 4, CacheCreation1hTokens: 6}
	splitCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-cache", splitTokens, aggregateResolved)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.052000000000000005", splitCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.052000000000000005", splitCost.CacheCreationCost)

	explicitZeroTokens := UsageTokens{CacheCreationTokens: 10}
	explicitZeroResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{
		InputPricePerToken: tokenHiveSlice4RuntimePrice(t, "0.008"), CacheCreationPriceExplicit: true,
	}}
	explicitZeroCost := tokenHiveSlice4CalculateTokenCost(t, "gpt-5.6-sol", explicitZeroTokens, explicitZeroResolved)
	tokenHiveSlice4RequireRuntimeAmount(t, "0", explicitZeroCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0", explicitZeroCost.CacheCreationCost)
	fallbackCacheResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{InputPricePerToken: tokenHiveSlice4RuntimePrice(t, "0.008")}}
	fallbackCacheCost := tokenHiveSlice4CalculateTokenCost(t, "gpt-5.6-sol", explicitZeroTokens, fallbackCacheResolved)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.1", fallbackCacheCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.1", fallbackCacheCost.CacheCreationCost)

	imageTokens := UsageTokens{OutputTokens: 5, ImageOutputTokens: 5}
	explicitZeroImageResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{
		OutputPricePerToken: tokenHiveSlice4RuntimePrice(t, "0.02"), ImageOutputPriceExplicit: true,
	}}
	explicitZeroImageCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-image-token", imageTokens, explicitZeroImageResolved)
	tokenHiveSlice4RequireRuntimeAmount(t, "0", explicitZeroImageCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0", explicitZeroImageCost.ImageOutputCost)
	fallbackImageResolved := &ResolvedPricing{Mode: BillingModeToken, BasePricing: &ModelPricing{OutputPricePerToken: tokenHiveSlice4RuntimePrice(t, "0.02")}}
	fallbackImageCost := tokenHiveSlice4CalculateTokenCost(t, "fixture-image-token", imageTokens, fallbackImageResolved)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.1", fallbackImageCost.TotalCost)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.1", fallbackImageCost.ImageOutputCost)

	baseFormula := tokenHiveSlice4TokenFormulaSource("TestCalculateCostUnified_TokenMode")
	explicitZeroModelPolicy := tokenHiveSlice4Source("selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.applyModelSpecificPricingPolicy", "TestGPT56ExplicitZeroCacheWritePriceIsPreserved")
	fallbackModelPolicy := tokenHiveSlice4Source("selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.applyModelSpecificPricingPolicy", "TestBillingService_GPT56CacheWritePricingUsesOfficialMultiplier")
	cases := []tokenHiveSlice4BillingGoldenCase{
		tokenHiveSlice4GoldenCase("interval_lower_bound_open", append([]tokenHiveSlice4SourceFact{baseFormula}, intervalSelector...), tokenHiveSlice4TokenResolverInputs("fixture-interval", map[string]int{"input_tokens": 100}, basePrice, intervalInputs), "0.1",
			tokenHiveSlice4ComponentInput{"input", "base_pricing because total_context == interval.min", "100", "0.001", "0.1"}),
		tokenHiveSlice4GoldenCase("interval_upper_bound_closed", append([]tokenHiveSlice4SourceFact{baseFormula}, intervalSelector...), tokenHiveSlice4TokenResolverInputs("fixture-interval", map[string]int{"input_tokens": 200}, basePrice, intervalInputs), "0.4",
			tokenHiveSlice4ComponentInput{"input", "first interval because min < total_context <= max", "200", "0.002", "0.4"}),
		tokenHiveSlice4GoldenCase("cache_aggregate_standard", []tokenHiveSlice4SourceFact{baseFormula, standardCacheSelector}, tokenHiveSlice4TokenResolverInputs("fixture-cache", map[string]int{"cache_creation_tokens": 10}, map[string]any{"cache_creation_price_per_token": "0.003", "supports_cache_breakdown": false}, nil), "0.03",
			tokenHiveSlice4ComponentInput{"cache_creation", "aggregate cache_creation_tokens at standard price", "10", "0.003", "0.03"}),
		tokenHiveSlice4GoldenCase("cache_aggregate_falls_back_to_5m", []tokenHiveSlice4SourceFact{baseFormula, zeroDetailFallbackSelector, zeroDetailFallbackFixture}, tokenHiveSlice4TokenResolverInputs("fixture-cache", map[string]int{"cache_creation_tokens": 10, "cache_creation_5m_tokens": 0, "cache_creation_1h_tokens": 0}, map[string]any{"supports_cache_breakdown": true, "cache_creation_5m_price": "0.004", "cache_creation_1h_price": "0.006"}, nil), "0.04",
			tokenHiveSlice4ComponentInput{"cache_creation_5m", "aggregate tokens at 5m when both detail counters are zero", "10", "0.004", "0.04"}),
		tokenHiveSlice4GoldenCaseWithRuntimeTotal("cache_5m_1h_split", []tokenHiveSlice4SourceFact{baseFormula, breakdownCacheSelector}, tokenHiveSlice4TokenResolverInputs("fixture-cache", map[string]int{"cache_creation_tokens": 10, "cache_creation_5m_tokens": 4, "cache_creation_1h_tokens": 6}, map[string]any{"supports_cache_breakdown": true, "cache_creation_5m_price": "0.004", "cache_creation_1h_price": "0.006"}, nil), "0.052", "0.052000000000000005",
			tokenHiveSlice4ComponentInput{"cache_creation_5m", "cache_creation_5m_tokens", "4", "0.004", "0.016"},
			tokenHiveSlice4ComponentInput{"cache_creation_1h", "cache_creation_1h_tokens", "6", "0.006", "0.036"}),
		tokenHiveSlice4GoldenCase("cache_explicit_zero", []tokenHiveSlice4SourceFact{baseFormula, standardCacheSelector, explicitZeroModelPolicy}, tokenHiveSlice4TokenResolverInputs("gpt-5.6-sol", map[string]int{"cache_creation_tokens": 10}, map[string]any{"input_price_per_token": "0.008", "cache_creation_price_per_token": "0", "cache_creation_price_explicit": true}, nil), "0",
			tokenHiveSlice4ComponentInput{"cache_creation", "explicit zero suppresses model fallback", "10", "0", "0"}),
		tokenHiveSlice4GoldenCase("cache_zero_falls_back_for_gpt_5_6", []tokenHiveSlice4SourceFact{baseFormula, standardCacheSelector, fallbackModelPolicy}, tokenHiveSlice4TokenResolverInputs("gpt-5.6-sol", map[string]int{"cache_creation_tokens": 10}, map[string]any{"input_price_per_token": "0.008", "cache_creation_price_per_token": "0", "cache_creation_price_explicit": false}, nil), "0.1",
			tokenHiveSlice4ComponentInput{"cache_creation", "non-explicit zero falls back to input_price * 1.25", "10", "0.01", "0.1"}),
		tokenHiveSlice4GoldenCase("image_output_explicit_zero", []tokenHiveSlice4SourceFact{baseFormula, imageOutputSelector}, tokenHiveSlice4TokenResolverInputs("fixture-image-token", map[string]int{"output_tokens": 5, "image_output_tokens": 5}, map[string]any{"output_price_per_token": "0.02", "image_output_price_per_token": "0", "image_output_price_explicit": true}, nil), "0",
			tokenHiveSlice4ComponentInput{"image_output", "explicit zero image output price", "5", "0", "0"}),
		tokenHiveSlice4GoldenCase("image_output_zero_falls_back", []tokenHiveSlice4SourceFact{baseFormula, tokenHiveSlice4Source("selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.computeTokenBreakdown", "TestComputeTokenBreakdown_NonExplicitZeroImagePrice_FallsBackToOutput")}, tokenHiveSlice4TokenResolverInputs("fixture-image-token", map[string]int{"output_tokens": 5, "image_output_tokens": 5}, map[string]any{"output_price_per_token": "0.02", "image_output_price_per_token": "0", "image_output_price_explicit": false}, nil), "0.1",
			tokenHiveSlice4ComponentInput{"image_output", "non-explicit zero falls back to output price", "5", "0.02", "0.1"}),
	}

	prices := &ImagePriceConfig{
		Price1K: tokenHiveSlice4RuntimePricePointer(t, "0.1"), Price2K: tokenHiveSlice4RuntimePricePointer(t, "0.2"), Price4K: tokenHiveSlice4RuntimePricePointer(t, "0.4"),
	}
	imageSources := []tokenHiveSlice4SourceFact{
		tokenHiveSlice4Source("selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/image_billing_size.go", "ResolveImageBillingSize", "TestResolveImageBillingSize"),
		tokenHiveSlice4Source("formula", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.CalculateImageCost", "TestCalculateImageCost"),
		tokenHiveSlice4Source("unit_price_selector", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.getImageUnitPrice", "TestCalculateImageCost"),
	}
	imageCases := []struct{ name, inputSize, wantTier, unit, total string }{
		{"image_1k", "1K", ImageBillingSize1K, "0.1", "0.2"},
		{"image_2k", "2K", ImageBillingSize2K, "0.2", "0.4"},
		{"image_4k", "4K", ImageBillingSize4K, "0.4", "0.8"},
		{"image_auto_defaults_to_2k", "auto", ImageBillingSize2K, "0.2", "0.4"},
	}
	billing := &BillingService{}
	for _, imageCase := range imageCases {
		outputSizes := []string{}
		resolution := ResolveImageBillingSize(imageCase.inputSize, outputSizes)
		require.Equal(t, imageCase.wantTier, resolution.BillingSize)
		cost := billing.CalculateImageCost("fixture-image", resolution.BillingSize, 2, prices, 1)
		tokenHiveSlice4RequireRuntimeAmount(t, imageCase.total, cost.TotalCost)
		cases = append(cases, tokenHiveSlice4GoldenCase(imageCase.name, imageSources, map[string]any{
			"model": "fixture-image", "input_size": imageCase.inputSize, "output_sizes": outputSizes,
			"resolution": map[string]any{
				"billing_size": resolution.BillingSize, "input_size": resolution.InputSize,
				"output_size": resolution.OutputSize, "source": resolution.Source, "breakdown": resolution.Breakdown,
			},
			"image_count": 2, "group_prices": map[string]string{"1K": "0.1", "2K": "0.2", "4K": "0.4"}, "rate_multiplier": "1",
		}, imageCase.total, tokenHiveSlice4ComponentInput{"image", "resolved billing size selects group unit price", "2", imageCase.unit, imageCase.total}))
	}

	perRequestResolved := &ResolvedPricing{Mode: BillingModePerRequest, DefaultPerRequestPrice: tokenHiveSlice4RuntimePrice(t, "0.07")}
	perRequestCost, err := billing.CalculateCostUnified(CostInput{
		Ctx: context.Background(), Model: "fixture-request", RequestCount: 3, RateMultiplier: 1,
		Resolver: &ModelPricingResolver{}, Resolved: perRequestResolved,
	})
	require.NoError(t, err)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.21000000000000002", perRequestCost.TotalCost)
	cases = append(cases, tokenHiveSlice4GoldenCaseWithRuntimeTotal("per_request_count", []tokenHiveSlice4SourceFact{
		tokenHiveSlice4Source("formula", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.calculatePerRequestCost", "TestCalculateCostUnified_PerRequestMode"),
	}, map[string]any{
		"model": "fixture-request", "request_count": 3, "size_tier": "",
		"tokens": map[string]int{
			"input_tokens": 0, "image_input_tokens": 0, "output_tokens": 0,
			"cache_creation_tokens": 0, "cache_read_tokens": 0,
			"cache_creation_5m_tokens": 0, "cache_creation_1h_tokens": 0, "image_output_tokens": 0,
		},
		"resolved": map[string]any{
			"mode": "per_request", "base_pricing": nil, "intervals": nil, "request_tiers": nil,
			"default_per_request_price": "0.07", "source": "", "supports_cache_breakdown": false,
		},
		"rate_multiplier": "1",
	}, "0.21", "0.21000000000000002",
		tokenHiveSlice4ComponentInput{"request", "default per-request price", "3", "0.07", "0.21"}))

	alphaCost := billing.CalculateWebSearchCost(1, nil, 1)
	tokenHiveSlice4RequireRuntimeAmount(t, "0.01", alphaCost.TotalCost)
	cases = append(cases, tokenHiveSlice4GoldenCase("alpha_search_fixed_one_call", []tokenHiveSlice4SourceFact{
		tokenHiveSlice4Source("formula", tokenHiveSlice4FormulaCommit, "backend/internal/service/billing_service.go", "BillingService.CalculateWebSearchCost", "TestCalculateWebSearchCostDefaultAndOverride"),
		tokenHiveSlice4Source("runtime_input", tokenHiveSlice4FormulaCommit, "backend/internal/service/openai_alpha_search.go", "OpenAIGatewayService.ForwardAlphaSearch", "TestForwardAlphaSearchOAuthPreservesWire"),
		tokenHiveSlice4Source("approved_tokenhive_handoff_difference", tokenHiveSlice4HandoffCommit, "backend/internal/service/openai_alpha_search.go", "OpenAIGatewayService.ForwardAlphaSearch", "TestTokenHiveMappedPATShapedAlphaSearchUsesCanonicalOperation"),
	}, map[string]any{"web_search_calls": 1, "group_price": nil, "rate_multiplier": "1"}, "0.01",
		tokenHiveSlice4ComponentInput{"web_search", "ForwardAlphaSearch fixes WebSearchCalls to one; nil group price selects default", "1", "0.01", "0.01"}))

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
			componentTotal := decimal.Zero
			for _, component := range goldenCase.Components {
				quantity := tokenHiveSlice4DecimalFromString(t, component.Quantity)
				unit := tokenHiveSlice4DecimalFromString(t, component.UnitPrice)
				subtotal := tokenHiveSlice4DecimalFromString(t, component.Subtotal)
				require.True(t, quantity.Mul(unit).Equal(subtotal), "%s component formula", component.Name)
				componentTotal = componentTotal.Add(subtotal)
			}
			total := tokenHiveSlice4DecimalFromString(t, goldenCase.Total)
			require.True(t, componentTotal.Equal(total), "component total: got %s want %s", componentTotal.String(), total.String())
			tokenHiveSlice4DecimalFromString(t, goldenCase.RuntimeCanonicalTotal)
		})
	}

	if output := strings.TrimSpace(os.Getenv("TOKENHIVE_SLICE4_GOLDEN_OUT")); output != "" {
		payload, err := json.MarshalIndent(cases, "", "  ")
		require.NoError(t, err)
		payload = append(payload, '\n')
		require.NoError(t, os.WriteFile(output, payload, 0o644))
	}
}

func TestTokenHiveSlice4CacheProvenanceMatchesEachBoundBranch(t *testing.T) {
	cases := tokenHiveSlice4BillingGoldenCases(t)
	byName := make(map[string]tokenHiveSlice4BillingGoldenCase, len(cases))
	for _, goldenCase := range cases {
		byName[goldenCase.Name] = goldenCase
	}

	require.Contains(t, byName["cache_aggregate_standard"].SourceFacts, tokenHiveSlice4SourceFact{
		Scope: "selector", Commit: tokenHiveSlice4FormulaCommit,
		File: "backend/internal/service/billing_service.go", Symbol: "BillingService.computeCacheCreationCost",
		SourceTest: "TestCalculateCost_WithCacheTokens",
	})
	require.Contains(t, byName["cache_5m_1h_split"].SourceFacts, tokenHiveSlice4SourceFact{
		Scope: "selector", Commit: tokenHiveSlice4FormulaCommit,
		File: "backend/internal/service/billing_service.go", Symbol: "BillingService.computeCacheCreationCost",
		SourceTest: "TestCalculateCost_SupportsCacheBreakdown",
	})
	require.Contains(t, byName["cache_explicit_zero"].SourceFacts, tokenHiveSlice4SourceFact{
		Scope: "selector", Commit: tokenHiveSlice4FormulaCommit,
		File: "backend/internal/service/billing_service.go", Symbol: "BillingService.applyModelSpecificPricingPolicy",
		SourceTest: "TestGPT56ExplicitZeroCacheWritePriceIsPreserved",
	})
	require.Contains(t, byName["cache_zero_falls_back_for_gpt_5_6"].SourceFacts, tokenHiveSlice4SourceFact{
		Scope: "selector", Commit: tokenHiveSlice4FormulaCommit,
		File: "backend/internal/service/billing_service.go", Symbol: "BillingService.applyModelSpecificPricingPolicy",
		SourceTest: "TestBillingService_GPT56CacheWritePricingUsesOfficialMultiplier",
	})

	fallbackFacts := byName["cache_aggregate_falls_back_to_5m"].SourceFacts
	require.Contains(t, fallbackFacts, tokenHiveSlice4SourceFact{
		Scope: "selector_without_bound_source_test", Commit: tokenHiveSlice4FormulaCommit,
		File: "backend/internal/service/billing_service.go", Symbol: "BillingService.computeCacheCreationCost",
		EvidenceNote: "No independent source test at the bound commit; see characterization_fixture fact.",
	})
	require.Contains(t, fallbackFacts, tokenHiveSlice4SourceFact{
		Scope: "characterization_fixture", Commit: "aaf39ab61fec7d2810b33541300419ab5b36ad8c",
		File:       "backend/internal/service/tokenhive_slice4_billing_characterization_test.go",
		Symbol:     "TestTokenHiveSlice4BillingGolden/cache_aggregate_falls_back_to_5m",
		SourceTest: "TestTokenHiveSlice4BillingGolden/cache_aggregate_falls_back_to_5m",
	})
}

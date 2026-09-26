package session

import "strings"

// ModelMeta holds the context-window and pricing facts escape knows about a
// model id. Prices are USD per 1,000,000 tokens, the same convention pi's
// provider pricing uses. This table is shared: the compaction trigger (WS5)
// reads ContextWindow, and session stats (stats.go) reads the prices for cost
// estimates.
type ModelMeta struct {
	ContextWindow         int
	InputPricePerMillion  float64
	OutputPricePerMillion float64
}

// DefaultModelMeta applies to model ids absent from the table: a generic
// 200k window and no per-token price (cost estimates stay 0).
var DefaultModelMeta = ModelMeta{ContextWindow: 200000}

type modelMetaEntry struct {
	model  string
	prefix bool // match model itself or any "model-" family member
	meta   ModelMeta
}

// modelMetaTable lists the known models. Exact ids win; "glm-5" matches any
// glm-5.* model; "gpt-5.6" matches gpt-5.6 variants other than the three
// priced ones (window only, prices 0).
var modelMetaTable = []modelMetaEntry{
	{model: "deepseek-v4-flash", meta: ModelMeta{ContextWindow: 1000000, InputPricePerMillion: 0.07, OutputPricePerMillion: 0.14}},
	{model: "deepseek-v4-pro", meta: ModelMeta{ContextWindow: 1000000, InputPricePerMillion: 0.435, OutputPricePerMillion: 0.87}},
	{model: "minimax-m3", meta: ModelMeta{ContextWindow: 1000000, InputPricePerMillion: 0.3, OutputPricePerMillion: 1.2}},
	{model: "glm-5", prefix: true, meta: ModelMeta{ContextWindow: 1000000, InputPricePerMillion: 1.4, OutputPricePerMillion: 4.4}},
	{model: "gpt-5.6-luna", meta: ModelMeta{ContextWindow: 272000, InputPricePerMillion: 0.2, OutputPricePerMillion: 1.2}},
	{model: "gpt-5.6-sol", meta: ModelMeta{ContextWindow: 272000, InputPricePerMillion: 5, OutputPricePerMillion: 30}},
	{model: "gpt-5.6-terra", meta: ModelMeta{ContextWindow: 272000, InputPricePerMillion: 2, OutputPricePerMillion: 12}},
	{model: "gpt-5.6", prefix: true, meta: ModelMeta{ContextWindow: 272000}},
}

// LookupModel returns metadata for a model id. Exact ids win; otherwise the
// longest family prefix matches (e.g. "glm-5-x1" -> glm-5); unknown ids fall
// back to DefaultModelMeta.
func LookupModel(model string) ModelMeta {
	m := strings.ToLower(strings.TrimSpace(model))
	best := DefaultModelMeta
	bestLen := -1
	for _, e := range modelMetaTable {
		if e.prefix {
			if m != e.model && !strings.HasPrefix(m, e.model+"-") {
				continue
			}
		} else if m != e.model {
			continue
		}
		if len(e.model) > bestLen {
			best, bestLen = e.meta, len(e.model)
		}
	}
	return best
}

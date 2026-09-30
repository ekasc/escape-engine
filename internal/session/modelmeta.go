package session

import "strings"

// Modality is an input modality a model accepts. Image support is a property of
// the model, not of the provider, so it is declared per model id and read at
// runtime rather than inferred from the endpoint.
type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
)

// ModelMeta holds the context-window, input-modality and pricing facts escape
// knows about a model id. Prices are USD per 1,000,000 tokens, the same
// convention pi's provider pricing uses. This table is shared: the compaction
// trigger (WS5) reads ContextWindow, history building (agent/history.go) reads
// AcceptsImage, and session stats (stats.go) reads the prices for cost
// estimates.
type ModelMeta struct {
	ContextWindow         int
	Input                 []Modality
	InputPricePerMillion  float64
	OutputPricePerMillion float64
}

// AcceptsImage reports whether the model takes image input. A model absent from
// the table is text-only, so a screenshot is never attached on the assumption
// that the model can see it.
func (m ModelMeta) AcceptsImage() bool {
	for _, in := range m.Input {
		if in == ModalityImage {
			return true
		}
	}
	return false
}

// textOnly is the input declaration for a model that reads text and nothing
// else. Named so the table states the fact instead of leaving a nil slice to
// mean it.
var textOnly = []Modality{ModalityText}

// DefaultModelMeta applies to model ids absent from the table: a generic 200k
// window, text-only input, and no per-token price (cost estimates stay 0).
var DefaultModelMeta = ModelMeta{ContextWindow: 200000, Input: textOnly}

type modelMetaEntry struct {
	model  string
	prefix bool // match model itself or any "model-" family member
	meta   ModelMeta
}

// modelMetaTable lists the known models. Exact ids win; "glm-5" matches any
// glm-5.* model; "gpt-5.6" matches gpt-5.6 variants other than the three
// priced ones (window only, prices 0). Input modality is transcribed from each
// model's own catalog declaration; where a family has no declaration, the
// family inherits the text-only default rather than an assumption of vision.
var modelMetaTable = []modelMetaEntry{
	{model: "deepseek-v4-flash", meta: ModelMeta{ContextWindow: 1000000, Input: textOnly, InputPricePerMillion: 0.07, OutputPricePerMillion: 0.14}},
	{model: "deepseek-v4-pro", meta: ModelMeta{ContextWindow: 1000000, Input: textOnly, InputPricePerMillion: 0.435, OutputPricePerMillion: 0.87}},
	{model: "minimax-m3", meta: ModelMeta{ContextWindow: 1000000, Input: []Modality{ModalityText, ModalityImage}, InputPricePerMillion: 0.3, OutputPricePerMillion: 1.2}},
	{model: "glm-5", prefix: true, meta: ModelMeta{ContextWindow: 1000000, Input: textOnly, InputPricePerMillion: 1.4, OutputPricePerMillion: 4.4}},
	{model: "gpt-5.6-luna", meta: ModelMeta{ContextWindow: 272000, Input: []Modality{ModalityText, ModalityImage}, InputPricePerMillion: 0.2, OutputPricePerMillion: 1.2}},
	{model: "gpt-5.6-sol", meta: ModelMeta{ContextWindow: 272000, Input: []Modality{ModalityText, ModalityImage}, InputPricePerMillion: 5, OutputPricePerMillion: 30}},
	{model: "gpt-5.6-terra", meta: ModelMeta{ContextWindow: 272000, Input: []Modality{ModalityText, ModalityImage}, InputPricePerMillion: 2, OutputPricePerMillion: 12}},
	{model: "gpt-5.6", prefix: true, meta: ModelMeta{ContextWindow: 272000, Input: textOnly}},
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

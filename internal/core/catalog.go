package core

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

func timeNowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// devinModelsFilePayload mirrors the embedded/curated JSON envelope.
type devinModelsFilePayload struct {
	Devin  []*modelInfo `json:"devin,omitempty"`
	Models []*modelInfo `json:"models,omitempty"`
}

// catalogStore holds the merged catalog: baseline (embedded) ∪ curated
// (remote JSONs) ∪ live (upstream GetCliModelConfigs). Goroutine-safe.
type catalogStore struct {
	mu        sync.RWMutex
	baseline  []*modelInfo
	curated   []*modelInfo
	live      []*modelInfo
	liveRaw   []rawDevinModel
	revision  uint64
	lastLive  string
	lastError string
	lastPoll  string
	authsUsed []string
}

func newCatalogStore(baseline []*modelInfo) *catalogStore {
	return &catalogStore{baseline: baseline}
}

// parseDevinModelsPayload decodes embedded/curated catalog JSON into wire models.
func parseDevinModelsPayload(data []byte) ([]*modelInfo, error) {
	var payload devinModelsFilePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode devin models payload: %w", err)
	}
	list := payload.Devin
	if len(list) == 0 {
		list = payload.Models
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("empty devin models payload")
	}
	out := make([]*modelInfo, 0, len(list))
	for _, m := range list {
		if m == nil || strings.TrimSpace(m.ID) == "" {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func cloneModel(m *modelInfo) *modelInfo {
	if m == nil {
		return nil
	}
	c := *m
	if m.Thinking != nil {
		t := *m.Thinking
		t.Levels = append([]string(nil), m.Thinking.Levels...)
		c.Thinking = &t
	}
	return &c
}

func normalizeCatalogModel(m *modelInfo) {
	if strings.TrimSpace(m.ID) == "" {
		return
	}
	id := strings.ToLower(strings.TrimSpace(m.ID))
	if !strings.HasPrefix(id, "devin/") {
		id = "devin/" + id
	}
	m.ID = id
	if m.Object == "" {
		m.Object = "model"
	}
	if m.Type == "" {
		m.Type = "devin"
	}
	if len(m.SupportedInputModalities) == 0 {
		m.SupportedInputModalities = []string{"text"}
	}
	if len(m.SupportedOutputModalities) == 0 {
		m.SupportedOutputModalities = []string{"text"}
	}
	if m.InputTokenLimit == 0 && m.ContextLength > 0 {
		m.InputTokenLimit = m.ContextLength
	}
	if m.OutputTokenLimit == 0 && m.MaxCompletionTokens > 0 {
		m.OutputTokenLimit = m.MaxCompletionTokens
	}
	if len(m.SupportedGenerationMethods) == 0 {
		m.SupportedGenerationMethods = []string{"generateContent", "countTokens"}
	}
}

// aggregateRawModels turns raw upstream uid rows into aggregated base models
// (ported from cmd/fetch_devin_models.aggregateModels).
func aggregateRawModels(raw []rawDevinModel, existingLevels func(base string) []string) []*modelInfo {
	type aggEntry struct {
		baseID        string
		displayName   string
		vendorID      uint64
		contextLength int
		multimodal    bool
		levels        map[string]struct{}
	}
	grouped := make(map[string]*aggEntry)
	var order []string

	for _, r := range raw {
		if strings.TrimSpace(r.UID) == "" {
			continue
		}
		base, level := splitDevinUID(r.UID)
		if base == "" {
			base = r.UID
		}
		isBase := base == r.UID

		entry, exists := grouped[base]
		if !exists {
			initial := make(map[string]struct{})
			if existingLevels != nil {
				for _, l := range existingLevels(base) {
					if l != "" && l != "priority" {
						initial[l] = struct{}{}
					}
				}
			}
			entry = &aggEntry{
				baseID:      base,
				displayName: cleanDevinDisplayName(r.Label),
				vendorID:    r.VendorID,
				multimodal:  r.Multimodal,
				levels:      initial,
			}
			if r.ContextLength > 0 {
				entry.contextLength = r.ContextLength
			}
			grouped[base] = entry
			order = append(order, base)
		}
		if isBase {
			entry.displayName = cleanDevinDisplayName(r.Label)
			if r.VendorID != 0 {
				entry.vendorID = r.VendorID
			}
		}
		if r.Multimodal {
			entry.multimodal = true
		}
		if r.ContextLength > entry.contextLength {
			entry.contextLength = r.ContextLength
		}
		if level != "" && level != "priority" {
			entry.levels[level] = struct{}{}
		}
	}

	out := make([]*modelInfo, 0, len(order))
	for _, base := range order {
		e := grouped[base]
		modalities := []string{"text"}
		if e.multimodal {
			modalities = append(modalities, "image")
		}
		var thinking *thinkingSupport
		if len(e.levels) > 0 {
			levels := make([]string, 0, len(e.levels))
			for l := range e.levels {
				levels = append(levels, l)
			}
			sortDevinLevels(levels)
			thinking = &thinkingSupport{Levels: levels}
		}
		m := &modelInfo{
			ID:                        "devin/" + strings.ToLower(base),
			Object:                    "model",
			Type:                      "devin",
			OwnedBy:                   vendorName(e.vendorID, base),
			DisplayName:               e.displayName,
			ContextLength:             int64(e.contextLength),
			MaxCompletionTokens:       64000,
			SupportedInputModalities:  modalities,
			SupportedOutputModalities: []string{"text"},
			Thinking:                  thinking,
		}
		normalizeCatalogModel(m)
		out = append(out, m)
	}
	return out
}

// mergeBuckets computes the union of baseline ∪ curated ∪ live keyed by
// lowercased id. Later sources override same-id entries field-wise; thinking
// levels are unioned so curated levels survive a sparser live list.
func mergeBuckets(baseline, curated, live []*modelInfo) []*modelInfo {
	order := []string{}
	byID := map[string]*modelInfo{}
	levels := map[string]map[string]struct{}{}

	put := func(list []*modelInfo, prefer bool) {
		for _, m := range list {
			if m == nil {
				continue
			}
			c := cloneModel(m)
			normalizeCatalogModel(c)
			key := strings.ToLower(c.ID)
			lv, ok := levels[key]
			if !ok {
				lv = map[string]struct{}{}
				levels[key] = lv
			}
			if c.Thinking != nil {
				for _, l := range c.Thinking.Levels {
					if l != "" {
						lv[l] = struct{}{}
					}
				}
			}
			if _, exists := byID[key]; !exists {
				byID[key] = c
				order = append(order, key)
			} else if prefer {
				old := byID[key]
				merged := *c
				if merged.DisplayName == "" {
					merged.DisplayName = old.DisplayName
				}
				if merged.ContextLength == 0 {
					merged.ContextLength = old.ContextLength
				}
				if merged.InputTokenLimit == 0 {
					merged.InputTokenLimit = old.InputTokenLimit
				}
				if merged.OutputTokenLimit == 0 {
					merged.OutputTokenLimit = old.OutputTokenLimit
				}
				if merged.MaxCompletionTokens == 0 {
					merged.MaxCompletionTokens = old.MaxCompletionTokens
				}
				if merged.OwnedBy == "" {
					merged.OwnedBy = old.OwnedBy
				}
				byID[key] = &merged
			}
		}
	}
	put(baseline, false)
	put(curated, true)
	put(live, true)

	out := make([]*modelInfo, 0, len(order))
	for _, key := range order {
		m := byID[key]
		if lv := levels[key]; len(lv) > 0 {
			levelsList := make([]string, 0, len(lv))
			for l := range lv {
				levelsList = append(levelsList, l)
			}
			sortDevinLevels(levelsList)
			if m.Thinking == nil {
				m.Thinking = &thinkingSupport{}
			}
			m.Thinking.Levels = levelsList
		}
		out = append(out, m)
	}
	return out
}

// snapshot returns the merged catalog.
func (s *catalogStore) snapshot() []*modelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return mergeBuckets(s.baseline, s.curated, s.live)
}

package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// emitVariants expands each base model into per-effort entries so brand-new
// models are directly addressable as devin/<base>-<level>.
func emitVariants(base []*modelInfo) []*modelInfo {
	out := make([]*modelInfo, 0, len(base)*2)
	for _, m := range base {
		out = append(out, m)
		if m == nil || m.Thinking == nil {
			continue
		}
		for _, lv := range m.Thinking.Levels {
			if lv == "" || lv == "priority" {
				continue
			}
			v := cloneModel(m)
			v.ID = m.ID + "-" + lv
			v.DisplayName = strings.TrimSpace(m.DisplayName + " " + variantLevelLabel(lv))
			v.Thinking = nil
			out = append(out, v)
		}
	}
	return out
}

func variantLevelLabel(level string) string {
	if level == "" {
		return ""
	}
	return strings.ToUpper(level[:1]) + level[1:]
}

// applyLive merges a freshly fetched raw model list into the live bucket.
// Returns true if the merged catalog changed.
func (s *catalogStore) applyLive(raw []rawDevinModel, auths []string) bool {
	s.mu.RLock()
	liveModels := aggregateRawModels(raw, s.baselineLevelsFor)
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	before := mergeBuckets(s.baseline, s.curated, s.live)
	s.live = liveModels
	s.liveRaw = append([]rawDevinModel(nil), raw...)
	s.authsUsed = append([]string(nil), auths...)
	after := mergeBuckets(s.baseline, s.curated, s.live)

	s.lastPoll = timeNowUTC()
	if catalogEqual(before, after) {
		s.lastLive = fmt.Sprintf("fetched %d raw rows from %s, catalog unchanged", len(raw), strings.Join(auths, ","))
		s.lastError = ""
		return false
	}
	s.revision++
	s.lastLive = fmt.Sprintf("fetched %d raw rows from %s, catalog changed (rev %d)", len(raw), strings.Join(auths, ","), s.revision)
	s.lastError = ""
	return true
}

func (s *catalogStore) applyCurated(models []*modelInfo, source string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := mergeBuckets(s.baseline, s.curated, s.live)
	s.curated = models
	after := mergeBuckets(s.baseline, s.curated, s.live)
	if catalogEqual(before, after) {
		return false
	}
	s.revision++
	s.lastLive = fmt.Sprintf("curated catalog updated from %s (rev %d)", source, s.revision)
	return true
}

func (s *catalogStore) noteError(err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err
	s.lastPoll = timeNowUTC()
}

// lastLiveText returns the last live-merge summary under lock.
func (s *catalogStore) lastLiveText() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastLive
}

// signature returns a stable content hash of the merged catalog's model IDs —
// used to dedupe propagation across plugin reloads.
func (s *catalogStore) signature() string {
	merged := s.snapshot()
	if len(merged) == 0 {
		return ""
	}
	ids := make([]string, 0, len(merged))
	for _, m := range merged {
		if m != nil {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	h := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(h[:8])
}

// baselineLevelsFor returns thinking levels the current catalog knows for a
// base id (preserves curated effort info when live rows only hint at it).
func (s *catalogStore) baselineLevelsFor(base string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := "devin/" + strings.ToLower(base)
	for _, bucket := range [][]*modelInfo{s.baseline, s.curated, s.live} {
		for _, m := range bucket {
			if m == nil {
				continue
			}
			if strings.EqualFold(m.ID, key) && m.Thinking != nil {
				return m.Thinking.Levels
			}
		}
	}
	return nil
}

func catalogEqual(a, b []*modelInfo) bool {
	if len(a) != len(b) {
		return false
	}
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(aj) == string(bj)
}

// statusSnapshot is emitted by the management status route.
type statusSnapshot struct {
	Enabled      bool     `json:"enabled"`
	Revision     uint64   `json:"revision"`
	ModelCount   int      `json:"model_count"`
	VariantCount int      `json:"variant_count"`
	BaselineN    int      `json:"baseline_count"`
	CuratedN     int      `json:"curated_count"`
	LiveN        int      `json:"live_count"`
	LastPoll     string   `json:"last_poll"`
	LastChange   string   `json:"last_change"`
	LastError    string   `json:"last_error,omitempty"`
	AuthsUsed    []string `json:"auths_used,omitempty"`
	Upstream     string   `json:"upstream"`
}

func (s *catalogStore) status(enabled bool, emitVariant bool) statusSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	merged := mergeBuckets(s.baseline, s.curated, s.live)
	variants := 0
	if emitVariant {
		variants = len(emitVariants(merged)) - len(merged)
	}
	return statusSnapshot{
		Enabled:      enabled,
		Revision:     s.revision,
		ModelCount:   len(merged),
		VariantCount: variants,
		BaselineN:    len(s.baseline),
		CuratedN:     len(s.curated),
		LiveN:        len(s.live),
		LastPoll:     s.lastPoll,
		LastChange:   s.lastLive,
		LastError:    s.lastError,
		AuthsUsed:    s.authsUsed,
		Upstream:     devinDefaultBaseURL + devinGetCliModelConfigsPath,
	}
}

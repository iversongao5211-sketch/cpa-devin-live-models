package core

import (
	"sort"
	"strings"
)

// ---------------- suffix/effort parsing (ported from registry + fetch tool) ----------------

var devinCompoundSuffixes = []struct {
	suffix string
	effort string
	readd  string
}{
	{suffix: "-low-fast", effort: "low"},
	{suffix: "-medium-fast", effort: "medium"},
	{suffix: "-high-fast", effort: "high"},
	{suffix: "-xhigh-fast", effort: "xhigh"},
	{suffix: "-max-fast", effort: "max"},
	{suffix: "-none-fast", effort: "none"},
	{suffix: "-low-priority", effort: "low"},
	{suffix: "-medium-priority", effort: "medium"},
	{suffix: "-high-priority", effort: "high"},
	{suffix: "-xhigh-priority", effort: "xhigh"},
	{suffix: "-max-priority", effort: "max"},
	{suffix: "-none-priority", effort: "none"},
	{suffix: "-thinking-1m", effort: "", readd: "-1m"},
	{suffix: "-thinking", effort: ""},
	{suffix: "-max-1m", effort: "max", readd: "-1m"},
	{suffix: "-none-1m", effort: "none", readd: "-1m"},
}

var devinSimpleEffortSuffixes = []struct {
	suffix string
	effort string
}{
	{suffix: "-none", effort: "none"},
	{suffix: "-minimal", effort: "minimal"},
	{suffix: "-low", effort: "low"},
	{suffix: "-medium", effort: "medium"},
	{suffix: "-high", effort: "high"},
	{suffix: "-xhigh", effort: "xhigh"},
	{suffix: "-max", effort: "max"},
}

// splitDevinUID splits a raw upstream uid into (base, effort), mirroring
// registry.splitDevinModelID / fetch_devin_models.splitDevinUID.
func splitDevinUID(uid string) (string, string) {
	if uid == "swe-1-6-slow" {
		return uid, ""
	}
	if uid == "swe-1-6-fast" {
		return "swe-1-6", ""
	}

	upper := strings.ToUpper(uid)
	for _, s := range []struct {
		suffix string
		effort string
	}{
		{"_NONE", "none"},
		{"_MINIMAL", "minimal"},
		{"_LOW", "low"},
		{"_MEDIUM", "medium"},
		{"_HIGH", "high"},
		{"_XHIGH", "xhigh"},
		{"_MAX", "max"},
		{"_THINKING", "high"},
	} {
		if strings.HasSuffix(upper, s.suffix) {
			base := uid[:len(uid)-len(s.suffix)]
			return base, s.effort
		}
	}

	for _, s := range devinCompoundSuffixes {
		if strings.HasSuffix(uid, s.suffix) {
			base := uid[:len(uid)-len(s.suffix)]
			if s.readd != "" {
				base += s.readd
			}
			return base, s.effort
		}
	}

	for _, s := range devinSimpleEffortSuffixes {
		if strings.HasSuffix(uid, s.suffix) {
			base := uid[:len(uid)-len(s.suffix)]
			return base, s.effort
		}
	}

	return uid, ""
}

var devinDisplayNameSuffixes = []string{
	" Low Fast", " Medium Fast", " High Fast", " XHigh Fast", " Max Fast",
	" Low Thinking Fast", " Medium Thinking Fast", " High Thinking Fast",
	" XHigh Thinking Fast", " Max Thinking Fast", " No Thinking Fast",
	" Low Thinking", " Medium Thinking", " High Thinking", " XHigh Thinking",
	" Max Thinking", " No Thinking",
	" Low", " Medium", " High", " XHigh", " Max", " None", " Minimal",
	" Thinking", " Fast",
}

func cleanDevinDisplayName(name string) string {
	trimmed := strings.TrimSpace(name)
	for {
		changed := false
		for _, s := range devinDisplayNameSuffixes {
			if strings.HasSuffix(strings.ToLower(trimmed), strings.ToLower(s)) {
				trimmed = strings.TrimSpace(trimmed[:len(trimmed)-len(s)])
				changed = true
				break
			}
		}
		if !changed {
			break
		}
	}
	return trimmed
}

var devinLevelOrder = map[string]int{
	"none": 0, "minimal": 1, "low": 2, "medium": 3,
	"high": 4, "xhigh": 5, "max": 6, "fast": 7, "priority": 8,
}

func sortDevinLevels(levels []string) {
	sort.Slice(levels, func(i, j int) bool {
		rI, okI := devinLevelOrder[levels[i]]
		if !okI {
			rI = 99
		}
		rJ, okJ := devinLevelOrder[levels[j]]
		if !okJ {
			rJ = 99
		}
		if rI != rJ {
			return rI < rJ
		}
		return levels[i] < levels[j]
	})
}

func vendorName(id uint64, uid string) string {
	switch id {
	case 1:
		return "cognition"
	case 2:
		return "openai"
	case 3:
		return "anthropic"
	case 4:
		return "google"
	case 6:
		return "deepseek"
	case 7:
		return "moonshot"
	case 9:
		return "zhipu"
	case 11:
		return "nvidia"
	default:
		if strings.Contains(strings.ToLower(uid), "grok") {
			return "xai"
		}
		return "devin"
	}
}

// matchWildcard performs case-insensitive wildcard matching where '*' matches
// any substring — same semantics as sdk/cliproxy.matchWildcard.
func matchWildcard(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "" {
		return false
	}
	if !strings.Contains(pattern, "*") {
		return pattern == value
	}
	parts := strings.Split(pattern, "*")
	if prefix := parts[0]; prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = value[len(prefix):]
	}
	if suffix := parts[len(parts)-1]; suffix != "" {
		if !strings.HasSuffix(value, suffix) {
			return false
		}
		value = value[:len(value)-len(suffix)]
	}
	for i := 1; i < len(parts)-1; i++ {
		segment := parts[i]
		if segment == "" {
			continue
		}
		idx := strings.Index(value, segment)
		if idx < 0 {
			return false
		}
		value = value[idx+len(segment):]
	}
	return true
}

func isExcluded(modelID string, patterns []string) bool {
	for _, p := range patterns {
		if matchWildcard(p, modelID) {
			return true
		}
	}
	return false
}

// knownDevinSuffixes mirrors helps.knownDevinSuffixes — recognized effort
// suffixes on upstream chat_model_uids (used to detect explicit-effort model
// names the router must not rewrite).
var knownDevinSuffixes = []string{
	"-none", "-low", "-medium", "-high", "-xhigh", "-max",
	"-fast", "-slow", "-priority",
	"-low-priority", "-medium-priority", "-high-priority",
	"-xhigh-priority", "-max-priority",
	"-low-fast", "-medium-fast", "-high-fast", "-xhigh-fast",
	"-max-fast", "-none-fast",
	"-thinking-1m", "-thinking", "-max-1m", "-none-1m",
	"_none", "_minimal", "_low", "_medium", "_high", "_xhigh", "_max", "_thinking",
}

// hasDevinEffortSuffix reports whether the model id already carries an
// explicit effort suffix.
func hasDevinEffortSuffix(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, s := range knownDevinSuffixes {
		if strings.HasSuffix(lower, s) {
			return true
		}
	}
	return false
}

package core

import (
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// pluginConfig is decoded from the plugin's own YAML node
// (plugins.configs.cpa-devin-live-models).
type pluginConfig struct {
	Enabled           bool     `yaml:"-"`
	PollInterval      string   `yaml:"-"`
	PollIntervalDur   time.Duration
	FetchTimeout      string   `yaml:"-"`
	FetchTimeoutDur   time.Duration
	AccountsPerPoll   int      `yaml:"-"`
	PropagateOnChange bool     `yaml:"-"`
	EmitVariants      bool     `yaml:"-"`
	DefaultEffort     string   `yaml:"-"`
	CuratedURLs       []string `yaml:"-"`
	ProxyURL          string   `yaml:"-"`
	BaselineFile      string   `yaml:"-"`
	Log               bool     `yaml:"-"`
}

var defaultCuratedURLs = []string{
	"https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/devin_models.json",
	"https://models.router-for.me/devin_models.json",
}

func defaultConfig() pluginConfig {
	return pluginConfig{
		Enabled:           true,
		PollIntervalDur:   5 * time.Minute,
		FetchTimeoutDur:   30 * time.Second,
		AccountsPerPoll:   3,
		PropagateOnChange: true,
		EmitVariants:      true,
		DefaultEffort:     "high",
		CuratedURLs:       append([]string(nil), defaultCuratedURLs...),
		Log:               true,
	}
}

// parseConfig decodes the YAML node delivered via config_yaml. Unknown keys are
// ignored; absent keys keep defaults.
func parseConfig(raw []byte) pluginConfig {
	cfg := defaultConfig()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return cfg
	}
	str := func(keys ...string) (string, bool) {
		for _, k := range keys {
			v, ok := doc[k]
			if !ok {
				continue
			}
			switch t := v.(type) {
			case string:
				return strings.TrimSpace(t), true
			case bool:
				return strconv.FormatBool(t), true
			case float64:
				return strconv.FormatFloat(t, 'f', -1, 64), true
			case int:
				return strconv.Itoa(t), true
			}
		}
		return "", false
	}
	boolv := func(def bool, keys ...string) bool {
		if s, ok := str(keys...); ok {
			switch strings.ToLower(s) {
			case "true", "1", "yes", "on":
				return true
			case "false", "0", "no", "off":
				return false
			}
		}
		return def
	}
	intv := func(def int, keys ...string) int {
		if s, ok := str(keys...); ok {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				return n
			}
		}
		return def
	}

	cfg.Enabled = boolv(cfg.Enabled, "enabled")
	if s, ok := str("poll-interval", "poll_interval", "interval"); ok {
		cfg.PollInterval = s
		if d, err := time.ParseDuration(s); err == nil && d >= 15*time.Second {
			cfg.PollIntervalDur = d
		}
	}
	if s, ok := str("fetch-timeout", "fetch_timeout"); ok {
		cfg.FetchTimeout = s
		if d, err := time.ParseDuration(s); err == nil && d >= 3*time.Second && d <= 120*time.Second {
			cfg.FetchTimeoutDur = d
		}
	}
	cfg.AccountsPerPoll = intv(cfg.AccountsPerPoll, "accounts-per-poll", "accounts_per_poll")
	if cfg.AccountsPerPoll > 16 {
		cfg.AccountsPerPoll = 16
	}
	cfg.PropagateOnChange = boolv(cfg.PropagateOnChange, "propagate-on-change", "propagate_on_change")
	cfg.EmitVariants = boolv(cfg.EmitVariants, "emit-variants", "emit_variants")
	if s, ok := str("default-effort", "default_effort"); ok {
		cfg.DefaultEffort = s
	}
	cfg.Log = boolv(cfg.Log, "log")
	if s, ok := str("proxy-url", "proxy_url"); ok {
		cfg.ProxyURL = s
	}
	if s, ok := str("baseline-file", "baseline_file"); ok {
		cfg.BaselineFile = s
	}
	if v, exists := doc["curated-urls"]; exists {
		if arr, ok := v.([]any); ok {
			urls := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					urls = append(urls, strings.TrimSpace(s))
				}
			}
			cfg.CuratedURLs = urls
		} else if v == nil {
			cfg.CuratedURLs = nil
		}
	}
	return cfg
}

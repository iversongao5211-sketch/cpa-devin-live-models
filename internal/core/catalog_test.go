package core

import (
	"strings"
	"testing"
)

func TestSplitDevinUID(t *testing.T) {
	cases := []struct{ uid, wantBase, wantEffort string }{
		{"swe-2", "swe-2", ""},
		{"swe-2-high", "swe-2", "high"},
		{"swe-2-max", "swe-2", "max"},
		{"swe-1-7", "swe-1-7", ""},
		{"swe-1-7-lightning-medium", "swe-1-7-lightning", "medium"},
		{"swe-1-6", "swe-1-6", ""},
		{"swe-1-6-fast", "swe-1-6", ""},
		{"swe-1-6-slow", "swe-1-6-slow", ""},
		{"MODEL_GPT_5_2_LOW", "MODEL_GPT_5_2", "low"},
		{"MODEL_GPT_5_2", "MODEL_GPT_5_2", ""},
		{"MODEL_CLAUDE_4_5_OPUS_THINKING", "MODEL_CLAUDE_4_5_OPUS", "high"},
		{"claude-opus-4-8-low-fast", "claude-opus-4-8", "low"},
		{"claude-opus-4-6-thinking-1m", "claude-opus-4-6-1m", ""},
		{"claude-opus-4-6-max-1m", "claude-opus-4-6-1m", "max"},
		{"glm-5-2-none", "glm-5-2", "none"},
		{"glm-5-2-1m", "glm-5-2-1m", ""},
		{"gpt-6-1-sol-high", "gpt-6-1-sol", "high"},
	}
	for _, c := range cases {
		base, effort := splitDevinUID(c.uid)
		if base != c.wantBase || effort != c.wantEffort {
			t.Errorf("splitDevinUID(%q) = (%q,%q), want (%q,%q)", c.uid, base, effort, c.wantBase, c.wantEffort)
		}
	}
}

func TestAggregateRawModels(t *testing.T) {
	raw := []rawDevinModel{
		{UID: "new-model-1", Label: "New Model 1", VendorID: 2, ContextLength: 1000000},
		{UID: "new-model-1-low", Label: "New Model 1 Low", VendorID: 2, ContextLength: 1000000},
		{UID: "new-model-1-high", Label: "New Model 1 High", VendorID: 2, ContextLength: 1000000},
		{UID: "solo", Label: "Solo", VendorID: 1, ContextLength: 200000},
	}
	models := aggregateRawModels(raw, nil)
	if len(models) != 2 {
		t.Fatalf("expected 2 aggregated models, got %d", len(models))
	}
	var nm *modelInfo
	for _, m := range models {
		if m.ID == "devin/new-model-1" {
			nm = m
		}
	}
	if nm == nil {
		t.Fatal("devin/new-model-1 not aggregated")
	}
	if nm.OwnedBy != "openai" {
		t.Errorf("owned_by = %q, want openai", nm.OwnedBy)
	}
	if nm.Thinking == nil || len(nm.Thinking.Levels) != 2 {
		t.Fatalf("thinking levels = %+v", nm.Thinking)
	}
	if nm.Thinking.Levels[0] != "low" || nm.Thinking.Levels[1] != "high" {
		t.Errorf("levels order = %v", nm.Thinking.Levels)
	}
}

func TestMergeBucketsUnion(t *testing.T) {
	base := []*modelInfo{{ID: "devin/a"}, {ID: "devin/b"}}
	cur := []*modelInfo{{ID: "devin/b", DisplayName: "B"}}
	live := []*modelInfo{{ID: "devin/c", ContextLength: 42}}
	merged := mergeBuckets(base, cur, live)
	if len(merged) != 3 {
		t.Fatalf("union len = %d", len(merged))
	}
	ids := map[string]bool{}
	for _, m := range merged {
		ids[m.ID] = true
	}
	for _, want := range []string{"devin/a", "devin/b", "devin/c"} {
		if !ids[want] {
			t.Errorf("missing %s", want)
		}
	}
}

func TestMatchWildcard(t *testing.T) {
	cases := []struct{ pat, val string; want bool }{
		{"devin/gpt-5-4", "devin/gpt-5-4", true},
		{"devin/gpt-5-4", "devin/gpt-5-5", false},
		{"devin/*", "devin/anything", true},
		{"*-high", "devin/x-high", true},
		{"devin/gpt-*-sol", "devin/gpt-6-1-sol", true},
		{"devin/gpt-*-sol", "devin/gpt-6-1-luna", false},
		{"", "x", false},
		{"devin/model_private_*", "devin/model_private_12", true},
	}
	for _, c := range cases {
		if got := matchWildcard(c.pat, c.val); got != c.want {
			t.Errorf("matchWildcard(%q,%q)=%v want %v", c.pat, c.val, got, c.want)
		}
	}
}

func TestParseDevinModelsPayload(t *testing.T) {
	payload := []byte(`{"devin":[{"id":"swe-2","type":"devin","owned_by":"cognition","display_name":"SWE-2","context_length":262000}]}`)
	models, err := parseDevinModelsPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "swe-2" {
		t.Fatalf("unexpected models %+v", models)
	}
	// normalize should namespace it
	normalizeCatalogModel(models[0])
	if models[0].ID != "devin/swe-2" {
		t.Errorf("normalize -> %s", models[0].ID)
	}
}

func TestEmitVariants(t *testing.T) {
	base := []*modelInfo{
		{ID: "devin/x", Thinking: &thinkingSupport{Levels: []string{"low", "high"}}},
		{ID: "devin/y"},
	}
	out := emitVariants(base)
	if len(out) != 4 {
		t.Fatalf("emitVariants len = %d, want 4", len(out))
	}
	var hasXHigh bool
	for _, m := range out {
		if m.ID == "devin/x-high" {
			hasXHigh = true
			if m.Thinking != nil {
				t.Error("variant should have nil Thinking")
			}
		}
	}
	if !hasXHigh {
		t.Error("missing devin/x-high variant")
	}
}

func TestCredsFromAuthJSON(t *testing.T) {
	raw := []byte(`{"type":"devin","api_key":"tok123","session_token":"tok456","proxy_url":"socks5://h:1"}`)
	c := credsFromAuthJSON(raw)
	if c.SessionToken != "tok123" {
		t.Errorf("session token = %q", c.SessionToken)
	}
	if c.ProxyURL != "socks5://h:1" {
		t.Errorf("proxy = %q", c.ProxyURL)
	}
	// fallback to session_token
	raw2 := []byte(`{"session_token":"tok456"}`)
	if c := credsFromAuthJSON(raw2); c.SessionToken != "tok456" {
		t.Errorf("fallback session token = %q", c.SessionToken)
	}
}

func TestParseConfig(t *testing.T) {
	yaml := []byte("enabled: true\npoll-interval: \"10m\"\naccounts-per-poll: 5\npropagate-on-change: false\ncurated-urls: []\n")
	cfg := parseConfig(yaml)
	if cfg.PollIntervalDur.String() != "10m0s" {
		t.Errorf("interval = %s", cfg.PollIntervalDur)
	}
	if cfg.AccountsPerPoll != 5 {
		t.Errorf("accounts = %d", cfg.AccountsPerPoll)
	}
	if cfg.PropagateOnChange {
		t.Error("propagate should be false")
	}
	if len(cfg.CuratedURLs) != 0 {
		t.Error("curated should be empty")
	}
	if !cfg.EmitVariants {
		t.Error("emit-variants default should stay true")
	}
}

func TestStoreApplyLiveChangeDetection(t *testing.T) {
	s := newCatalogStore(nil)
	raw := []rawDevinModel{{UID: "a-1", Label: "A1", VendorID: 1}}
	if !s.applyLive(raw, []string{"a"}) {
		t.Error("first apply should report change")
	}
	if s.applyLive(raw, []string{"a"}) {
		t.Error("identical re-apply should not report change")
	}
	if s.revision != 1 {
		t.Errorf("revision = %d", s.revision)
	}
}

func TestHandleForAuthNeverEmpty(t *testing.T) {
	p := NewPlugin(nil)
	out, err := p.HandleModelForAuth([]byte(`{"AuthProvider":"devin","AuthID":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "devin/swe-") && !strings.Contains(string(out), "devin/") {
		t.Errorf("for_auth response lacks models: %s", out[:200])
	}
}

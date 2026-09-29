package core

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

//go:embed baseline/devin_models.json
var embeddedBaselineJSON []byte

const (
	pluginID      = "cpa-devin-live-models"
	pluginName    = "Devin Live Models"
	pluginVersion = "1.0.0"
	pluginAuthor  = "szxypi"
	pluginRepo    = "https://github.com/iversongao5211-sketch/cpa-devin-live-models"
)

// Plugin holds the whole plugin state. All methods are safe to call from the
// host's dispatcher goroutine.
type Plugin struct {
	host Host

	cfgMu sync.RWMutex
	cfg   pluginConfig

	store  *catalogStore
	poller *poller

	started  atomic.Bool
	schema   uint32
	metadata pluginMetadata
}

// NewPlugin builds the plugin around a host call surface.
func NewPlugin(h Host) *Plugin {
	p := &Plugin{host: h, cfg: defaultConfig()}
	p.store = newCatalogStore(loadBaseline(p.cfg.BaselineFile))
	p.poller = newPoller(h, p.store, p.currentConfig)
	p.metadata = pluginMetadata{
		Name:             pluginName,
		Version:          pluginVersion,
		Author:           pluginAuthor,
		GitHubRepository: pluginRepo,
		Description:      "Fetches the live Devin/Cognition upstream model catalog via GetCliModelConfigs so brand-new devin/* models become listable and routable without a CPA rebuild.",
		ConfigFields: []configField{
			{Name: "poll-interval", Type: "string", Label: "Poll interval", Description: "How often to fetch the upstream catalog", Default: "5m"},
			{Name: "fetch-timeout", Type: "string", Label: "Fetch timeout", Description: "Per-request upstream timeout", Default: "30s"},
			{Name: "accounts-per-poll", Type: "integer", Label: "Accounts per poll", Description: "Devin credentials rotated into each fetch round", Default: 3},
			{Name: "propagate-on-change", Type: "boolean", Label: "Propagate on change", Description: "Touch devin auth files to force re-registration when the catalog changes", Default: true},
			{Name: "emit-variants", Type: "boolean", Label: "Emit effort variants", Description: "Also register devin/<model>-<level> entries for each thinking level", Default: true},
			{Name: "proxy-url", Type: "string", Label: "Fallback proxy", Description: "Proxy used when an auth has no proxy_url", Default: ""},
			{Name: "curated-urls", Type: "array", Label: "Curated catalog URLs", Description: "Additional devin_models.json sources to merge", Default: defaultCuratedURLs},
			{Name: "log", Type: "boolean", Label: "Verbose log", Default: true},
		},
	}
	return p
}

func (p *Plugin) currentConfig() pluginConfig {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
}

// loadBaseline parses the embedded catalog (plus optional file override).
func loadBaseline(override string) []*modelInfo {
	data := embeddedBaselineJSON
	if strings.TrimSpace(override) != "" {
		if raw, err := os.ReadFile(override); err == nil && len(raw) > 0 {
			data = raw
		}
	}
	models, err := parseDevinModelsPayload(data)
	if err != nil || len(models) == 0 {
		// last-resort minimal baseline: keep the provider mapping alive
		return []*modelInfo{{
			ID: "devin/swe-2", Object: "model", Type: "devin", OwnedBy: "cognition",
			DisplayName: "SWE-2", ContextLength: 262000, MaxCompletionTokens: 64000,
			InputTokenLimit: 262000, OutputTokenLimit: 64000,
			SupportedInputModalities:  []string{"text", "image"},
			SupportedOutputModalities: []string{"text"},
			Thinking:                  &thinkingSupport{Levels: []string{"medium", "high", "max"}},
		}}
	}
	for _, m := range models {
		normalizeCatalogModel(m)
	}
	return models
}

// ---------------- host->plugin entry points ----------------

// HandleRegister covers plugin.register AND plugin.reconfigure: decode config,
// (re)start the poll loop, return the capability handshake.
func (p *Plugin) HandleRegister(raw json.RawMessage) ([]byte, error) {
	req, cfgYAML, err := decodeLifecycle(raw)
	if err != nil {
		return nil, err
	}
	cfg := parseConfig(cfgYAML)
	p.cfgMu.Lock()
	p.cfg = cfg
	p.cfgMu.Unlock()

	if strings.TrimSpace(cfg.BaselineFile) != "" {
		// rebuild baseline bucket only if the override actually reads
		if models, err := parseDevinModelsPayload(mustReadFile(cfg.BaselineFile)); err == nil && len(models) > 0 {
			p.store.mu.Lock()
			for _, m := range models {
				normalizeCatalogModel(m)
			}
			p.store.baseline = models
			p.store.mu.Unlock()
		}
	}

	schema := req.SchemaVersion
	if schema == 0 || schema > 6 {
		schema = 6
	}
	p.schema = schema
	p.started.Store(true)
	p.poller.start()

	res := registrationResult{
		SchemaVersion: schema,
		Metadata:      p.metadata,
		Capabilities:  capabilityResult{ModelProvider: true, ManagementAPI: true},
	}
	return json.Marshal(res)
}

func mustReadFile(path string) []byte {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return raw
}

// HandleModelStatic covers model.static: the host calls this at plugin
// registration; the response establishes the devin provider association and
// seeds providerModels["devin"]. Must never return an error or an empty list.
func (p *Plugin) HandleModelStatic(raw json.RawMessage) ([]byte, error) {
	var req staticModelRequest
	_ = json.Unmarshal(raw, &req)
	cfg := p.currentConfig()
	models := p.catalogForWire(cfg)
	models = p.applyHostExclusions(models, req.Host)
	return json.Marshal(modelResponse{Provider: "devin", Models: models})
}

// HandleModelForAuth covers model.for_auth: per-auth model set. The host
// re-applies exclusions and aliases after our response, so return the full
// union unfiltered.
func (p *Plugin) HandleModelForAuth(raw json.RawMessage) ([]byte, error) {
	var req authModelRequest
	_ = json.Unmarshal(raw, &req)
	if !strings.EqualFold(strings.TrimSpace(req.AuthProvider), "devin") {
		// host pre-filters by provider; being defensive here costs nothing
		return json.Marshal(modelResponse{Provider: "devin", Models: nil})
	}
	cfg := p.currentConfig()
	return json.Marshal(modelResponse{Provider: "devin", Models: p.catalogForWire(cfg)})
}

// catalogForWire produces the model list to emit: union (+variants per config).
func (p *Plugin) catalogForWire(cfg pluginConfig) []modelInfo {
	merged := p.store.snapshot()
	if cfg.EmitVariants {
		merged = emitVariants(merged)
	}
	out := make([]modelInfo, 0, len(merged))
	for _, m := range merged {
		if m != nil {
			out = append(out, *m)
		}
	}
	return out
}

// applyHostExclusions filters wire models by the host-advertised exclusion
// patterns (protects the virtual-client/static path where the host does not
// apply exclusions itself).
func (p *Plugin) applyHostExclusions(models []modelInfo, host hostConfigSummary) []modelInfo {
	patterns := host.ExcludedModels["devin"]
	if len(patterns) == 0 {
		return models
	}
	out := models[:0]
	for _, m := range models {
		if !isExcluded(m.ID, patterns) {
			out = append(out, m)
		}
	}
	return out
}

// HandleManagementRegister covers management.register: declare our routes.
// NOTE: GET routes with a non-empty Menu are treated as legacy *resource*
// routes mounted under /v0/resource/plugins/<id>/, not /v0/management — leave
// Menu empty for JSON API endpoints.
func (p *Plugin) HandleManagementRegister(raw json.RawMessage) ([]byte, error) {
	return json.Marshal(managementRegistration{Routes: []managementRoute{
		{Method: "GET", Path: "plugins/" + pluginID + "/status", Description: "Live Devin catalog status"},
		{Method: "GET", Path: "plugins/" + pluginID + "/catalog", Description: "Dump the merged Devin catalog"},
		{Method: "POST", Path: "plugins/" + pluginID + "/refresh", Description: "Force an immediate catalog fetch"},
	}})
}

// HandleManagementHandle covers management.handle.
func (p *Plugin) HandleManagementHandle(raw json.RawMessage) ([]byte, error) {
	var req managementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return json.Marshal(managementResponse{StatusCode: 400, Body: []byte(`{"ok":false,"error":"bad request"}`)})
	}
	// req.Path is the FULL request path (e.g. /v0/management/plugins/<id>/status),
	// not the declared relative route — match on the tail segment.
	path := "/" + strings.Trim(req.Path, "/")
	suffix := path
	if idx := strings.Index(path, "plugins/"+pluginID); idx >= 0 {
		suffix = path[idx+len("plugins/"+pluginID):]
	}
	cfg := p.currentConfig()
	switch {
	case strings.EqualFold(req.Method, "GET") && suffix == "/status":
		st := p.store.status(cfg.Enabled, cfg.EmitVariants)
		return json.Marshal(jsonManagementResponse(200, st))
	case strings.EqualFold(req.Method, "GET") && suffix == "/catalog":
		merged := p.catalogForWire(cfg)
		ids := make([]string, 0, len(merged))
		for _, m := range merged {
			ids = append(ids, m.ID)
		}
		return json.Marshal(jsonManagementResponse(200, map[string]any{"models": ids, "count": len(ids)}))
	case strings.EqualFold(req.Method, "POST") && suffix == "/refresh":
		go p.poller.refresh()
		return json.Marshal(jsonManagementResponse(202, map[string]any{"ok": true, "accepted": true}))
	default:
		return json.Marshal(jsonManagementResponse(404, map[string]any{"ok": false, "error": "unknown route " + req.Method + " " + req.Path}))
	}
}

// HandleQuiesce covers plugin.quiesce: stop the loop, keep state.
func (p *Plugin) HandleQuiesce() {
	p.poller.stop()
}

// HandleShutdown covers plugin.shutdown: final cleanup.
func (p *Plugin) HandleShutdown() {
	p.poller.stop()
}

var _ = fmt.Sprintf // keep fmt import if unused in future edits

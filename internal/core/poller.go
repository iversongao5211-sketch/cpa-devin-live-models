package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// poller runs the periodic live-catalog fetch loop.
type poller struct {
	host  Host
	store *catalogStore
	cfgFn func() pluginConfig

	clients *clientCache

	mu           sync.Mutex
	running      bool
	stopCh       chan struct{}
	doneCh       chan struct{}
	lastRun      time.Time
	rotating     atomic.Uint64
	propagateSeq atomic.Uint64

	fetchMu sync.Mutex // single-flight across ticks and manual refresh
}

func newPoller(h Host, store *catalogStore, cfgFn func() pluginConfig) *poller {
	return &poller{host: h, store: store, cfgFn: cfgFn, clients: newClientCache(30 * time.Second)}
}

// start launches the loop if not running; interval changes take effect next tick.
func (p *poller) start() {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	p.running = true
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	p.mu.Unlock()

	go func() {
		defer close(p.doneCh)
		// small startup delay lets the host finish booting/auth scan
		select {
		case <-time.After(4 * time.Second):
		case <-p.stopCh:
			return
		}
		for {
			p.pollOnce()
			cfg := p.cfgFn()
			interval := cfg.PollIntervalDur
			if interval < 15*time.Second {
				interval = 15 * time.Second
			}
			// +-15% jitter
			jitter := time.Duration(float64(interval) * (0.85 + 0.3*float64(p.rotating.Add(1)%7)/6.0))
			select {
			case <-time.After(jitter):
			case <-p.stopCh:
				return
			}
		}
	}()
}

func (p *poller) stop() {
	p.mu.Lock()
	if !p.running {
		p.mu.Unlock()
		return
	}
	close(p.stopCh)
	done := p.doneCh
	p.running = false
	p.mu.Unlock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
	}
}

// refresh forces one poll synchronously (management endpoint).
func (p *poller) refresh() { p.pollOnce() }

func (p *poller) logf(level, format string, args ...any) {
	if !p.cfgFn().Log {
		return
	}
	p.host.Log(level, fmt.Sprintf(format, args...), map[string]any{"plugin": "cpa-devin-live-models"})
}

func (p *poller) pollOnce() {
	p.fetchMu.Lock()
	defer p.fetchMu.Unlock()
	p.lastRun = time.Now()

	cfg := p.cfgFn()
	if !cfg.Enabled {
		return
	}

	// 1) curated JSONs (public; no credential needed)
	if len(cfg.CuratedURLs) > 0 {
		for _, u := range cfg.CuratedURLs {
			if models, err := p.fetchCurated(u, cfg); err == nil {
				p.store.applyCurated(models, u)
				break // first reachable URL wins, like upstream updater
			}
		}
	}

	// 2) live upstream via pooled devin credentials
	auths, err := p.pickDevinAuths(cfg)
	if err != nil {
		p.store.noteError("auth list failed: " + err.Error())
		p.logf("warn", "devin live catalog: auth list failed: %v", err)
	} else if len(auths) == 0 {
		p.store.noteError("no enabled devin auth")
	} else {
		raw, used, lastErr := p.fetchUnion(auths, cfg)
		if len(raw) > 0 {
			if p.store.applyLive(raw, used) {
				p.logf("info", "devin live catalog: %s", p.store.lastLiveText())
			}
		} else {
			errStr := "all devin upstream fetches failed"
			if lastErr != nil {
				errStr = lastErr.Error()
			}
			p.store.noteError(errStr)
			p.logf("warn", "devin live catalog: %s", errStr)
		}
	}

	// 3) propagate: force re-registration so model.for_auth re-answers.
	// Signature-deduped inside propagate() — only writes when the merged
	// catalog actually differs from what the auth file last recorded, which
	// breaks the write->watcher->ApplyConfig->plugin-reload->write loop.
	if cfg.PropagateOnChange {
		p.propagate()
	}
}

func (p *poller) fetchCurated(url string, cfg pluginConfig) ([]*modelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.FetchTimeoutDur)
	defer cancel()
	cl, err := p.clients.clientFor(cfg.ProxyURL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelConfigsResponseSize))
	if err != nil {
		return nil, err
	}
	return parseDevinModelsPayload(body)
}

type devinAuthEntry struct {
	index    string
	name     string
	label    string
	creds    devinCredentials
}

func (p *poller) pickDevinAuths(cfg pluginConfig) ([]devinAuthEntry, error) {
	files, err := listAuths(p.host)
	if err != nil {
		return nil, err
	}
	var candidates []hostAuthFileEntry
	for _, f := range files {
		if f.Disabled || f.RuntimeOnly {
			continue
		}
		prov := strings.ToLower(strings.TrimSpace(f.Provider))
		typ := strings.ToLower(strings.TrimSpace(f.Type))
		if prov == "devin" || typ == "devin" {
			candidates = append(candidates, f)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// prefer available (non-cooling) auths, but fall back to all
	avail := make([]hostAuthFileEntry, 0, len(candidates))
	for _, c := range candidates {
		if !c.Unavailable {
			avail = append(avail, c)
		}
	}
	pool := avail
	if len(pool) == 0 {
		pool = candidates
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].Name < pool[j].Name })

	n := cfg.AccountsPerPoll
	if n <= 0 {
		n = 1
	}
	if n > len(pool) {
		n = len(pool)
	}
	start := int(p.rotating.Load() % uint64(len(pool)))
	picked := make([]devinAuthEntry, 0, n)
	for i := 0; i < n; i++ {
		f := pool[(start+i)%len(pool)]
		idx := f.AuthIndex
		if idx == "" {
			idx = f.ID
		}
		raw, err := getAuth(p.host, idx)
		if err != nil {
			p.logf("warn", "devin live catalog: auth get %s failed: %v", f.Name, err)
			continue
		}
		creds := credsFromAuthJSON(raw)
		if creds.SessionToken == "" {
			p.logf("warn", "devin live catalog: auth %s has no session token", f.Name)
			continue
		}
		if creds.ProxyURL == "" {
			creds.ProxyURL = cfg.ProxyURL
		}
		picked = append(picked, devinAuthEntry{index: idx, name: f.Name, label: f.Label, creds: creds})
	}
	return picked, nil
}

// fetchUnion fetches the raw catalog with up to len(auths) credentials
// sequentially; the union of successful responses is aggregated.
func (p *poller) fetchUnion(auths []devinAuthEntry, cfg pluginConfig) ([]rawDevinModel, []string, error) {
	seen := map[string]struct{}{}
	var union []rawDevinModel
	var used []string
	var lastErr error
	for _, a := range auths {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.FetchTimeoutDur)
		cl, err := p.clients.clientFor(a.creds.ProxyURL)
		var raw []rawDevinModel
		if err == nil {
			raw, err = fetchRawDevinModels(ctx, cl, a.creds.BaseURL, a.creds.SessionToken, a.creds.DeviceSeed, "")
		}
		cancel()
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", a.name, err)
			p.logf("warn", "devin live catalog: fetch via %s failed: %v", a.name, err)
			continue
		}
		used = append(used, a.name)
		for _, r := range raw {
			if _, ok := seen[r.UID]; ok {
				continue
			}
			seen[r.UID] = struct{}{}
			union = append(union, r)
		}
		if len(union) > 0 {
			break // first healthy credential is authoritative enough; others only add plan deltas next round
		}
	}
	return union, used, lastErr
}

// propagate bumps a marker field inside each devin auth file so the host fs
// watcher re-registers it (identical writes are sha-deduped upstream of us).
// propagate stamps the current catalog signature into each devin auth file so
// the host fs watcher re-registers it. A file is only rewritten when its
// recorded signature differs from the live one — byte-identical writes are
// sha-deduped by the watcher anyway, and signature comparison prevents the
// write -> ApplyConfig -> plugin reload -> propagate loop.
func (p *poller) propagate() {
	sig := p.store.signature()
	if sig == "" {
		return
	}
	files, err := listAuths(p.host)
	if err != nil {
		p.logf("warn", "devin live catalog: propagate auth list failed: %v", err)
		return
	}
	rev := p.store.revision
	touched := 0
	for _, f := range files {
		prov := strings.ToLower(strings.TrimSpace(f.Provider))
		typ := strings.ToLower(strings.TrimSpace(f.Type))
		if prov != "devin" && typ != "devin" {
			continue
		}
		idx := f.AuthIndex
		if idx == "" {
			idx = f.ID
		}
		raw, err := getAuth(p.host, idx)
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		if cur, ok := doc["_devin_live_sig"].(string); ok && cur == sig {
			continue
		}
		doc["_devin_live_sig"] = sig
		doc["_devin_live_rev"] = rev
		doc["_devin_live_touch"] = p.propagateSeq.Add(1)
		if err := saveAuth(p.host, f.Name, doc); err != nil {
			p.logf("warn", "devin live catalog: propagate save %s failed: %v", f.Name, err)
			continue
		}
		touched++
	}
	if touched > 0 {
		p.logf("info", "devin live catalog: propagated rev %d (sig %s…) to %d devin auth file(s)", rev, sig[:8], touched)
	}
}

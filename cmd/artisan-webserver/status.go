package main

// Public service status, derived from blackbox_exporter probes stored in
// Prometheus.
//
// Prometheus' HTTP API is unauthenticated and exposes every metric we scrape,
// so it is never reachable from a visitor's browser. This file proxies it:
// the queries run server-side, the result is reduced to the few fields the
// status page renders, and only targets named in STATUS_SERVICES are ever
// published.
//
// Two details of the monitoring setup drive the shape of the queries:
//
//   - The probed URL lives in the "target" label, not "instance". Relabeling
//     rewrites __address__ to the blackbox exporter's own address, so
//     "instance" is the exporter (10.5.0.11:9115) rather than the site.
//     Everything here aggregates by (target).
//   - Every target is probed twice, from two exporters. Probe disagreement is
//     what "Degraded" means: both up is Healthy, one up is Degraded, neither
//     is Down.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// defaultStatusSelector matches the four blackbox probe jobs
	// (blackbox_http_lt_400_probe_{a,b} and
	// blackbox_nextcloud_status_probe_{a,b}) while excluding
	// job="blackbox_exporter" and job="prometheus", which scrape exporter
	// internals rather than probe results.
	defaultStatusSelector = `job=~"blackbox_.+_probe_[ab]"`
	// defaultStatusCacheTTL matches Prometheus' scrape_interval. Polling
	// faster than the scrape interval cannot produce new information.
	defaultStatusCacheTTL = 30 * time.Second
	// statusHistoryDays is how many daily buckets the history strip carries.
	// Requires at least this much TSDB retention to be fully populated;
	// buckets with no data are published as null rather than as an outage.
	statusHistoryDays = 30
	// statusQueryTimeout bounds a full refresh. The 30d window scans roughly
	// 86k samples per series, so this is deliberately generous.
	statusQueryTimeout = 8 * time.Second
)

// statusUptimeWindows are the ranges the page reports uptime over, in the
// order they are rendered.
var statusUptimeWindows = []string{"24h", "7d", "30d"}

// statusState is the three-level vocabulary the status page renders. It
// matches the .badge ok/warn/bad classes in the site's components.css.
type statusState string

const (
	statusOK   statusState = "ok"
	statusWarn statusState = "warn"
	statusBad  statusState = "bad"
)

// statusTarget is one allowlisted entry: the probe target to look for and the
// name to publish it under. Because no friendly-name label exists in the
// scrape config, this map is the only source of display names -- which makes
// it the allowlist too. A target absent from it is never published.
type statusTarget struct {
	target string // normalized; see normalizeStatusTarget
	name   string
}

// statusConfig is the deployment's status configuration, read from the
// environment. Mirrors captchaConfig(): leaving PROMETHEUS_URL unset disables
// the endpoint cleanly rather than failing at startup.
type statusConfig struct {
	promURL    string
	bearer     string
	selector   string
	targets    []statusTarget
	degradedMS float64
	cacheTTL   time.Duration
	enabled    bool
}

// statusService is one published service. Nothing identifying the underlying
// infrastructure (instance, job, or the raw target URL) appears here.
type statusService struct {
	Name        string              `json:"name"`
	State       statusState         `json:"state"`
	HTTPStatus  int                 `json:"http_status,omitempty"`
	LatencyMS   int                 `json:"latency_ms,omitempty"`
	ProbesUp    int                 `json:"probes_up"`
	ProbesTotal int                 `json:"probes_total"`
	LastProbe   *time.Time          `json:"last_probe,omitempty"`
	Uptime      map[string]float64  `json:"uptime,omitempty"`
	History     []*statusHistoryDay `json:"history,omitempty"`
}

// statusHistoryDay is one daily bucket of the history strip. Everything past
// Availability is only filled in for days that were not perfect, and describes
// what monitoring observed -- never which host or exporter was involved.
type statusHistoryDay struct {
	Availability      float64 `json:"availability"`
	LocationsAffected int     `json:"locations_affected,omitempty"`
	LocationsTotal    int     `json:"locations_total,omitempty"`
	HTTPStatus        int     `json:"http_status,omitempty"`
	NoResponse        bool    `json:"no_response,omitempty"`
	ContentFailed     bool    `json:"content_failed,omitempty"`
}

// statusSnapshot is the JSON body served by /api/status.
type statusSnapshot struct {
	Enabled     bool            `json:"enabled"`
	GeneratedAt time.Time       `json:"generated_at"`
	Stale       bool            `json:"stale"`
	Overall     statusState     `json:"overall"`
	Windows     []string        `json:"windows"`
	Services    []statusService `json:"services"`
}

// statusDiagnostics reports allowlist/monitoring drift for the operator. It is
// logged, never served: naming a target we failed to match would defeat the
// point of the allowlist.
type statusDiagnostics struct {
	unmatchedTargets []string // probed by Prometheus, absent from the allowlist
	missingServices  []string // in the allowlist, no probe data found
}

// statusConfigFromEnv reads the status configuration. See README.md for the
// full variable table.
func statusConfigFromEnv() statusConfig {
	cfg := statusConfig{
		promURL:  strings.TrimSpace(os.Getenv("PROMETHEUS_URL")),
		bearer:   strings.TrimSpace(os.Getenv("PROMETHEUS_BEARER_TOKEN")),
		selector: strings.TrimSpace(os.Getenv("STATUS_SELECTOR")),
		targets:  parseStatusServices(os.Getenv("STATUS_SERVICES")),
		cacheTTL: defaultStatusCacheTTL,
	}
	if cfg.selector == "" {
		cfg.selector = defaultStatusSelector
	}
	if raw := strings.TrimSpace(os.Getenv("STATUS_DEGRADED_MS")); raw != "" {
		if ms, err := strconv.ParseFloat(raw, 64); err == nil && ms > 0 {
			cfg.degradedMS = ms
		} else if err != nil {
			log.Printf("status: ignoring unparseable STATUS_DEGRADED_MS %q: %v", raw, err)
		}
	}
	if raw := strings.TrimSpace(os.Getenv("STATUS_CACHE_TTL")); raw != "" {
		if ttl, err := time.ParseDuration(raw); err == nil && ttl > 0 {
			cfg.cacheTTL = ttl
		} else if err != nil {
			log.Printf("status: ignoring unparseable STATUS_CACHE_TTL %q: %v", raw, err)
		}
	}
	cfg.enabled = cfg.promURL != "" && len(cfg.targets) > 0
	return cfg
}

// parseStatusServices reads the "<target url>|<display name>" allowlist, one
// entry per line. Blank lines and #-comments are skipped, matching the
// tolerance of loadDotEnv. Order is preserved so the page renders in the
// order the operator wrote.
func parseStatusServices(raw string) []statusTarget {
	var targets []statusTarget
	seen := make(map[string]bool)
	for i, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		target, name, ok := strings.Cut(line, "|")
		if !ok {
			log.Printf("status: STATUS_SERVICES line %d: no '|' separator, skipping %q", i+1, line)
			continue
		}
		target = normalizeStatusTarget(target)
		name = strings.TrimSpace(name)
		if target == "" || name == "" {
			log.Printf("status: STATUS_SERVICES line %d: empty target or name, skipping %q", i+1, line)
			continue
		}
		if seen[target] {
			log.Printf("status: STATUS_SERVICES line %d: duplicate target %q, skipping", i+1, target)
			continue
		}
		seen[target] = true
		targets = append(targets, statusTarget{target: target, name: name})
	}
	return targets
}

// normalizeStatusTarget makes allowlist keys and Prometheus label values
// comparable. The target list mixes trailing slashes
// ("https://www.artisanhosting.net/" alongside
// "https://www.artisanstudio.net"), so an exact match would silently publish
// nothing.
func normalizeStatusTarget(target string) string {
	return strings.TrimSuffix(strings.TrimSpace(target), "/")
}

// --- Prometheus HTTP API ----------------------------------------------------

// promSample is one [unix_timestamp, "value"] pair. Prometheus encodes sample
// values as strings so that NaN and ±Inf survive JSON.
type promSample struct {
	At    time.Time
	Value float64
}

func (s *promSample) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw) != 2 {
		return fmt.Errorf("expected a [timestamp, value] pair, got %d elements", len(raw))
	}
	var ts float64
	if err := json.Unmarshal(raw[0], &ts); err != nil {
		return err
	}
	var text string
	if err := json.Unmarshal(raw[1], &text); err != nil {
		return err
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	sec, frac := math.Modf(ts)
	s.At = time.Unix(int64(sec), int64(frac*float64(time.Second))).UTC()
	s.Value = value
	return nil
}

// ok reports whether the sample carries a usable number. Prometheus returns
// NaN for absent data in some aggregations.
func (s promSample) ok() bool { return !math.IsNaN(s.Value) && !math.IsInf(s.Value, 0) }

// promSeries is one result entry. Value is populated for instant queries,
// Values for range queries.
type promSeries struct {
	Metric map[string]string `json:"metric"`
	Value  promSample        `json:"value"`
	Values []promSample      `json:"values"`
}

// promResponse is the standard Prometheus HTTP API envelope.
type promResponse struct {
	Status    string `json:"status"`
	ErrorType string `json:"errorType"`
	Error     string `json:"error"`
	Data      struct {
		ResultType string       `json:"resultType"`
		Result     []promSeries `json:"result"`
	} `json:"data"`
}

// promQueryFunc runs one Prometheus API call. Injected so the handler can be
// tested without a Prometheus, following the ...WithDependencies convention
// used by the rest of this package.
type promQueryFunc func(ctx context.Context, path string, params url.Values) ([]promSeries, error)

// newPromQuery returns a promQueryFunc talking to a real Prometheus. Requests
// are POSTed because the status queries are long enough to make URL length a
// consideration, and Prometheus documents POST for exactly that reason.
func newPromQuery(client *http.Client, base, bearer string) promQueryFunc {
	endpointBase := strings.TrimRight(base, "/")
	return func(ctx context.Context, path string, params url.Values) ([]promSeries, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointBase+path, strings.NewReader(params.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		var body promResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			// A non-200 with an undecodable body is more usefully reported by
			// its status than by the JSON error.
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("prometheus %s: %s", path, resp.Status)
			}
			return nil, fmt.Errorf("prometheus %s: %w", path, err)
		}
		if body.Status != "success" {
			detail := body.Error
			if detail == "" {
				detail = resp.Status
			}
			return nil, fmt.Errorf("prometheus %s: %s", path, detail)
		}
		return body.Data.Result, nil
	}
}

// --- Queries ----------------------------------------------------------------

// statusCurrentQuery returns every per-target aggregate the page needs as a
// single instant query, each clause tagged with an "agg" label so one round
// trip carries all five.
//
// "and probe_success == 1" intersects on the full label set, keeping a failing
// probe's timeout duration and missing status code out of the latency and HTTP
// figures. timestamp() reports the real scrape time; a sample's own timestamp
// would just be the query evaluation time.
func statusCurrentQuery(selector string) string {
	clauses := []string{
		`label_replace(sum by (target) (probe_success{` + selector + `}), "agg", "up", "", "")`,
		`label_replace(count by (target) (probe_success{` + selector + `}), "agg", "total", "", "")`,
		`label_replace(avg by (target) (probe_duration_seconds{` + selector + `} and probe_success{` + selector + `} == 1), "agg", "latency", "", "")`,
		`label_replace(max by (target) (probe_http_status_code{` + selector + `} and probe_success{` + selector + `} == 1), "agg", "http", "", "")`,
		`label_replace(max by (target) (timestamp(probe_success{` + selector + `})), "agg", "checked", "", "")`,
	}
	return strings.Join(clauses, " or ")
}

// statusUptimeQuery returns mean probe success per target for each window,
// tagged with a "window" label.
//
// Averaging across both probes means a single-vantage failure counts partially
// against uptime, which is consistent with calling that state Degraded rather
// than Healthy. The alternative -- "up from at least one vantage point" --
// needs a [30d:30s] subquery, which is an order of magnitude more expensive.
func statusUptimeQuery(selector string, windows []string) string {
	clauses := make([]string, 0, len(windows))
	for _, window := range windows {
		clauses = append(clauses,
			`label_replace(avg by (target) (avg_over_time(probe_success{`+selector+`}[`+window+`])), "window", "`+window+`", "", "")`)
	}
	return strings.Join(clauses, " or ")
}

// statusHistoryQuery returns one day's availability per target, plus enough
// detail to say what went wrong on the days that were not clean. Evaluated at a
// 1d step so the buckets do not overlap, and tagged with an "agg" label so all
// of it arrives in one range query.
//
// The failure story is assembled from what blackbox already exports: a
// per-location breakdown says how much of the redundancy was lost, a status
// code of 0 means a probe never got an HTTP response at all (DNS, TCP or TLS),
// and probe_failed_due_to_regex separates "responded, but with the wrong body"
// from "responded with a bad status".
func statusHistoryQuery(selector string) string {
	clauses := []string{
		`label_replace(avg by (target) (avg_over_time(probe_success{` + selector + `}[1d])), "agg", "availability", "", "")`,
		`label_replace(avg by (target, probe) (avg_over_time(probe_success{` + selector + `}[1d])), "agg", "location", "", "")`,
		`label_replace(min by (target) (min_over_time(probe_http_status_code{` + selector + `}[1d])), "agg", "http_min", "", "")`,
		`label_replace(max by (target) (max_over_time(probe_http_status_code{` + selector + `}[1d])), "agg", "http_max", "", "")`,
		`label_replace(max by (target) (max_over_time(probe_failed_due_to_regex{` + selector + `}[1d])), "agg", "regex", "", "")`,
	}
	return strings.Join(clauses, " or ")
}

// fetchStatusSnapshot runs the three queries and reduces them to a snapshot.
func fetchStatusSnapshot(ctx context.Context, cfg statusConfig, query promQueryFunc, now time.Time) (statusSnapshot, statusDiagnostics, error) {
	// Align buckets to the step so the last bucket ends at now and exactly
	// statusHistoryDays points come back.
	historyEnd := now.UTC().Truncate(time.Minute)
	historyStart := historyEnd.AddDate(0, 0, -(statusHistoryDays - 1))

	current, err := query(ctx, "/api/v1/query", url.Values{
		"query": {statusCurrentQuery(cfg.selector)},
	})
	if err != nil {
		return statusSnapshot{}, statusDiagnostics{}, err
	}

	uptime, err := query(ctx, "/api/v1/query", url.Values{
		"query": {statusUptimeQuery(cfg.selector, statusUptimeWindows)},
	})
	if err != nil {
		return statusSnapshot{}, statusDiagnostics{}, err
	}

	history, err := query(ctx, "/api/v1/query_range", url.Values{
		"query": {statusHistoryQuery(cfg.selector)},
		"start": {strconv.FormatInt(historyStart.Unix(), 10)},
		"end":   {strconv.FormatInt(historyEnd.Unix(), 10)},
		"step":  {"1d"},
	})
	if err != nil {
		return statusSnapshot{}, statusDiagnostics{}, err
	}

	snapshot, diags := buildStatusSnapshot(cfg, current, uptime, history, historyStart, now)
	return snapshot, diags, nil
}

// buildStatusSnapshot reduces raw query results to the published payload. It
// is pure so the reduction can be tested without a Prometheus.
func buildStatusSnapshot(cfg statusConfig, current, uptime, history []promSeries, historyStart, now time.Time) (statusSnapshot, statusDiagnostics) {
	// Accumulate into allowlist order; index by normalized target so the
	// trailing-slash inconsistency in the scrape config cannot cause a miss.
	type accumulator struct {
		service statusService
		seen    bool
	}
	accumulators := make([]accumulator, len(cfg.targets))
	byTarget := make(map[string]*accumulator, len(cfg.targets))
	for i, target := range cfg.targets {
		accumulators[i].service = statusService{Name: target.name}
		byTarget[target.target] = &accumulators[i]
	}

	unmatched := make(map[string]bool)
	lookup := func(metric map[string]string) *accumulator {
		target := normalizeStatusTarget(metric["target"])
		if target == "" {
			return nil
		}
		entry, ok := byTarget[target]
		if !ok {
			unmatched[target] = true
			return nil
		}
		return entry
	}

	for _, series := range current {
		entry := lookup(series.Metric)
		if entry == nil || !series.Value.ok() {
			continue
		}
		entry.seen = true
		switch series.Metric["agg"] {
		case "up":
			entry.service.ProbesUp = int(math.Round(series.Value.Value))
		case "total":
			entry.service.ProbesTotal = int(math.Round(series.Value.Value))
		case "latency":
			entry.service.LatencyMS = int(math.Round(series.Value.Value * 1000))
		case "http":
			entry.service.HTTPStatus = int(math.Round(series.Value.Value))
		case "checked":
			at := time.Unix(int64(series.Value.Value), 0).UTC()
			entry.service.LastProbe = &at
		}
	}

	for _, series := range uptime {
		entry := lookup(series.Metric)
		if entry == nil || !series.Value.ok() {
			continue
		}
		window := series.Metric["window"]
		if window == "" {
			continue
		}
		if entry.service.Uptime == nil {
			entry.service.Uptime = make(map[string]float64, len(statusUptimeWindows))
		}
		// Round to basis points; more precision than that is noise, and it
		// keeps 0.9999999 from rendering as 100%.
		entry.service.Uptime[window] = math.Round(series.Value.Value*10000) / 10000
	}

	histories := make(map[*accumulator][]historyDay, len(cfg.targets))
	for _, series := range history {
		entry := lookup(series.Metric)
		if entry == nil {
			continue
		}
		days, ok := histories[entry]
		if !ok {
			days = make([]historyDay, statusHistoryDays)
			histories[entry] = days
		}
		agg := series.Metric["agg"]
		for _, sample := range series.Values {
			if !sample.ok() {
				continue
			}
			index := int(math.Round(sample.At.Sub(historyStart).Hours() / 24))
			if index < 0 || index >= statusHistoryDays {
				continue
			}
			days[index].observe(agg, sample.Value)
		}
	}
	for entry, days := range histories {
		entry.service.History = finalizeHistory(days)
	}

	snapshot := statusSnapshot{
		Enabled:     true,
		GeneratedAt: now.UTC(),
		Overall:     statusOK,
		Windows:     statusUptimeWindows,
		Services:    make([]statusService, 0, len(cfg.targets)),
	}
	diags := statusDiagnostics{}

	for i := range accumulators {
		entry := &accumulators[i]
		// A configured target with no probe data is dropped rather than
		// published as Down: the overwhelmingly likely cause is a typo in
		// STATUS_SERVICES or a target removed from monitoring, and announcing
		// a false outage is worse than announcing nothing.
		if !entry.seen || entry.service.ProbesTotal == 0 {
			diags.missingServices = append(diags.missingServices, cfg.targets[i].target)
			continue
		}
		entry.service.State = deriveStatusState(entry.service, cfg.degradedMS)
		snapshot.Services = append(snapshot.Services, entry.service)
		snapshot.Overall = worseStatusState(snapshot.Overall, entry.service.State)
	}

	for target := range unmatched {
		diags.unmatchedTargets = append(diags.unmatchedTargets, target)
	}
	sort.Strings(diags.unmatchedTargets)

	return snapshot, diags
}

// historyDay accumulates the tagged aggregates for one daily bucket before
// they are reduced to the published statusHistoryDay.
type historyDay struct {
	availability    float64
	hasAvailability bool
	locationsTotal  int
	locationsLost   int
	httpMin         float64
	httpMax         float64
	hasHTTP         bool
	contentFailed   bool
}

// observe folds one sample into the day, keyed by the "agg" label its query
// clause tagged it with.
func (d *historyDay) observe(agg string, value float64) {
	switch agg {
	case "availability":
		d.availability = value
		d.hasAvailability = true
	case "location":
		// One series per monitoring location, so counting them gives both how
		// many were watching and how many had a bad day.
		d.locationsTotal++
		if value < 1 {
			d.locationsLost++
		}
	case "http_min":
		if !d.hasHTTP || value < d.httpMin {
			d.httpMin = value
		}
		d.hasHTTP = true
	case "http_max":
		if value > d.httpMax {
			d.httpMax = value
		}
	case "regex":
		if value > 0 {
			d.contentFailed = true
		}
	}
}

// finalizeHistory converts accumulated buckets to the published strip. A day
// Prometheus has no data for stays nil so the page can render it as "no data"
// rather than as an outage, and a clean day carries nothing but its
// availability -- there is no failure to describe.
func finalizeHistory(days []historyDay) []*statusHistoryDay {
	out := make([]*statusHistoryDay, len(days))
	for i := range days {
		day := days[i]
		if !day.hasAvailability {
			continue
		}
		published := &statusHistoryDay{Availability: math.Round(day.availability*10000) / 10000}
		if published.Availability < 1 {
			published.LocationsAffected = day.locationsLost
			published.LocationsTotal = day.locationsTotal
			published.ContentFailed = day.contentFailed
			// A status code of 0 is blackbox reporting that a probe never got
			// an HTTP response, which is a different story from a bad status.
			published.NoResponse = day.hasHTTP && day.httpMin == 0
			if day.httpMax >= 400 {
				published.HTTPStatus = int(math.Round(day.httpMax))
			}
		}
		out[i] = published
	}
	return out
}

// deriveStatusState maps probe agreement onto the page's three states. The
// optional degradedMS threshold is a secondary signal only: with it unset
// (the default) Degraded means precisely "one of two monitoring locations
// cannot reach this".
func deriveStatusState(service statusService, degradedMS float64) statusState {
	switch {
	case service.ProbesUp == 0:
		return statusBad
	case service.ProbesUp < service.ProbesTotal:
		return statusWarn
	case degradedMS > 0 && float64(service.LatencyMS) > degradedMS:
		return statusWarn
	default:
		return statusOK
	}
}

// worseStatusState returns whichever of the two states is more severe.
func worseStatusState(a, b statusState) statusState {
	severity := map[statusState]int{statusOK: 0, statusWarn: 1, statusBad: 2}
	if severity[b] > severity[a] {
		return b
	}
	return a
}

// --- Handler ----------------------------------------------------------------

// statusCache holds the most recent good snapshot. A public page should not
// go blank because one Prometheus query timed out, so a failed refresh falls
// back to serving the previous snapshot flagged stale.
type statusCache struct {
	mu        sync.RWMutex
	snapshot  statusSnapshot
	fetchedAt time.Time
	valid     bool

	// refreshing serializes refreshes so a burst of requests arriving on an
	// expired cache produces one set of Prometheus queries, not one per
	// request.
	refreshing sync.Mutex

	// logged tracks which drift diagnostics have already been reported, so a
	// misconfigured allowlist logs once rather than every cacheTTL.
	logged sync.Map
}

func newStatusCache() *statusCache { return &statusCache{} }

func (c *statusCache) read() (statusSnapshot, time.Time, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot, c.fetchedAt, c.valid
}

func (c *statusCache) store(snapshot statusSnapshot, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot = snapshot
	c.fetchedAt = at
	c.valid = true
}

// logOnce reports a message the first time a given key is seen.
func (c *statusCache) logOnce(key, format string, args ...any) {
	if _, seen := c.logged.LoadOrStore(key, true); seen {
		return
	}
	log.Printf(format, args...)
}

// statusHandlerWithDependencies serves the cached snapshot, refreshing it from
// Prometheus when it has aged past cfg.cacheTTL.
func statusHandlerWithDependencies(cfg statusConfig, query promQueryFunc, cache *statusCache, now func() time.Time) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")

		if !cfg.enabled {
			// Not an error: a deployment without PROMETHEUS_URL and
			// STATUS_SERVICES simply has no status to publish, and the page
			// shows its own unavailable card.
			writeStatusUnavailable(w, http.StatusServiceUnavailable, false)
			return
		}

		snapshot, ok := refreshStatusSnapshot(r.Context(), cfg, query, cache, now)
		if !ok {
			writeStatusUnavailable(w, http.StatusBadGateway, true)
			return
		}
		json.NewEncoder(w).Encode(snapshot)
	}
}

// writeStatusUnavailable reports that there is no status to serve. It sends a
// deliberately minimal body rather than a zero-valued snapshot, so a client
// cannot mistake placeholder fields for real readings.
func writeStatusUnavailable(w http.ResponseWriter, code int, enabled bool) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled})
}

// refreshStatusSnapshot returns a snapshot to serve, refreshing the cache if
// it has expired. It reports false only when there is nothing at all to serve:
// the cache is empty and Prometheus is unreachable.
func refreshStatusSnapshot(ctx context.Context, cfg statusConfig, query promQueryFunc, cache *statusCache, now func() time.Time) (statusSnapshot, bool) {
	if snapshot, fetchedAt, valid := cache.read(); valid && now().Sub(fetchedAt) < cfg.cacheTTL {
		return snapshot, true
	}

	cache.refreshing.Lock()
	defer cache.refreshing.Unlock()

	// Another request may have refreshed while we waited for the lock.
	if snapshot, fetchedAt, valid := cache.read(); valid && now().Sub(fetchedAt) < cfg.cacheTTL {
		return snapshot, true
	}

	queryCtx, cancel := context.WithTimeout(ctx, statusQueryTimeout)
	defer cancel()

	snapshot, diags, err := fetchStatusSnapshot(queryCtx, cfg, query, now())
	if err != nil {
		log.Printf("status: prometheus query failed: %v", err)
		if stale, _, valid := cache.read(); valid {
			stale.Stale = true
			return stale, true
		}
		return statusSnapshot{}, false
	}

	logStatusDiagnostics(cache, diags, len(snapshot.Services))
	cache.store(snapshot, now())
	return snapshot, true
}

// logStatusDiagnostics reports allowlist drift once per distinct problem.
func logStatusDiagnostics(cache *statusCache, diags statusDiagnostics, published int) {
	for _, target := range diags.missingServices {
		cache.logOnce("missing:"+target, "status: %q is in STATUS_SERVICES but Prometheus has no probe data for it; not publishing it. Check the target label matches exactly (trailing slashes are normalized).", target)
	}
	for _, target := range diags.unmatchedTargets {
		cache.logOnce("unmatched:"+target, "status: Prometheus probes %q but it is not in STATUS_SERVICES; not publishing it.", target)
	}
	if published == 0 {
		cache.logOnce("empty", "status: no services matched; /api/status will render as unavailable. Verify STATUS_SELECTOR (%s) and the target labels in STATUS_SERVICES.", defaultStatusSelector)
	}
}

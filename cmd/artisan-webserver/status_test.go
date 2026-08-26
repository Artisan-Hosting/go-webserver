package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedNow is an arbitrary but stable evaluation time for snapshot tests.
var fixedNow = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

func testStatusConfig() statusConfig {
	return statusConfig{
		promURL:  "http://prometheus.test",
		selector: defaultStatusSelector,
		targets: []statusTarget{
			{target: "https://www.artisanhosting.net", name: "Website"},
			{target: "https://cloud.artisanhosting.net/status.php", name: "Cloud Storage"},
		},
		cacheTTL: defaultStatusCacheTTL,
		enabled:  true,
	}
}

// instantSeries builds one instant-query result entry.
func instantSeries(labels map[string]string, value float64) promSeries {
	return promSeries{Metric: labels, Value: promSample{At: fixedNow, Value: value}}
}

// currentFor produces the five per-target aggregates statusCurrentQuery returns.
func currentFor(target string, up, total, latencySeconds, httpStatus float64) []promSeries {
	return []promSeries{
		instantSeries(map[string]string{"target": target, "agg": "up"}, up),
		instantSeries(map[string]string{"target": target, "agg": "total"}, total),
		instantSeries(map[string]string{"target": target, "agg": "latency"}, latencySeconds),
		instantSeries(map[string]string{"target": target, "agg": "http"}, httpStatus),
		instantSeries(map[string]string{"target": target, "agg": "checked"}, float64(fixedNow.Add(-15*time.Second).Unix())),
	}
}

func serviceByName(t *testing.T, snapshot statusSnapshot, name string) statusService {
	t.Helper()
	for _, service := range snapshot.Services {
		if service.Name == name {
			return service
		}
	}
	t.Fatalf("service %q not present; got %+v", name, snapshot.Services)
	return statusService{}
}

func TestParseStatusServicesSkipsMalformedEntries(t *testing.T) {
	targets := parseStatusServices(`
# a comment
https://www.artisanhosting.net/|Website

https://office.artisanhosting.net|Office
no-separator-here
https://blank.example.com|
|Nameless
https://www.artisanhosting.net|Duplicate
`)

	if len(targets) != 2 {
		t.Fatalf("expected 2 usable targets, got %d: %+v", len(targets), targets)
	}
	// Order is preserved, and the trailing slash is normalized away so the
	// entry matches the target label regardless of how it was written.
	if targets[0].target != "https://www.artisanhosting.net" || targets[0].name != "Website" {
		t.Errorf("unexpected first target: %+v", targets[0])
	}
	if targets[1].name != "Office" {
		t.Errorf("unexpected second target: %+v", targets[1])
	}
}

func TestNormalizeStatusTargetMatchesAcrossTrailingSlashes(t *testing.T) {
	// The scrape config mixes both spellings; they must collapse to one key.
	withSlash := normalizeStatusTarget("https://www.artisanhosting.net/")
	without := normalizeStatusTarget("  https://www.artisanhosting.net  ")
	if withSlash != without {
		t.Fatalf("expected %q and %q to normalize alike", withSlash, without)
	}
}

func TestStatusConfigFromEnvDisabledWithoutPrometheusURL(t *testing.T) {
	t.Setenv("PROMETHEUS_URL", "")
	t.Setenv("STATUS_SERVICES", "https://www.artisanhosting.net|Website")
	if cfg := statusConfigFromEnv(); cfg.enabled {
		t.Fatal("expected status to be disabled without PROMETHEUS_URL")
	}
}

func TestStatusConfigFromEnvDisabledWithoutServices(t *testing.T) {
	t.Setenv("PROMETHEUS_URL", "http://prometheus.test")
	t.Setenv("STATUS_SERVICES", "")
	if cfg := statusConfigFromEnv(); cfg.enabled {
		t.Fatal("expected status to be disabled with an empty allowlist")
	}
}

func TestStatusConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("PROMETHEUS_URL", "http://prometheus.test")
	t.Setenv("STATUS_SERVICES", "https://www.artisanhosting.net|Website")
	t.Setenv("STATUS_SELECTOR", "")
	t.Setenv("STATUS_DEGRADED_MS", "")
	t.Setenv("STATUS_CACHE_TTL", "")

	cfg := statusConfigFromEnv()
	if !cfg.enabled {
		t.Fatal("expected status to be enabled")
	}
	if cfg.selector != defaultStatusSelector {
		t.Errorf("selector = %q, want the default", cfg.selector)
	}
	if cfg.degradedMS != 0 {
		t.Errorf("degradedMS = %v, want 0 (latency threshold off by default)", cfg.degradedMS)
	}
	if cfg.cacheTTL != defaultStatusCacheTTL {
		t.Errorf("cacheTTL = %v, want %v", cfg.cacheTTL, defaultStatusCacheTTL)
	}
}

func TestStatusSelectorExcludesExporterInternals(t *testing.T) {
	// job="blackbox_exporter" and job="prometheus" scrape exporter internals,
	// not probe results, and must not be swept into the status page.
	query := statusCurrentQuery(defaultStatusSelector)
	if !strings.Contains(query, `job=~"blackbox_.+_probe_[ab]"`) {
		t.Fatalf("current query lost its job selector: %s", query)
	}
	for _, excluded := range []string{`job="blackbox_exporter"`, `job="prometheus"`} {
		if strings.Contains(query, excluded) {
			t.Errorf("query should not reference %s", excluded)
		}
	}
	// Every aggregate must group by target, never by instance -- instance is
	// the blackbox exporter's address, not the probed site.
	if strings.Contains(query, "by (instance)") {
		t.Error("aggregates must group by (target), not by (instance)")
	}
}

func TestBuildStatusSnapshotDerivesStateFromProbeAgreement(t *testing.T) {
	cfg := testStatusConfig()
	cfg.targets = append(cfg.targets, statusTarget{target: "https://office.artisanhosting.net", name: "Office"})

	current := currentFor("https://www.artisanhosting.net/", 2, 2, 0.084, 200)
	current = append(current, currentFor("https://cloud.artisanhosting.net/status.php", 1, 2, 0.210, 200)...)
	current = append(current, currentFor("https://office.artisanhosting.net", 0, 2, 0, 0)...)

	snapshot, _ := buildStatusSnapshot(cfg, current, nil, nil, fixedNow.AddDate(0, 0, -29), fixedNow)

	if got := serviceByName(t, snapshot, "Website").State; got != statusOK {
		t.Errorf("both probes up: state = %q, want %q", got, statusOK)
	}
	if got := serviceByName(t, snapshot, "Cloud Storage").State; got != statusWarn {
		t.Errorf("one probe up: state = %q, want %q", got, statusWarn)
	}
	if got := serviceByName(t, snapshot, "Office").State; got != statusBad {
		t.Errorf("no probes up: state = %q, want %q", got, statusBad)
	}
	if snapshot.Overall != statusBad {
		t.Errorf("overall = %q, want %q (worst wins)", snapshot.Overall, statusBad)
	}

	website := serviceByName(t, snapshot, "Website")
	if website.LatencyMS != 84 {
		t.Errorf("latency = %d ms, want 84", website.LatencyMS)
	}
	if website.HTTPStatus != 200 {
		t.Errorf("http status = %d, want 200", website.HTTPStatus)
	}
	if website.LastProbe == nil || !website.LastProbe.Equal(fixedNow.Add(-15*time.Second)) {
		t.Errorf("last probe = %v, want the scrape timestamp", website.LastProbe)
	}
}

func TestBuildStatusSnapshotMatchesAcrossTrailingSlashes(t *testing.T) {
	cfg := testStatusConfig()
	// Allowlist written without a slash, Prometheus reports one (as the real
	// blackbox_urls.yml does). This is the likeliest cause of an empty grid.
	snapshot, diags := buildStatusSnapshot(cfg, currentFor("https://www.artisanhosting.net/", 2, 2, 0.1, 200), nil, nil, fixedNow, fixedNow)

	if len(snapshot.Services) != 1 {
		t.Fatalf("expected the target to match despite the trailing slash, got %+v", snapshot.Services)
	}
	if len(diags.unmatchedTargets) != 0 {
		t.Errorf("expected no unmatched targets, got %v", diags.unmatchedTargets)
	}
}

func TestBuildStatusSnapshotExcludesTargetsOutsideTheAllowlist(t *testing.T) {
	cfg := testStatusConfig()
	current := currentFor("https://www.artisanhosting.net", 2, 2, 0.1, 200)
	// Prometheus probes plenty of things the public page must never name.
	current = append(current, currentFor("https://temp1.artisanstudio.net", 2, 2, 0.1, 200)...)
	current = append(current, currentFor("https://dywnotary.com/", 2, 2, 0.1, 200)...)

	snapshot, diags := buildStatusSnapshot(cfg, current, nil, nil, fixedNow, fixedNow)

	if len(snapshot.Services) != 1 || snapshot.Services[0].Name != "Website" {
		t.Fatalf("only allowlisted targets may be published, got %+v", snapshot.Services)
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leaked := range []string{"temp1.artisanstudio.net", "dywnotary.com", "artisanhosting.net", "instance", "job"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("payload leaks %q: %s", leaked, body)
		}
	}
	if len(diags.unmatchedTargets) != 2 {
		t.Errorf("expected both unlisted targets reported as drift, got %v", diags.unmatchedTargets)
	}
}

func TestBuildStatusSnapshotDropsAllowlistedTargetWithNoData(t *testing.T) {
	cfg := testStatusConfig()
	// Only one of the two configured targets reports. A typo'd or retired
	// entry must vanish rather than be published as a false outage.
	snapshot, diags := buildStatusSnapshot(cfg, currentFor("https://www.artisanhosting.net", 2, 2, 0.1, 200), nil, nil, fixedNow, fixedNow)

	if len(snapshot.Services) != 1 {
		t.Fatalf("expected the dataless target to be dropped, got %+v", snapshot.Services)
	}
	if snapshot.Overall != statusOK {
		t.Errorf("overall = %q; a missing target must not read as an outage", snapshot.Overall)
	}
	if len(diags.missingServices) != 1 || diags.missingServices[0] != "https://cloud.artisanhosting.net/status.php" {
		t.Errorf("expected the missing target reported for the operator, got %v", diags.missingServices)
	}
}

func TestBuildStatusSnapshotLatencyThresholdDegrades(t *testing.T) {
	cfg := testStatusConfig()
	cfg.degradedMS = 100
	cfg.targets = cfg.targets[:1]

	snapshot, _ := buildStatusSnapshot(cfg, currentFor("https://www.artisanhosting.net", 2, 2, 0.250, 200), nil, nil, fixedNow, fixedNow)
	if got := snapshot.Services[0].State; got != statusWarn {
		t.Errorf("state = %q, want %q when both probes are up but slow", got, statusWarn)
	}
}

func TestBuildStatusSnapshotCollectsUptimeAndHistory(t *testing.T) {
	cfg := testStatusConfig()
	cfg.targets = cfg.targets[:1]
	historyStart := fixedNow.AddDate(0, 0, -(statusHistoryDays - 1))

	uptime := []promSeries{
		instantSeries(map[string]string{"target": "https://www.artisanhosting.net", "window": "24h"}, 1),
		instantSeries(map[string]string{"target": "https://www.artisanhosting.net", "window": "7d"}, 0.99981234),
		instantSeries(map[string]string{"target": "https://www.artisanhosting.net", "window": "30d"}, 0.9994),
	}
	history := []promSeries{{
		Metric: map[string]string{"target": "https://www.artisanhosting.net", "agg": "availability"},
		Values: []promSample{
			{At: historyStart, Value: 1},
			{At: historyStart.AddDate(0, 0, 3), Value: 0.5},
			{At: fixedNow, Value: 0},
		},
	}}

	snapshot, _ := buildStatusSnapshot(cfg, currentFor("https://www.artisanhosting.net", 2, 2, 0.1, 200), uptime, history, historyStart, fixedNow)
	service := snapshot.Services[0]

	if service.Uptime["24h"] != 1 {
		t.Errorf("24h uptime = %v, want 1", service.Uptime["24h"])
	}
	if service.Uptime["7d"] != 0.9998 {
		t.Errorf("7d uptime = %v, want it rounded to basis points", service.Uptime["7d"])
	}
	if len(service.History) != statusHistoryDays {
		t.Fatalf("history has %d buckets, want %d", len(service.History), statusHistoryDays)
	}
	if service.History[0] == nil || service.History[0].Availability != 1 {
		t.Errorf("first bucket = %v, want 1", service.History[0])
	}
	if service.History[3] == nil || service.History[3].Availability != 0.5 {
		t.Errorf("fourth bucket = %v, want 0.5", service.History[3])
	}
	if last := service.History[statusHistoryDays-1]; last == nil || last.Availability != 0 {
		t.Errorf("last bucket = %v, want 0", last)
	}
	// Buckets Prometheus had no data for stay null so the page can render
	// them as "no data" rather than as an outage.
	if service.History[1] != nil {
		t.Errorf("gap bucket = %v, want null", service.History[1])
	}
}

// stubQuery returns a promQueryFunc serving canned results per API path, and a
// counter of how many calls it received.
func stubQuery(current, uptime, history []promSeries, err error) (promQueryFunc, *int) {
	var mu sync.Mutex
	calls := 0
	return func(_ context.Context, path string, params url.Values) ([]promSeries, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if err != nil {
			return nil, err
		}
		if path == "/api/v1/query_range" {
			return history, nil
		}
		if strings.Contains(params.Get("query"), "avg_over_time") {
			return uptime, nil
		}
		return current, nil
	}, &calls
}

func TestStatusHandlerDisabledReportsUnavailable(t *testing.T) {
	cfg := statusConfig{}
	query, calls := stubQuery(nil, nil, nil, nil)
	handler := statusHandlerWithDependencies(cfg, query, newStatusCache(), func() time.Time { return fixedNow })

	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	var body statusSnapshot
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Enabled {
		t.Error("expected enabled=false")
	}
	if *calls != 0 {
		t.Errorf("a disabled endpoint must not query Prometheus, got %d calls", *calls)
	}
}

func TestStatusHandlerRejectsNonGET(t *testing.T) {
	handler := statusHandlerWithDependencies(testStatusConfig(), func(context.Context, string, url.Values) ([]promSeries, error) {
		t.Fatal("must not query Prometheus for a rejected method")
		return nil, nil
	}, newStatusCache(), func() time.Time { return fixedNow })

	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodPost, "/api/status", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}

func TestStatusHandlerCachesWithinTTL(t *testing.T) {
	cfg := testStatusConfig()
	cfg.targets = cfg.targets[:1]
	query, calls := stubQuery(currentFor("https://www.artisanhosting.net", 2, 2, 0.1, 200), nil, nil, nil)

	now := fixedNow
	cache := newStatusCache()
	handler := statusHandlerWithDependencies(cfg, query, cache, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, recorder.Code)
		}
	}
	// Three queries make up one refresh; the two later requests are cache hits.
	if *calls != 3 {
		t.Errorf("expected one refresh (3 queries) inside the TTL, got %d", *calls)
	}

	now = now.Add(cfg.cacheTTL + time.Second)
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if *calls != 6 {
		t.Errorf("expected a second refresh past the TTL, got %d queries", *calls)
	}
}

func TestStatusHandlerServesStaleSnapshotWhenPrometheusFails(t *testing.T) {
	cfg := testStatusConfig()
	cfg.targets = cfg.targets[:1]
	current := currentFor("https://www.artisanhosting.net", 2, 2, 0.1, 200)

	var failing bool
	var mu sync.Mutex
	query := func(_ context.Context, path string, params url.Values) ([]promSeries, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return nil, fmt.Errorf("connection refused")
		}
		if path == "/api/v1/query_range" || strings.Contains(params.Get("query"), "avg_over_time") {
			return nil, nil
		}
		return current, nil
	}

	now := fixedNow
	cache := newStatusCache()
	handler := statusHandlerWithDependencies(cfg, query, cache, func() time.Time { return now })

	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("priming request failed: %d", recorder.Code)
	}

	mu.Lock()
	failing = true
	mu.Unlock()
	now = now.Add(cfg.cacheTTL + time.Second)

	recorder = httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want the last good snapshot to still be served", recorder.Code)
	}
	var body statusSnapshot
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Stale {
		t.Error("expected stale=true so the page can say the data is aging")
	}
	if len(body.Services) != 1 {
		t.Errorf("expected the cached service to survive, got %+v", body.Services)
	}
}

func TestStatusHandlerFailsWhenPrometheusUnreachableAndCacheEmpty(t *testing.T) {
	cfg := testStatusConfig()
	query, _ := stubQuery(nil, nil, nil, fmt.Errorf("connection refused"))
	handler := statusHandlerWithDependencies(cfg, query, newStatusCache(), func() time.Time { return fixedNow })

	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
}

func TestPromSampleUnmarshalsPrometheusEncoding(t *testing.T) {
	var sample promSample
	if err := json.Unmarshal([]byte(`[1756123200.5,"0.084"]`), &sample); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sample.Value != 0.084 {
		t.Errorf("value = %v, want 0.084", sample.Value)
	}
	if !sample.At.Equal(time.Unix(1756123200, 500000000).UTC()) {
		t.Errorf("timestamp = %v", sample.At)
	}
	if !sample.ok() {
		t.Error("expected a finite sample to be usable")
	}

	// Prometheus reports absent aggregations as NaN; those must not be
	// mistaken for a zero (which would read as a total outage).
	var missing promSample
	if err := json.Unmarshal([]byte(`[1756123200,"NaN"]`), &missing); err != nil {
		t.Fatalf("unmarshal NaN: %v", err)
	}
	if missing.ok() {
		t.Error("NaN must not be treated as usable data")
	}
}

func TestNewPromQueryDecodesAndReportsErrors(t *testing.T) {
	var gotPath, gotQuery, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotPath, gotQuery, gotAuth = r.URL.Path, r.Form.Get("query"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"target":"https://example.test","agg":"up"},"value":[1756123200,"2"]}]}}`)
	}))
	defer server.Close()

	query := newPromQuery(server.Client(), server.URL, "s3cret")
	series, err := query(context.Background(), "/api/v1/query", url.Values{"query": {"probe_success"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if gotPath != "/api/v1/query" || gotQuery != "probe_success" {
		t.Errorf("unexpected request: path=%q query=%q", gotPath, gotQuery)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("authorization = %q, want the bearer token forwarded", gotAuth)
	}
	if len(series) != 1 || series[0].Value.Value != 2 {
		t.Fatalf("unexpected series: %+v", series)
	}

	// A PromQL error arrives as HTTP 400 with an error envelope.
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"status":"error","errorType":"bad_data","error":"parse error"}`)
	}))
	defer failing.Close()

	if _, err := newPromQuery(failing.Client(), failing.URL, "")(context.Background(), "/api/v1/query", url.Values{}); err == nil {
		t.Fatal("expected an error from a failed query")
	} else if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("error = %v, want it to carry Prometheus' message", err)
	}
}

// historyPoint builds one tagged history series for a single day bucket.
func historyPoint(target, agg string, at time.Time, value float64) promSeries {
	return promSeries{
		Metric: map[string]string{"target": target, "agg": agg},
		Values: []promSample{{At: at, Value: value}},
	}
}

func TestBuildStatusSnapshotExplainsBadDays(t *testing.T) {
	cfg := testStatusConfig()
	cfg.targets = cfg.targets[:1]
	target := "https://www.artisanhosting.net"
	start := fixedNow.AddDate(0, 0, -(statusHistoryDays - 1))
	badDay := start.AddDate(0, 0, 5)
	cleanDay := start

	history := []promSeries{
		historyPoint(target, "availability", cleanDay, 1),
		historyPoint(target, "availability", badDay, 0.9722),
		// One of two monitoring locations had a bad day.
		{Metric: map[string]string{"target": target, "agg": "location", "probe": "probe_a"},
			Values: []promSample{{At: badDay, Value: 1}}},
		{Metric: map[string]string{"target": target, "agg": "location", "probe": "probe_b"},
			Values: []promSample{{At: badDay, Value: 0.94}}},
		historyPoint(target, "http_min", badDay, 200),
		historyPoint(target, "http_max", badDay, 502),
	}

	snapshot, _ := buildStatusSnapshot(cfg, currentFor(target, 2, 2, 0.1, 200), nil, history, start, fixedNow)
	days := snapshot.Services[0].History

	clean := days[0]
	if clean == nil || clean.Availability != 1 {
		t.Fatalf("clean day = %+v, want availability 1", clean)
	}
	// A clean day has no failure to describe, so it carries nothing else.
	if clean.LocationsTotal != 0 || clean.HTTPStatus != 0 || clean.NoResponse || clean.ContentFailed {
		t.Errorf("clean day should carry no failure detail, got %+v", clean)
	}

	bad := days[5]
	if bad == nil {
		t.Fatal("expected the bad day to be published")
	}
	if bad.Availability != 0.9722 {
		t.Errorf("availability = %v, want 0.9722", bad.Availability)
	}
	if bad.LocationsAffected != 1 || bad.LocationsTotal != 2 {
		t.Errorf("locations = %d/%d, want 1/2", bad.LocationsAffected, bad.LocationsTotal)
	}
	if bad.HTTPStatus != 502 {
		t.Errorf("http status = %d, want 502", bad.HTTPStatus)
	}
	if bad.NoResponse {
		t.Error("a day whose probes all got an HTTP response must not report no_response")
	}
}

func TestBuildStatusSnapshotDistinguishesNoResponseFromBadStatus(t *testing.T) {
	cfg := testStatusConfig()
	cfg.targets = cfg.targets[:1]
	target := "https://www.artisanhosting.net"
	start := fixedNow.AddDate(0, 0, -(statusHistoryDays - 1))

	// blackbox reports status code 0 when a probe never got an HTTP response
	// at all -- DNS, TCP or TLS -- which is a different story from a 5xx.
	history := []promSeries{
		historyPoint(target, "availability", start, 0.4),
		historyPoint(target, "http_min", start, 0),
		historyPoint(target, "http_max", start, 200),
		historyPoint(target, "regex", start, 1),
	}

	snapshot, _ := buildStatusSnapshot(cfg, currentFor(target, 2, 2, 0.1, 200), nil, history, start, fixedNow)
	day := snapshot.Services[0].History[0]

	if day == nil || !day.NoResponse {
		t.Fatalf("expected no_response for a day containing a status code of 0, got %+v", day)
	}
	// 200 is not a failure code, so it must not be surfaced as the cause.
	if day.HTTPStatus != 0 {
		t.Errorf("http status = %d; only error codes should be reported", day.HTTPStatus)
	}
	if !day.ContentFailed {
		t.Error("expected the regex failure to be reported")
	}
}

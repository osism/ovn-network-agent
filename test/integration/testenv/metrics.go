//go:build integration

package testenv

import (
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// MetricsSnapshot is a parsed Prometheus text-format /metrics scrape.
//
// Outer key: metric name (e.g. "ovn_network_agent_reconcile_total").
// Inner key: a canonical label string built by LabelKey from the
// label set (e.g. `trigger="periodic"`). Empty inner key represents
// a metric with no labels.
//
// Histograms expand to several derived series the way Prometheus
// exposes them: <name>_bucket{le="…"}, <name>_sum, <name>_count.
type MetricsSnapshot map[string]map[string]float64

// Value returns the value of metric `name` with the given label set, or
// (0, false) if the series is not present. Labels are normalised through
// LabelKey, so callers may pass them in any order.
func (s MetricsSnapshot) Value(name string, labels map[string]string) (float64, bool) {
	series, ok := s[name]
	if !ok {
		return 0, false
	}
	v, ok := series[LabelKey(labels)]
	return v, ok
}

// LabelKey returns the canonical inner-map key for a label set: each
// `name="value"` pair joined by `,` after sorting by name. Empty labels
// yield "". The escape rules match Prometheus text exposition: `\` →
// `\\`, `"` → `\"`, newline → `\n`.
func LabelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(labels[n]))
		b.WriteByte('"')
	}
	return b.String()
}

// ScrapeMetrics fetches http://addr/metrics and parses the response into a
// MetricsSnapshot. It fails the test on transport or parse errors. Callers
// must have configured the agent with cfg.MetricsListen=addr.
func ScrapeMetrics(t *testing.T, addr string) MetricsSnapshot {
	t.Helper()
	url := "http://" + addr + "/metrics"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("ScrapeMetrics: build request: %v", err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("ScrapeMetrics %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ScrapeMetrics %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ScrapeMetrics %s: read body: %v", url, err)
	}
	snap, err := parseMetrics(string(body))
	if err != nil {
		t.Fatalf("ScrapeMetrics %s: parse: %v", url, err)
	}
	return snap
}

// AssertMetricEventually polls /metrics until predicate returns true for the
// (name, labels) series or timeout expires. Useful for asserting that a
// counter has incremented or a gauge has settled to an expected value
// without baking in a fixed sleep. Scrape errors short-circuit the
// test — the metrics endpoint must stay reachable for the duration.
func AssertMetricEventually(
	t *testing.T,
	addr, name string,
	labels map[string]string,
	predicate func(value float64, present bool) bool,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastVal float64
	var lastPresent bool
	for {
		snap := ScrapeMetrics(t, addr)
		lastVal, lastPresent = snap.Value(name, labels)
		if predicate(lastVal, lastPresent) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metric %s%s did not satisfy predicate within %s "+
				"(last value=%v present=%v)",
				name, formatLabelsForError(labels), timeout, lastVal, lastPresent)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// AssertHTTPStatusEventually polls GET http://addr<path> until the response
// status equals want or timeout expires. Useful for asserting that a readiness
// endpoint has flipped (e.g. /readyz 200 <-> 503) without baking in a fixed
// sleep. Transport errors are tolerated across the deadline — the listener may
// briefly refuse connections around startup — and surface only if the timeout
// is reached. On timeout it fails with the last observed status and body.
func AssertHTTPStatusEventually(
	t *testing.T,
	addr, path string,
	want int,
	timeout time.Duration,
) {
	t.Helper()
	url := "http://" + addr + path
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(timeout)
	var lastStatus int
	var lastBody string
	for {
		resp, err := client.Get(url)
		if err != nil {
			lastStatus = 0
			lastBody = err.Error()
		} else {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			lastStatus = resp.StatusCode
			lastBody = string(body)
			if lastStatus == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s did not return status %d within %s "+
				"(last status=%d, body:\n%s)",
				url, want, timeout, lastStatus, lastBody)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// FreeLoopbackAddr returns a 127.0.0.1:<port> address whose port is currently
// free. The listener is opened and closed before returning, leaving a small
// TOCTOU window — the agent re-listens immediately on startup, so collisions
// are vanishingly rare in practice. Tests should pass this string into
// cfg.MetricsListen.
func FreeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("FreeLoopbackAddr: listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("FreeLoopbackAddr: close: %v", err)
	}
	return addr
}

// parseMetrics walks Prometheus text exposition output line by line.
// Comments (# HELP / # TYPE) and blank lines are skipped. Every other line
// is `name{labels} value [timestamp]` or `name value [timestamp]`. The
// timestamp is discarded.
//
// This is intentionally a hand-rolled parser rather than pulling in
// prometheus/common/expfmt — the format is line-oriented, the parser is
// short, and we avoid promoting an indirect dep to a direct one for the
// test harness alone.
func parseMetrics(text string) (MetricsSnapshot, error) {
	out := MetricsSnapshot{}
	for ln, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, err := parseMetricLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln+1, err)
		}
		series, ok := out[name]
		if !ok {
			series = map[string]float64{}
			out[name] = series
		}
		series[labels] = value
	}
	return out, nil
}

// parseMetricLine extracts (name, label-key, value) from a single
// exposition-format line. Returns the canonical label key (sorted,
// `k="v",…`) so it can be looked up by callers that build the same
// canonical key from a map.
func parseMetricLine(line string) (string, string, float64, error) {
	// Name: everything up to the first `{` or whitespace.
	nameEnd := strings.IndexAny(line, "{ \t")
	if nameEnd < 0 {
		return "", "", 0, fmt.Errorf("no value: %q", line)
	}
	name := line[:nameEnd]
	rest := line[nameEnd:]

	labelKey := ""
	if strings.HasPrefix(rest, "{") {
		// Find the closing `}` that isn't inside a quoted label value.
		// Prometheus text format escapes `\\`, `\"`, `\n` inside values
		// but allows other characters (including `}`) verbatim, so a
		// naive strings.Index would mis-terminate on `metric{l="a}b"} 1`.
		close := -1
		inQuote := false
		for i := 1; i < len(rest); i++ {
			c := rest[i]
			if inQuote {
				if c == '\\' && i+1 < len(rest) {
					i++
					continue
				}
				if c == '"' {
					inQuote = false
				}
				continue
			}
			if c == '"' {
				inQuote = true
				continue
			}
			if c == '}' {
				close = i
				break
			}
		}
		if close < 0 {
			return "", "", 0, fmt.Errorf("unterminated label block: %q", line)
		}
		labels, err := parseLabels(rest[1:close])
		if err != nil {
			return "", "", 0, fmt.Errorf("labels: %w", err)
		}
		labelKey = LabelKey(labels)
		rest = rest[close+1:]
	}

	rest = strings.TrimSpace(rest)
	// Value is the first whitespace-separated token after the label block;
	// any trailing token is the optional timestamp, which we discard.
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", "", 0, fmt.Errorf("missing value: %q", line)
	}
	v, err := parseFloat(fields[0])
	if err != nil {
		return "", "", 0, fmt.Errorf("value %q: %w", fields[0], err)
	}
	return name, labelKey, v, nil
}

// parseLabels turns the inside of a `{…}` block into a label map. Handles
// the standard escape sequences (`\\`, `\"`, `\n`). Whitespace between
// pairs is tolerated.
func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		// Skip whitespace and commas between pairs.
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			break
		}
		// Name: up to '='.
		eq := strings.IndexByte(s[i:], '=')
		if eq < 0 {
			return nil, fmt.Errorf("no '=' after %q", s[i:])
		}
		name := strings.TrimSpace(s[i : i+eq])
		i += eq + 1
		if i >= len(s) || s[i] != '"' {
			return nil, fmt.Errorf("expected '\"' after %s=", name)
		}
		i++
		var val strings.Builder
		for i < len(s) {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				switch s[i+1] {
				case '\\':
					val.WriteByte('\\')
				case '"':
					val.WriteByte('"')
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				i++
				break
			}
			val.WriteByte(c)
			i++
		}
		out[name] = val.String()
	}
	return out, nil
}

// parseFloat accepts Prometheus's special float tokens (+Inf, -Inf, NaN)
// alongside ordinary numeric values.
func parseFloat(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

func escapeLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

func formatLabelsForError(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	return "{" + LabelKey(labels) + "}"
}

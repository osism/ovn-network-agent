package main

import (
	"fmt"
	"regexp"
	"strings"
)

// renderConfiguration emits docs/reference/configuration.md: a banner
// plus one row per CLI flag with the corresponding env var, YAML key,
// default, and description, followed by sub-tables for the YAML-only
// PortForwardRule and PortForwardVIP structs.
func renderConfiguration(info *sourceInfo) string {
	var b strings.Builder
	writeBanner(&b)
	b.WriteString("# Configuration reference\n\n")
	b.WriteString("Settings are loaded with the following priority (highest wins):\n\n")
	b.WriteString("**CLI flags > environment variables > config file > defaults**\n\n")
	b.WriteString("For task-oriented setup notes (where the config file lives, how to override\n")
	b.WriteString("via env vars, the example config), see\n")
	b.WriteString("[Configure the agent](../guides/configuration).\n\n")
	b.WriteString("Every row below is derived from the corresponding Go declaration in\n")
	b.WriteString("[`config.go`](https://github.com/osism/ovn-network-agent/blob/main/config.go);\n")
	b.WriteString("regenerate this page with `go generate ./...`.\n\n")

	b.WriteString("## Flags, environment variables and YAML keys\n\n")
	b.WriteString("| Flag | Env Var | Config key | Default | Description |\n")
	b.WriteString("|------|---------|------------|---------|-------------|\n")
	for _, fl := range info.Flags {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
			mdCode("--"+fl.Name),
			renderEnv(fl, info),
			renderYAML(fl, info),
			renderDefault(resolveDefault(fl, info)),
			mdRow(fl.Usage),
		)
	}

	// YAML-only Config fields: Config has a yaml binding (via
	// configFile + applyFileConfig) but no CLI flag (currently:
	// port_forwards).
	if extra := extraYAMLOnlyRows(info); len(extra) > 0 {
		b.WriteString("\nAdditional YAML-only keys:\n\n")
		b.WriteString("| Config key | Type | Description |\n")
		b.WriteString("|------------|------|-------------|\n")
		for _, row := range extra {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", mdCode(row.key), mdCode(row.typ), mdRow(row.desc))
		}
	}

	// PortForwardVIP and PortForwardRule cover the YAML-only nested
	// structures driven by port_forwards.
	for _, name := range []string{"PortForwardVIP", "PortForwardRule"} {
		st, ok := info.Structs[name]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "\n## `%s` (YAML-only)\n\n", name)
		b.WriteString("Nested YAML structure used inside `port_forwards`.\n\n")
		b.WriteString("| YAML key | Type | Description |\n")
		b.WriteString("|----------|------|-------------|\n")
		for _, f := range st.Fields {
			if f.YAMLTag == "" {
				continue
			}
			desc := f.Comment
			if desc == "" {
				desc = "—"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", mdCode(f.YAMLTag), mdCode(f.Type), mdRow(desc))
		}
	}

	return b.String()
}

// renderMetricLabels renders the Labels cell. For a Vec collector we
// show each label followed by its bootstrap value set (when known)
// so the table is as informative as the previously hand-written one.
func renderMetricLabels(m metricInfo) string {
	if len(m.Labels) == 0 {
		return "—"
	}
	parts := make([]string, len(m.Labels))
	for i, label := range m.Labels {
		values := m.LabelValues[label]
		if len(values) == 0 {
			parts[i] = mdCode(label)
			continue
		}
		vals := make([]string, len(values))
		for j, v := range values {
			vals[j] = mdCode(v)
		}
		parts[i] = mdCode(label) + "={" + strings.Join(vals, ",") + "}"
	}
	return strings.Join(parts, ", ")
}

func renderEnv(fl flagInfo, info *sourceInfo) string {
	if fl.ConfigField != "" {
		if v, ok := info.EnvByField[fl.ConfigField]; ok {
			return mdCode(v)
		}
	}
	if fl.ImplicitEnv != "" {
		return mdCode(fl.ImplicitEnv)
	}
	return "—"
}

func renderYAML(fl flagInfo, info *sourceInfo) string {
	if fl.ConfigField == "" {
		return "—"
	}
	if v, ok := info.YAMLByField[fl.ConfigField]; ok {
		return mdCode(v)
	}
	return "—"
}

// renderCLI emits docs/reference/cli.md: a banner, a brief intro, and
// one row per CLI flag with its default and usage string.
func renderCLI(info *sourceInfo) string {
	var b strings.Builder
	writeBanner(&b)
	b.WriteString("# Command-line flags\n\n")
	b.WriteString("Every command-line flag accepted by `ovn-network-agent`. Most flags also\n")
	b.WriteString("have an environment variable and YAML equivalent — see the\n")
	b.WriteString("[configuration reference](configuration) for the cross-reference table.\n\n")
	b.WriteString("This page is generated from the `flag.FlagSet` declared in\n")
	b.WriteString("[`config.go`](https://github.com/osism/ovn-network-agent/blob/main/config.go);\n")
	b.WriteString("regenerate it with `go generate ./...`.\n\n")
	b.WriteString("| Flag | Default | Description |\n")
	b.WriteString("|------|---------|-------------|\n")
	for _, fl := range info.Flags {
		fmt.Fprintf(&b, "| %s | %s | %s |\n",
			mdCode("--"+fl.Name),
			renderDefault(resolveDefault(fl, info)),
			mdRow(fl.Usage),
		)
	}
	return b.String()
}

// renderMetrics emits docs/reference/metrics.md: a banner, the
// hand-written intro and alert suggestions, and the auto-generated
// metrics table sourced from metrics.go.
func renderMetrics(info *sourceInfo) string {
	var b strings.Builder
	writeBanner(&b)
	b.WriteString("# Metrics\n\n")
	b.WriteString("The agent can expose Prometheus-formatted metrics on an optional HTTP\n")
	b.WriteString("endpoint. Enable it by setting `--metrics-listen` (or\n")
	b.WriteString("`OVN_NETWORK_METRICS_LISTEN` / `metrics_listen`) to a `host:port` such as\n")
	b.WriteString("`127.0.0.1:9273`. The endpoint is **off by default**; bind to `127.0.0.1` for\n")
	b.WriteString("node-local scraping, or to `0.0.0.0` for a remote scraper.\n\n")
	b.WriteString("```bash\n")
	b.WriteString("ovn-network-agent --metrics-listen 127.0.0.1:9273\n")
	b.WriteString("curl -s http://127.0.0.1:9273/metrics\n")
	b.WriteString("```\n\n")
	b.WriteString("Three paths are served:\n\n")
	b.WriteString("- `/metrics` — Prometheus exposition format\n")
	b.WriteString("- `/healthz` — always returns `200 ok` while the process is up (liveness only)\n")
	b.WriteString("- `/readyz` — returns `200` only when the agent is functional (OVN connected, last reconcile succeeded); `503` otherwise\n\n")
	if info.Namespace != "" {
		fmt.Fprintf(&b, "All metrics are prefixed with `%s_`. The table below is generated from the\n", info.Namespace)
	} else {
		b.WriteString("The table below is generated from the\n")
	}
	b.WriteString("`prometheus.New*` constructors in\n")
	b.WriteString("[`metrics.go`](https://github.com/osism/ovn-network-agent/blob/main/metrics.go);\n")
	b.WriteString("regenerate it with `go generate ./...`.\n\n")

	b.WriteString("| Metric | Type | Labels | Description |\n")
	b.WriteString("|--------|------|--------|-------------|\n")
	for _, m := range info.Metrics {
		typ := m.Kind
		if m.IsVec {
			typ = m.Kind + " (vec)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n",
			mdCode(m.FullName),
			typ,
			renderMetricLabels(m),
			mdRow(m.Help),
		)
	}

	b.WriteString("\n## Suggested alerts\n\n")
	b.WriteString("Ready-to-load rules implementing these alerts ship in\n")
	b.WriteString("[`contrib/prometheus-rules.yaml`](https://github.com/osism/ovn-network-agent/blob/main/contrib/prometheus-rules.yaml),\n")
	b.WriteString("and each alert has a cause→diagnosis→remediation section in the\n")
	b.WriteString("[troubleshooting guide](../guides/troubleshooting).\n\n")
	// Keep these bullets in sync with contrib/prometheus-rules.yaml.
	b.WriteString("- `consecutive_readds >= 3` — persistent route instability (FRR or kernel\n")
	b.WriteString("  races).\n")
	b.WriteString("- `ovn_connection_state{database=\"nb\"} == 0` for >2m — NB DB unreachable;\n")
	b.WriteString("  agent cannot write OVN state.\n")
	b.WriteString("- `rate(route_readds_total[10m]) > 0` — flapping routes.\n")
	b.WriteString("- `inactive_routes > 0` for >2m — FIP `/32`s configured in FRR but not\n")
	b.WriteString("  advertised via BGP (e.g. an unresolvable next-hop); those FIPs are\n")
	b.WriteString("  unreachable from outside.\n")
	b.WriteString("- `hairpin_flows_installed < hairpin_flows_desired` for >1m — same-chassis\n")
	b.WriteString("  hairpin flows are missing, so those FIPs are unreachable from workloads on\n")
	b.WriteString("  the same chassis while remote clients still reach them.\n")
	b.WriteString("- `rate(nexthop_repairs_total[1h]) > 0` — zebra was missing the connected\n")
	b.WriteString("  route for the veth next-hop, so no FIP route could resolve. The agent\n")
	b.WriteString("  repaired it; the alert says it happened.\n")
	b.WriteString("- `histogram_quantile(0.95, rate(failover_announce_seconds_bucket[1h])) > 2`\n")
	b.WriteString("  — failover announces slower than the ~2s failover budget.\n")
	b.WriteString("- `histogram_quantile(0.95, rate(reconcile_duration_seconds_bucket[5m])) > 5`\n")
	b.WriteString("  — slow reconciles.\n")

	return b.String()
}

// resolveDefault picks the effective default for a flag: the Config
// composite-literal value (highest signal) wins over the literal
// default supplied to fs.<Kind>(...).
func resolveDefault(fl flagInfo, info *sourceInfo) string {
	if fl.ConfigField != "" {
		if v, ok := info.DefaultByField[fl.ConfigField]; ok && v != "" {
			return v
		}
	}
	return fl.Default
}

// renderDefault formats a Go literal as a Markdown cell. We
// special-case `time.Duration` arithmetic and bare string literals
// because those are the dominant shapes in this codebase; the rest
// is passed through verbatim.
func renderDefault(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == `""` {
		return ""
	}
	if dur, ok := durationLiteral(raw); ok {
		return mdCode(dur)
	}
	// String literals: drop surrounding quotes if balanced.
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return mdCode(raw[1 : len(raw)-1])
	}
	return mdCode(raw)
}

var durationPattern = regexp.MustCompile(`^(\d+)\s*\*\s*time\.(Nanosecond|Microsecond|Millisecond|Second|Minute|Hour)$`)

// durationLiteral converts compound Go time expressions like
// `60 * time.Second` into the human-readable form `60s`.
func durationLiteral(raw string) (string, bool) {
	m := durationPattern.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	suffix := map[string]string{
		"Nanosecond":  "ns",
		"Microsecond": "us",
		"Millisecond": "ms",
		"Second":      "s",
		"Minute":      "m",
		"Hour":        "h",
	}[m[2]]
	return m[1] + suffix, true
}

// extraYAMLOnlyRow describes a registry option with a YAML key but no CLI
// flag (currently port_forwards). Its Go type is read off the Config
// field the option binds to.
type extraYAMLOnlyRow struct {
	key  string
	typ  string
	desc string
}

func extraYAMLOnlyRows(info *sourceInfo) []extraYAMLOnlyRow {
	cfgStruct := info.Structs["Config"]
	var rows []extraYAMLOnlyRow
	for _, o := range info.YAMLOnly {
		row := extraYAMLOnlyRow{key: o.Key, desc: o.Desc}
		if cfgStruct != nil {
			for _, f := range cfgStruct.Fields {
				if f.Name == o.ConfigField {
					row.typ = f.Type
					break
				}
			}
		}
		if row.desc == "" {
			row.desc = "See sample config for usage."
		}
		rows = append(rows, row)
	}
	return rows
}

// mdCode wraps a value in backticks for Markdown.
func mdCode(s string) string {
	return "`" + s + "`"
}

// mdRow escapes the few characters that would break a Markdown table
// row.
func mdRow(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

func writeBanner(b *strings.Builder) {
	b.WriteString(banner)
	b.WriteString("\n\n")
}

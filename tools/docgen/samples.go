package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// checkSamples verifies that the two operator-facing config surfaces
// that ship in the .deb — ovn-network-agent.default (env vars) and
// ovn-network-agent.yaml.sample (YAML keys) — cover every option the
// agent actually consumes. It gives those files the same anti-drift
// guarantee docs/reference/ already has via the docs-gen CI job: the
// generator (go run ./tools/docgen) fails when either file misses an
// option, so a new flag/env/YAML key cannot land without the sample
// being updated too.
func checkSamples(root string, info *sourceInfo) error {
	defaultPath := filepath.Join(root, "ovn-network-agent.default")
	samplePath := filepath.Join(root, "ovn-network-agent.yaml.sample")

	defaultData, err := os.ReadFile(defaultPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", defaultPath, err)
	}
	sampleData, err := os.ReadFile(samplePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", samplePath, err)
	}

	var problems []string
	if missing := missingEnvVars(string(defaultData), requiredEnvVars(info)); len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("%s is missing env vars: %s", defaultPath, strings.Join(missing, ", ")))
	}
	if missing := missingYAMLKeys(string(sampleData), requiredYAMLKeys(info)); len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("%s is missing YAML keys: %s", samplePath, strings.Join(missing, ", ")))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// requiredEnvVars is the sorted set of environment variables the agent
// reads: every var bound in applyEnvConfig plus any implicit env var
// used as a flag default (e.g. --config defaults to
// os.Getenv("OVN_NETWORK_CONFIG")).
func requiredEnvVars(info *sourceInfo) []string {
	set := map[string]bool{}
	for _, v := range info.EnvByField {
		set[v] = true
	}
	for _, fl := range info.Flags {
		if fl.ImplicitEnv != "" {
			set[fl.ImplicitEnv] = true
		}
	}
	return sortedKeys(set)
}

// requiredYAMLKeys is the sorted set of YAML keys the agent accepts,
// gathered by walking configFile and following every struct-typed field
// so nested structures (currently PortForwardVIP → PortForwardRule) stay
// covered as they are added. A hardcoded list of struct names would
// silently under-cover a newly-added nested struct — the exact drift
// this tool exists to prevent.
func requiredYAMLKeys(info *sourceInfo) []string {
	set := map[string]bool{}
	seen := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		st := info.Structs[name]
		if st == nil {
			return
		}
		for _, f := range st.Fields {
			if f.YAMLTag != "" {
				set[f.YAMLTag] = true
			}
			visit(baseTypeName(f.Type))
		}
	}
	visit("configFile")
	return sortedKeys(set)
}

// baseTypeName strips pointer, slice, array, and map wrappers from a
// field's source type text to reach the underlying named type (e.g.
// "[]PortForwardVIP" and "*PortForwardRule" both resolve to their bare
// struct name). Scalars like "string" are returned unchanged; callers
// look the result up in info.Structs, where non-struct names simply miss.
func baseTypeName(typ string) string {
	typ = strings.TrimPrefix(typ, "*")
	if i := strings.LastIndex(typ, "]"); i >= 0 {
		typ = typ[i+1:]
	}
	return strings.TrimPrefix(typ, "*")
}

// missingEnvVars returns the vars that do not appear in content as an
// assignment line. A line counts whether or not it is commented out
// (the sample ships every override commented), so `#VAR=default` and
// `VAR=value` both satisfy VAR. The trailing `=` prevents a prefix like
// OVN_NETWORK_DRAIN_TIMEOUT from satisfying OVN_NETWORK_DRAIN_SETTLE_DELAY.
func missingEnvVars(content string, vars []string) []string {
	var missing []string
	for _, v := range vars {
		re := regexp.MustCompile(`(?m)^\s*#?\s*` + regexp.QuoteMeta(v) + `=`)
		if !re.MatchString(content) {
			missing = append(missing, v)
		}
	}
	return missing
}

// missingYAMLKeys returns the keys that do not appear in content as a
// mapping key. A key counts whether it is commented, indented, or
// list-item-prefixed (`#   - vip:`). The `\s*:` anchor keeps dest_addr
// from being satisfied by a dest_addrs: line.
func missingYAMLKeys(content string, keys []string) []string {
	var missing []string
	for _, k := range keys {
		re := regexp.MustCompile(`(?m)^\s*#?\s*(?:- )?` + regexp.QuoteMeta(k) + `\s*:`)
		if !re.MatchString(content) {
			missing = append(missing, k)
		}
	}
	return missing
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

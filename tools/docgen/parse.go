package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
)

// sourceInfo bundles every fact the renderer needs about the upstream
// Go source. The parser is responsible for fully resolving references
// across config.go and metrics.go so the renderers can stay pure
// string formatters.
type sourceInfo struct {
	// Namespace is the Prometheus metric namespace prefix.
	Namespace string

	// Structs holds the parsed struct declarations keyed by Go type
	// name (Config, PortForwardRule, PortForwardVIP, configFile).
	Structs map[string]*structInfo

	// Flags are CLI flags declared in loadConfig, in source order.
	Flags []flagInfo

	// EnvByField maps Config field name to the env var that sets it.
	EnvByField map[string]string

	// YAMLByField maps Config field name to the YAML key (resolved
	// through configFile + applyFileConfig).
	YAMLByField map[string]string

	// DefaultByField maps Config field name to the literal default
	// taken from the `cfg := Config{...}` composite in loadConfig.
	DefaultByField map[string]string

	// YAMLOnly are registry options with no CLI flag (currently
	// port_forwards), in table order.
	YAMLOnly []yamlOnlyInfo

	// Metrics are Prometheus collectors declared in newMetricsRegistry,
	// in source order.
	Metrics []metricInfo
}

// yamlOnlyInfo is a config-file-only option: it has a YAML key and a
// Config field, but no CLI flag and no environment variable.
type yamlOnlyInfo struct {
	Key         string
	ConfigField string
	Desc        string
}

type structInfo struct {
	Name   string
	Fields []structField
}

type structField struct {
	Name    string
	Type    string
	YAMLTag string
	Comment string
}

type flagInfo struct {
	Name        string // CLI flag name (without leading --)
	Kind        string // "string" | "int" | "bool"
	Default     string // raw literal text (e.g. `"br-ex"`, `60`, `false`)
	Usage       string
	ConfigField string // Go field on Config that this flag writes to

	// ImplicitEnv is the env var pulled from an `os.Getenv("…")`
	// expression used as the flag's default (e.g. `--config`
	// defaults to `os.Getenv("OVN_NETWORK_CONFIG")`). Such flags
	// have no entry in applyEnvConfig but should still be cross-
	// referenced as env-var configurable.
	ImplicitEnv string
}

type metricInfo struct {
	Name        string   // unqualified metric name (Opts.Name)
	FullName    string   // namespace + "_" + name
	Kind        string   // "counter" | "gauge" | "histogram"
	IsVec       bool     // true for *Vec collectors
	Labels      []string // label names (nil for non-Vec)
	Help        string
	StructField string // metricsRegistry field that holds this collector
	// LabelValues maps each declared label to the literal values
	// seen in the bootstrap `WithLabelValues(...)` calls inside
	// newMetricsRegistry, in first-seen order. Empty for metrics
	// that are never pre-populated.
	LabelValues map[string][]string
}

func parseSource(root string) (*sourceInfo, error) {
	fset := token.NewFileSet()
	cfgFile, err := parser.ParseFile(fset, filepath.Join(root, "config.go"), nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse config.go: %w", err)
	}
	metricsFile, err := parser.ParseFile(fset, filepath.Join(root, "metrics.go"), nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse metrics.go: %w", err)
	}

	info := &sourceInfo{
		Structs:        map[string]*structInfo{},
		EnvByField:     map[string]string{},
		YAMLByField:    map[string]string{},
		DefaultByField: map[string]string{},
	}

	parseStructs(cfgFile, info)
	parseLoadConfig(cfgFile, info)
	parseConfigOptions(cfgFile, info)

	if err := parseMetrics(metricsFile, info); err != nil {
		return nil, err
	}

	return info, nil
}

// parseStructs collects every top-level struct declaration with its
// field-level documentation. We deliberately preserve fields verbatim
// (Go type as written in source, including pointer/slice prefixes)
// because that text is what we surface in the reference.
func parseStructs(f *ast.File, info *sourceInfo) {
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				continue
			}
			si := &structInfo{Name: ts.Name.Name}
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					sf := structField{
						Name: name.Name,
						Type: exprString(field.Type),
					}
					if field.Tag != nil {
						sf.YAMLTag = structTagValue(field.Tag.Value, "yaml")
					}
					sf.Comment = fieldComment(field)
					si.Fields = append(si.Fields, sf)
				}
			}
			info.Structs[ts.Name.Name] = si
		}
	}
}

// parseLoadConfig collects the CLI-only action flags declared directly on
// the FlagSet in loadConfig (--config, --version, --check-config). Every
// other flag comes from the option registry (see parseConfigOptions);
// these three configure what the process *does* rather than how it
// behaves, so they carry no env/YAML binding.
func parseLoadConfig(f *ast.File, info *sourceInfo) {
	fn := findFunc(f, "loadConfig")
	if fn == nil {
		return
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fi := flagFromCall(call); fi != nil {
			info.Flags = append(info.Flags, *fi)
		}
		return true
	})
}

// flagFromCall builds a flagInfo from an `fs.String/Int/Bool(...)` call.
// Only the CLI-only action flags are declared this way now; every other
// flag comes from the option registry.
func flagFromCall(call *ast.CallExpr) *flagInfo {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	recv, ok := sel.X.(*ast.Ident)
	if !ok || recv.Name != "fs" {
		return nil
	}
	var kind string
	switch sel.Sel.Name {
	case "String":
		kind = "string"
	case "Int":
		kind = "int"
	case "Bool":
		kind = "bool"
	default:
		return nil
	}
	if len(call.Args) < 3 {
		return nil
	}
	name, ok := stringLit(call.Args[0])
	if !ok {
		return nil
	}
	usage, ok := stringLit(call.Args[2])
	if !ok {
		return nil
	}
	fi := &flagInfo{
		Name:    name,
		Kind:    kind,
		Default: exprString(call.Args[1]),
		Usage:   usage,
	}
	if env, ok := getenvCallArg(call.Args[1]); ok {
		fi.ImplicitEnv = env
		fi.Default = ""
	}
	return fi
}

// getenvCallArg returns the literal argument of an `os.Getenv("X")`
// call expression, or ("", false) if expr is not such a call.
func getenvCallArg(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Getenv" {
		return "", false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "os" {
		return "", false
	}
	if len(call.Args) != 1 {
		return "", false
	}
	return stringLit(call.Args[0])
}

// optConstructors maps each registry constructor to the flag kind it
// declares. A YAML-only constructor maps to "" — it has no flag.
var optConstructors = map[string]string{
	"stringOpt":       "string",
	"boolOpt":         "bool",
	"intOpt":          "int",
	"durationOpt":     "string",
	"stringSliceOpt":  "string",
	"portForwardsOpt": "",
}

// parseConfigOptions reads the option registry — the single table in
// config.go that declares every configuration knob — and derives from it
// everything the reference pages need: the flag list (in table order),
// each option's default and usage, the Config field it binds to, and the
// env var / YAML key derived from its flag name.
//
// This replaces the six hand-synchronised code shapes docgen used to
// walk (flag block, defaults composite, fs.Visit switch, configFile
// struct, applyFileConfig, applyEnvConfig). One table in, one table out.
func parseConfigOptions(f *ast.File, info *sourceInfo) {
	fn := findFunc(f, "configOptions")
	if fn == nil {
		return
	}
	for _, stmt := range fn.Body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		lit, ok := ret.Results[0].(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, elt := range lit.Elts {
			call, ok := elt.(*ast.CallExpr)
			if !ok {
				continue
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				continue
			}
			kind, known := optConstructors[ident.Name]
			if !known {
				continue
			}
			appendOption(info, ident.Name, kind, call)
		}
	}
}

// appendOption records one registry row.
//
// The constructors share a positional shape:
//
//	stringOpt/boolOpt/intOpt/durationOpt(flag, default, usage, accessor)
//	stringSliceOpt(flag, usage, accessor)          — no default
//	portForwardsOpt(yamlKey, usage, accessor)      — YAML-only, no flag
func appendOption(info *sourceInfo, ctor, kind string, call *ast.CallExpr) {
	if len(call.Args) < 3 {
		return
	}
	first, ok := stringLit(call.Args[0])
	if !ok {
		return
	}

	var def, usage string
	var accessor ast.Expr
	switch ctor {
	case "stringSliceOpt", "portForwardsOpt":
		// (name, usage, accessor) — no default literal.
		if usage, ok = stringLit(call.Args[1]); !ok {
			return
		}
		accessor = call.Args[2]
	default:
		// (flag, default, usage, accessor)
		if len(call.Args) < 4 {
			return
		}
		def = exprString(call.Args[1])
		if usage, ok = stringLit(call.Args[2]); !ok {
			return
		}
		accessor = call.Args[3]
	}

	field := accessorField(accessor)

	if ctor == "portForwardsOpt" {
		// YAML-only: no flag, no env var. first is the YAML key.
		info.YAMLOnly = append(info.YAMLOnly, yamlOnlyInfo{
			Key:         first,
			ConfigField: field,
			Desc:        usage,
		})
		if field != "" {
			info.YAMLByField[field] = first
		}
		return
	}

	fi := flagInfo{
		Name:        first,
		Kind:        kind,
		Default:     def,
		Usage:       usage,
		ConfigField: field,
	}
	info.Flags = append(info.Flags, fi)

	if field == "" {
		return
	}
	// The env var and the YAML key are derived from the flag name — the
	// same derivation config.go performs at runtime — so they can never
	// drift from it.
	info.EnvByField[field] = envVarName(first)
	info.YAMLByField[field] = yamlKeyName(first)
	if def != "" {
		info.DefaultByField[field] = def
	}
}

// envVarName mirrors config.go: "reconcile-interval" -> "OVN_NETWORK_RECONCILE_INTERVAL".
func envVarName(flagName string) string {
	return "OVN_NETWORK_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// yamlKeyName mirrors config.go: "reconcile-interval" -> "reconcile_interval".
func yamlKeyName(flagName string) string {
	return strings.ReplaceAll(flagName, "-", "_")
}

// accessorField extracts "BridgeDev" from an accessor closure of the form
//
//	func(c *Config) *string { return &c.BridgeDev }
func accessorField(expr ast.Expr) string {
	fn, ok := expr.(*ast.FuncLit)
	if !ok || fn.Body == nil {
		return ""
	}
	for _, stmt := range fn.Body.List {
		ret, ok := stmt.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		unary, ok := ret.Results[0].(*ast.UnaryExpr)
		if !ok || unary.Op != token.AND {
			continue
		}
		sel, ok := unary.X.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		return sel.Sel.Name
	}
	return ""
}

func parseMetrics(f *ast.File, info *sourceInfo) error {
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if name.Name != "metricsNamespace" || i >= len(vs.Values) {
					continue
				}
				if v, ok := stringLit(vs.Values[i]); ok {
					info.Namespace = v
				}
			}
		}
	}

	fn := findFunc(f, "newMetricsRegistry")
	if fn == nil || fn.Body == nil {
		return fmt.Errorf("metrics.go: function newMetricsRegistry not found")
	}
	var cl *ast.CompositeLit
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if cl != nil {
			return false
		}
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := lit.Type.(*ast.Ident)
		if !ok || ident.Name != "metricsRegistry" {
			return true
		}
		cl = lit
		return false
	})
	if cl == nil {
		return fmt.Errorf("metrics.go: metricsRegistry composite literal not found")
	}

	// Map struct field name -> index in info.Metrics so we can look
	// the metric back up when walking the WithLabelValues bootstrap
	// calls below.
	indexByField := map[string]int{}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		fieldName, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		mi, ok := metricFromCall(kv.Value, info.Namespace)
		if !ok {
			continue
		}
		mi.StructField = fieldName.Name
		info.Metrics = append(info.Metrics, mi)
		indexByField[fieldName.Name] = len(info.Metrics) - 1
	}

	collectLabelValues(fn, indexByField, info.Metrics)
	return nil
}

// collectLabelValues walks newMetricsRegistry for calls of the form
// `m.<field>.WithLabelValues("v1", "v2", …).<op>(…)` and records the
// literal values against the corresponding metric. Values are
// associated positionally with the metric's declared labels.
func collectLabelValues(fn *ast.FuncDecl, indexByField map[string]int, metrics []metricInfo) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		outer, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		outerSel, ok := outer.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := outerSel.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		innerSel, ok := inner.Fun.(*ast.SelectorExpr)
		if !ok || innerSel.Sel.Name != "WithLabelValues" {
			return true
		}
		fieldSel, ok := innerSel.X.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := fieldSel.X.(*ast.Ident)
		if !ok || recv.Name != "m" {
			return true
		}
		idx, ok := indexByField[fieldSel.Sel.Name]
		if !ok {
			return true
		}
		labels := metrics[idx].Labels
		if len(labels) == 0 {
			return true
		}
		if metrics[idx].LabelValues == nil {
			metrics[idx].LabelValues = map[string][]string{}
		}
		for i, arg := range inner.Args {
			if i >= len(labels) {
				break
			}
			val, ok := stringLit(arg)
			if !ok {
				continue
			}
			label := labels[i]
			if !containsString(metrics[idx].LabelValues[label], val) {
				metrics[idx].LabelValues[label] = append(metrics[idx].LabelValues[label], val)
			}
		}
		return true
	})
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// metricFromCall recognises calls of the form
// `prometheus.New<Kind>(prometheus.<Kind>Opts{...}, []string{labels...})`
// and extracts the metric's Name, Help, kind, and label set.
func metricFromCall(expr ast.Expr, namespace string) (metricInfo, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return metricInfo{}, false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return metricInfo{}, false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "prometheus" {
		return metricInfo{}, false
	}
	kind, isVec := classifyConstructor(sel.Sel.Name)
	if kind == "" {
		return metricInfo{}, false
	}
	if len(call.Args) == 0 {
		return metricInfo{}, false
	}
	optsLit, ok := call.Args[0].(*ast.CompositeLit)
	if !ok {
		return metricInfo{}, false
	}
	var (
		name string
		help string
	)
	for _, e := range optsLit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Name":
			if v, ok := stringLit(kv.Value); ok {
				name = v
			}
		case "Help":
			if v, ok := stringLit(kv.Value); ok {
				help = v
			}
		}
	}
	if name == "" {
		return metricInfo{}, false
	}
	mi := metricInfo{
		Name:     name,
		FullName: name,
		Kind:     kind,
		IsVec:    isVec,
		Help:     help,
	}
	if namespace != "" {
		mi.FullName = namespace + "_" + name
	}
	if isVec && len(call.Args) >= 2 {
		mi.Labels = stringSliceLit(call.Args[1])
	}
	return mi, true
}

func classifyConstructor(name string) (kind string, isVec bool) {
	switch name {
	case "NewCounter":
		return "counter", false
	case "NewCounterVec":
		return "counter", true
	case "NewGauge":
		return "gauge", false
	case "NewGaugeVec":
		return "gauge", true
	case "NewHistogram":
		return "histogram", false
	case "NewHistogramVec":
		return "histogram", true
	}
	return "", false
}

func stringSliceLit(expr ast.Expr) []string {
	cl, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range cl.Elts {
		if v, ok := stringLit(e); ok {
			out = append(out, v)
		}
	}
	return out
}

// findFunc returns the top-level function declaration with the given
// name, or nil if absent.
func findFunc(f *ast.File, name string) *ast.FuncDecl {
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		if fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// stringLit returns the unquoted value of a basic string literal.
func stringLit(expr ast.Expr) (string, bool) {
	bl, ok := expr.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(bl.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

// structTagValue mirrors reflect.StructTag.Get without reaching for
// the reflect package. The input is the raw `…` source tag.
func structTagValue(raw, key string) string {
	unquoted, err := strconv.Unquote(raw)
	if err != nil {
		return ""
	}
	for len(unquoted) > 0 {
		i := 0
		for i < len(unquoted) && unquoted[i] == ' ' {
			i++
		}
		unquoted = unquoted[i:]
		if unquoted == "" {
			break
		}
		i = 0
		for i < len(unquoted) && unquoted[i] != ':' && unquoted[i] > ' ' && unquoted[i] != '"' {
			i++
		}
		if i == 0 || i+1 >= len(unquoted) || unquoted[i] != ':' || unquoted[i+1] != '"' {
			break
		}
		tagKey := unquoted[:i]
		unquoted = unquoted[i+1:]
		i = 1
		for i < len(unquoted) && unquoted[i] != '"' {
			if unquoted[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(unquoted) {
			break
		}
		qval := unquoted[:i+1]
		unquoted = unquoted[i+1:]
		if tagKey == key {
			val, err := strconv.Unquote(qval)
			if err != nil {
				return ""
			}
			// The yaml tag may carry comma-separated options
			// (e.g. `yaml:"foo,omitempty"`); only the key matters.
			if idx := strings.Index(val, ","); idx >= 0 {
				val = val[:idx]
			}
			return val
		}
	}
	return ""
}

// fieldComment returns the first available source documentation for
// a struct field, preferring the trailing line comment (which the
// existing structs use to annotate YAML semantics) over the doc
// comment block above the field.
func fieldComment(field *ast.Field) string {
	if c := commentText(field.Comment); c != "" {
		return c
	}
	return commentText(field.Doc)
}

func commentText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	var parts []string
	for _, c := range g.List {
		text := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		text = strings.TrimSpace(strings.TrimPrefix(text, "/*"))
		text = strings.TrimSpace(strings.TrimSuffix(text, "*/"))
		if text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

// exprString renders an arbitrary AST expression back to a compact
// source-like string. We avoid go/printer to keep the output stable
// across Go versions and free of trailing whitespace.
func exprString(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.BasicLit:
		return e.Value
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	case *ast.ArrayType:
		if e.Len == nil {
			return "[]" + exprString(e.Elt)
		}
		return "[" + exprString(e.Len) + "]" + exprString(e.Elt)
	case *ast.MapType:
		return "map[" + exprString(e.Key) + "]" + exprString(e.Value)
	case *ast.BinaryExpr:
		return exprString(e.X) + " " + e.Op.String() + " " + exprString(e.Y)
	case *ast.CallExpr:
		args := make([]string, 0, len(e.Args))
		for _, a := range e.Args {
			args = append(args, exprString(a))
		}
		return exprString(e.Fun) + "(" + strings.Join(args, ", ") + ")"
	case *ast.UnaryExpr:
		return e.Op.String() + exprString(e.X)
	case *ast.ParenExpr:
		return "(" + exprString(e.X) + ")"
	case *ast.CompositeLit:
		return exprString(e.Type) + "{…}"
	case *ast.InterfaceType:
		return "interface{}"
	}
	return ""
}

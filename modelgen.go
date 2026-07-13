//go:generate go run github.com/ovn-kubernetes/libovsdb/cmd/modelgen -p nbdb -o internal/nbdb schemas/ovn-nb.ovsschema
//go:generate go run github.com/ovn-kubernetes/libovsdb/cmd/modelgen -p sbdb -o internal/sbdb schemas/ovn-sb.ovsschema

package main

// This file anchors `go generate ./...` so the OVSDB models under
// internal/nbdb and internal/sbdb are regenerated from the OVN schemas
// checked in under schemas/, rather than hand-maintained. `make
// models-gen` runs the same two commands, and `make models-gen-check`
// fails the build when the committed output is stale — so a schema bump
// without a regen, or a hand-edit of generated code, is caught in CI.
//
// See docs/contributing/ovsdb-models.md for the supported OVN version
// range and the procedure for bumping it.

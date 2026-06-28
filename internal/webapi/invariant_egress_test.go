// Copyright 2026 Scott Friedman
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package webapi

import (
	"reflect"
	"testing"

	"github.com/scttfrdmn/foray/internal/brain"
	"github.com/scttfrdmn/foray/internal/gateway"
)

// Invariant gate (issue #32): no automatic egress of activations. By default
// only references (s3:// save refs, viz refs) and pixels-not-tensors metadata
// may cross a trace-result boundary — never the saved tensors themselves (the
// eDIF anti-pattern). The only egress path is the user-initiated, presigned
// export.
//
// The runtime JSON-key guards in api_test.go and http_test.go assert specific
// responses; this is the structural complement — it reflects over EVERY field
// of every result-boundary struct and fails if any field has a type that could
// carry tensor bytes (a []byte, a numeric slice/array, a map, or an opaque
// interface{} a future change could stuff a tensor into). A new field on any of
// these structs that isn't a scalar reference breaks the build here, before it
// can reach a wire test.
//
// Covered boundaries (every struct a trace result rides across):
//   - gateway.TraceResult — the worker->forayd->client reply
//   - brain.RawResult     — the brain-local view of a trace
//   - brain.Result        — a rung outcome interpreted against the question
//   - webapi.resultView    — the flat page projection the SPA reads
func TestNoTensorEgressBoundaries(t *testing.T) {
	boundaries := []struct {
		name string
		typ  reflect.Type
	}{
		{"gateway.TraceResult", reflect.TypeOf(gateway.TraceResult{})},
		{"brain.RawResult", reflect.TypeOf(brain.RawResult{})},
		{"brain.Result", reflect.TypeOf(brain.Result{})},
		{"webapi.resultView", reflect.TypeOf(resultView{})},
	}
	for _, b := range boundaries {
		t.Run(b.name, func(t *testing.T) {
			for i := 0; i < b.typ.NumField(); i++ {
				f := b.typ.Field(i)
				if reason := tensorBearing(f.Type); reason != "" {
					t.Errorf("%s.%s is %s — a tensor-bearing field type; "+
						"trace-result boundaries may carry only scalar references "+
						"(no-automatic-egress invariant, CLAUDE.md / issue #32)",
						b.name, f.Name, reason)
				}
			}
		})
	}
}

// tensorBearing reports why a boundary field's type could smuggle tensor bytes,
// or "" if it is a safe scalar reference. Allowed: strings (s3://, viz refs,
// generated code), bools (effect-present), and the integer/float scalars that
// label a rung. Forbidden: []byte and other slices/arrays (raw values), maps,
// and interface{} (an opaque escape hatch). A nested struct is recursed into so
// an embedded value-bag can't hide a tensor one level down.
func tensorBearing(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return ""
	case reflect.Slice, reflect.Array:
		return "a " + t.Kind().String() + " of " + t.Elem().Kind().String()
	case reflect.Map:
		return "a map"
	case reflect.Interface:
		return "an interface{} (opaque — could hold a tensor)"
	case reflect.Pointer:
		return tensorBearing(t.Elem())
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if reason := tensorBearing(t.Field(i).Type); reason != "" {
				return "a struct whose ." + t.Field(i).Name + " is " + reason
			}
		}
		return ""
	default:
		return "a " + t.Kind().String()
	}
}

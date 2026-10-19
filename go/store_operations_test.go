package job

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"testing"

	wire "github.com/openabstractions/abstraction-job/go/rec"
)

// These protocol frames are fixed references. Operations remain strings and
// adding constants changes no request spelling, field order or omission.
var storeOperationFrames = []struct{ name, operation, frame string }{
	{"OpSubmit", wire.OpSubmit, `{"op":"submit","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpLoad", wire.OpLoad, `{"op":"load","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpList", wire.OpList, `{"op":"list","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpOrphans", wire.OpOrphans, `{"op":"orphans","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpClaimable", wire.OpClaimable, `{"op":"claimable","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpClaim", wire.OpClaim, `{"op":"claim","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpRenew", wire.OpRenew, `{"op":"renew","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpRelease", wire.OpRelease, `{"op":"release","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpSetIntent", wire.OpSetIntent, `{"op":"set_intent","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpRecall", wire.OpRecall, `{"op":"recall","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
	{"OpWrite", wire.OpWrite, `{"op":"write","id":"job","owner":"worker","epoch":7,"ttl_ms":1000,"want":"cancel","by":"person","reason":"test","record":{"x":1},"base":{"x":0}}`},
}

func TestStoreOperationConstantsKeepElevenGoldenFrames(t *testing.T) {
	var operations []string
	for _, ref := range storeOperationFrames {
		operations = append(operations, ref.operation)
		t.Run(ref.name, func(t *testing.T) {
			request := wire.Request{Op: ref.operation, ID: "job", Owner: "worker", Epoch: 7, TTLMs: 1000, Want: "cancel", By: "person", Reason: "test", Record: wire.Raw(`{ "x": 1 }`), Base: wire.Raw(`{ "x": 0 }`)}
			if actual := wire.EncodeRequest(&request); string(actual) != ref.frame {
				t.Fatalf("frame changed\nwant %s\ngot  %s", ref.frame, actual)
			}
			decoded, err := wire.DecodeRequest([]byte(ref.frame))
			if err != nil || decoded.Op != ref.operation || !bytes.Equal(wire.EncodeRequest(decoded), []byte(ref.frame)) {
				t.Fatal(decoded, err)
			}
		})
	}
	if !reflect.DeepEqual(operations, wire.Operations) {
		t.Fatal("constant/golden roster differs from definition", operations, wire.Operations)
	}
}

func TestEveryStoreOperationHasAConstantServeCase(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{}
	for _, declaration := range f.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Name.Name != "apply" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expression := range clause.List {
				name, ok := expression.(*ast.SelectorExpr)
				if !ok {
					t.Fatal("serve operation lost its generated constant", expression)
				}
				owner, ok := name.X.(*ast.Ident)
				if !ok || owner.Name != "wire" {
					t.Fatal("wrong operation owner", name)
				}
				cases[name.Sel.Name] = true
			}
			return false
		})
	}
	if len(cases) != len(wire.Operations) {
		t.Fatal("serve case count", cases)
	}
	for _, ref := range storeOperationFrames {
		if !cases[ref.name] {
			t.Fatal("missing serve case", ref.name)
		}
	}
}

func TestStoreUnlistedOperationKeepsUnknownOp(t *testing.T) {
	for _, operation := range []string{"future_action", "Submit", ""} {
		request, err := wire.DecodeRequest(wire.EncodeRequest(&wire.Request{Op: operation}))
		if err != nil {
			t.Fatal("string operation was narrowed", operation, err)
		}
		// An unknown operation reaches no Store method, even with no store.
		answer := apply(nil, *request)
		if answer.Kind != wire.UnknownOperation || answer.Error != "unknown op "+operation {
			t.Fatal(answer)
		}
	}
}

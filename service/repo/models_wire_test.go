package repo

import (
	"encoding/json"
	"reflect"
	"testing"
)

// WikiRoot is served verbatim by GET /v1/wiki/roots and consumed by the UI,
// NimoOS-AI, NimoOS-Parser and the CLI, all of which key on the PascalCase
// names Go emitted by default. Pinning each field's json tag to its current
// name turns a future field rename into a compile-visible contract change
// instead of a silent wire break (Parser's verify once read [] and nearly
// retired its whole ledger over exactly that failure mode).
func TestWikiRootWireKeysArePinned(t *testing.T) {
	rt := reflect.TypeOf(WikiRoot{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if got := f.Tag.Get("json"); got != f.Name {
			t.Errorf("field %s: json tag %q, want %q (wire contract is PascalCase)", f.Name, got, f.Name)
		}
	}
	b, err := json.Marshal(WikiRoot{ID: "r1", Path: "/DATA", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ID", "Path", "Level", "Enabled"} {
		if _, ok := m[k]; !ok {
			t.Errorf("wire key %q missing from %s", k, b)
		}
	}
}

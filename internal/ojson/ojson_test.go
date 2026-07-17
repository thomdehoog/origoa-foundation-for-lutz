package ojson

import (
	"bytes"
	"testing"
)

func TestRoundTripPreservesOrder(t *testing.T) {
	in := []byte("{\n  \"z\": 1,\n  \"a\": {\n    \"nested\": \"x\",\n    \"first\": true\n  },\n  \"m\": [1, 2]\n}\n")
	o, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := o.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// Canonical format re-indents but must preserve key order everywhere.
	o2, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := o2.Keys(), []string{"z", "a", "m"}; !equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	// A second load/save cycle must be byte-identical (stable serialization).
	out2, _ := o2.Encode()
	if !bytes.Equal(out, out2) {
		t.Fatalf("unstable serialization:\n%s\nvs\n%s", out, out2)
	}
	// Nested order is preserved verbatim.
	if !bytes.Contains(out, []byte(`"nested"`)) || bytes.Index(out, []byte(`"nested"`)) > bytes.Index(out, []byte(`"first"`)) {
		t.Fatalf("nested key order lost:\n%s", out)
	}
}

func TestSetKeepsPositionAppendsNew(t *testing.T) {
	o, _ := Parse([]byte(`{"a":1,"b":2,"c":3}`))
	o.SetString("b", "changed")
	o.SetString("d", "new")
	if got, want := o.Keys(), []string{"a", "b", "c", "d"}; !equal(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
	o.Delete("a")
	if got, want := o.Keys(), []string{"b", "c", "d"}; !equal(got, want) {
		t.Fatalf("keys after delete = %v, want %v", got, want)
	}
}

func TestParseRejectsNonObject(t *testing.T) {
	for _, in := range []string{`[1,2]`, `"str"`, `123`, `{"a":`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Fatalf("Parse(%q) succeeded, want error", in)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

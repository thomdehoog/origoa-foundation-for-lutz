// Package ojson provides an order-preserving JSON object.
//
// The repository format requires stable serialization: loading and saving an
// unchanged artifact must produce identical bytes, and modifications must not
// reorder existing properties. New properties are appended.
package ojson

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Obj is a JSON object whose key order is preserved across decode/encode.
// Values are kept as raw JSON, so nested content round-trips verbatim.
type Obj struct {
	keys []string
	vals map[string]json.RawMessage
}

func New() *Obj {
	return &Obj{vals: map[string]json.RawMessage{}}
}

func Parse(b []byte) (*Obj, error) {
	o := New()
	if err := o.UnmarshalJSON(b); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *Obj) UnmarshalJSON(b []byte) error {
	o.keys = nil
	o.vals = map[string]json.RawMessage{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("ojson: expected object, got %v", tok)
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if _, dup := o.vals[key]; !dup {
			o.keys = append(o.keys, key)
		}
		o.vals[key] = raw
	}
	_, err = dec.Token() // closing '}'
	return err
}

func (o *Obj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(o.vals[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Encode renders the object in the canonical repository format:
// two-space indent, preserved key order, trailing newline.
func (o *Obj) Encode() ([]byte, error) {
	compact, err := o.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", "  "); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func (o *Obj) Keys() []string { return append([]string(nil), o.keys...) }

func (o *Obj) Has(key string) bool {
	_, ok := o.vals[key]
	return ok
}

func (o *Obj) Get(key string) (json.RawMessage, bool) {
	v, ok := o.vals[key]
	return v, ok
}

// GetString returns the value if it is a JSON string, else "".
func (o *Obj) GetString(key string) string {
	var s string
	if v, ok := o.vals[key]; ok {
		_ = json.Unmarshal(v, &s)
	}
	return s
}

// Set stores a raw JSON value, appending the key if new and keeping its
// position if it already exists.
func (o *Obj) Set(key string, raw json.RawMessage) {
	if _, ok := o.vals[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.vals[key] = raw
}

func (o *Obj) SetString(key, val string) {
	b, _ := json.Marshal(val)
	o.Set(key, b)
}

// SetAny marshals any Go value and stores it.
func (o *Obj) SetAny(key string, val any) error {
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	o.Set(key, b)
	return nil
}

func (o *Obj) Delete(key string) {
	if _, ok := o.vals[key]; !ok {
		return
	}
	delete(o.vals, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

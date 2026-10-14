package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Unknown-key preservation.
//
// Config is a typed struct, so a decode drops every JSON key it does not
// model: a hand-added custom key, a typo, or a key a newer tclaude wrote.
// Save must not take those with it, so it re-reads the file it replaces and
// carries every unmodeled key over into the new bytes, at the position the
// typed encoding would not otherwise produce (appended after the modeled
// keys of the same object).
//
// Two kinds of unmodeled key are NOT carried over:
//   - retiredConfigKeys: keys tclaude itself used to write and has since
//     retired. Dropping them on the next save is the intended cleanup.
//   - keys inside array elements: elements carry no identity, so an edit that
//     reorders or removes elements would graft the key onto the wrong one.
//     UnpreservedConfigKeys reports these so a UI can warn about them.

// retiredConfigKeys lists dotted paths tclaude once modeled and retired. A
// save drops them silently; they are not reported as unknown.
var retiredConfigKeys = map[string]bool{
	"dashboard.show_debug_tab": true, // the Debug tab became Activity › Performance, always shown
}

// orderedObject is a JSON object that keeps its key order, so a merge can
// re-emit the typed encoding's order byte-for-byte.
type orderedObject struct {
	keys []string
	vals map[string]any
}

func (o *orderedObject) set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *orderedObject) MarshalJSON() ([]byte, error) {
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
		vb, err := json.Marshal(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// parseOrdered decodes JSON into orderedObject / []any / scalar values,
// keeping numbers as json.Number so they re-encode exactly.
func parseOrdered(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := parseOrderedValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data after JSON value")
	}
	return v, nil
}

func parseOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &orderedObject{vals: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("non-string object key %v", kt)
				}
				v, err := parseOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				obj.set(k, v)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := parseOrderedValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return t, nil
	}
}

// jsonFieldTypes maps a struct type's JSON field names to their types.
func jsonFieldTypes(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = t.Field(i).Type
		}
	}
	return out
}

func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

func joinPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}

// carryUnknown copies into dst every unmodeled key of src (an object modeled
// by struct type t), descending through modeled struct and map values. It
// returns whether it added anything, so a parent dst created only to hold
// carried keys can be skipped when nothing landed in it.
func carryUnknown(prefix string, dst, src *orderedObject, t reflect.Type) bool {
	fields := jsonFieldTypes(t)
	added := false
	for _, k := range src.keys {
		path := joinPath(prefix, k)
		ft, modeled := fields[k]
		if !modeled {
			if retiredConfigKeys[path] {
				continue
			}
			if _, exists := dst.vals[k]; !exists {
				dst.set(k, src.vals[k])
				added = true
			}
			continue
		}
		if carryUnknownValue(path, dst, k, src.vals[k], derefType(ft)) {
			added = true
		}
	}
	return added
}

// carryUnknownValue descends into the modeled field k of dst, whose old value
// is srcVal and whose Go type is ft.
func carryUnknownValue(path string, dst *orderedObject, k string, srcVal any, ft reflect.Type) bool {
	srcObj, ok := srcVal.(*orderedObject)
	if !ok {
		return false
	}
	switch ft.Kind() {
	case reflect.Struct:
		dstObj, ok := dst.vals[k].(*orderedObject)
		if ok {
			return carryUnknown(path, dstObj, srcObj, ft)
		}
		if _, exists := dst.vals[k]; exists && dst.vals[k] != nil {
			return false // the field changed shape; leave the new value alone
		}
		// The new encoding omits the block (all defaults): recreate it
		// only if it has unmodeled keys to hold.
		holder := &orderedObject{vals: map[string]any{}}
		if !carryUnknown(path, holder, srcObj, ft) {
			return false
		}
		dst.set(k, holder)
		return true
	case reflect.Map:
		// Map keys are arbitrary by design; descend only into entries the
		// new config still has, so a deleted entry stays deleted.
		dstObj, ok := dst.vals[k].(*orderedObject)
		if !ok {
			return false
		}
		elem := derefType(ft.Elem())
		added := false
		for _, mk := range srcObj.keys {
			if _, still := dstObj.vals[mk]; !still {
				continue
			}
			if carryUnknownValue(path+"."+mk, dstObj, mk, srcObj.vals[mk], elem) {
				added = true
			}
		}
		return added
	}
	return false
}

// mergeUnknownKeys returns typed (the canonical MarshalIndent of a Config)
// with old's unmodeled keys carried over. It returns typed unchanged when old
// is unparseable or contributes nothing.
func mergeUnknownKeys(typed, old []byte) ([]byte, error) {
	oldTree, err := parseOrdered(old)
	if err != nil {
		return typed, nil
	}
	oldObj, ok := oldTree.(*orderedObject)
	if !ok {
		return typed, nil
	}
	newTree, err := parseOrdered(typed)
	if err != nil {
		return nil, err
	}
	newObj, ok := newTree.(*orderedObject)
	if !ok {
		return typed, nil
	}
	if !carryUnknown("", newObj, oldObj, reflect.TypeOf(Config{})) {
		return typed, nil
	}
	compact, err := json.Marshal(newObj)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// UnknownConfigKeys parses a config file's bytes and returns the dotted paths
// of keys the Config schema does not model, split into those a Save carries
// over (preserved) and those it drops (dropped: keys inside array elements).
// Retired keys are in neither list. Both are sorted; both are empty for
// unparseable input.
func UnknownConfigKeys(data []byte) (preserved, dropped []string) {
	var tree map[string]any
	if err := json.Unmarshal(data, &tree); err != nil {
		return nil, nil
	}
	collectUnknown("", tree, reflect.TypeOf(Config{}), false, &preserved, &dropped)
	sort.Strings(preserved)
	sort.Strings(dropped)
	return preserved, dropped
}

func collectUnknown(prefix string, node map[string]any, t reflect.Type, inArray bool, preserved, dropped *[]string) {
	fields := jsonFieldTypes(t)
	for key, val := range node {
		path := joinPath(prefix, key)
		ft, ok := fields[key]
		if !ok {
			if retiredConfigKeys[path] {
				continue
			}
			if inArray {
				*dropped = append(*dropped, path)
			} else {
				*preserved = append(*preserved, path)
			}
			continue
		}
		descendUnknown(path, val, ft, inArray, preserved, dropped)
	}
}

func descendUnknown(path string, val any, ft reflect.Type, inArray bool, preserved, dropped *[]string) {
	ft = derefType(ft)
	switch ft.Kind() {
	case reflect.Struct:
		if obj, ok := val.(map[string]any); ok {
			collectUnknown(path, obj, ft, inArray, preserved, dropped)
		}
	case reflect.Map:
		if obj, ok := val.(map[string]any); ok {
			for mk, mv := range obj {
				descendUnknown(path+"."+mk, mv, ft.Elem(), inArray, preserved, dropped)
			}
		}
	case reflect.Slice, reflect.Array:
		if arr, ok := val.([]any); ok {
			for i, item := range arr {
				descendUnknown(fmt.Sprintf("%s[%d]", path, i), item, ft.Elem(), true, preserved, dropped)
			}
		}
	}
}

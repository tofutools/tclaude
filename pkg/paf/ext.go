package paf

import (
	"encoding/json"
	"fmt"
)

// collectExtensionIDs records every extension id used in an "extensions"
// object anywhere in a JSON document, and refuses ids that are not valid.
func collectExtensionIDs(doc []byte, seen map[string]bool) error {
	return forEachExtension(doc, func(id string, _ map[string]any) error {
		seen[id] = true
		return nil
	})
}

// forEachExtension calls fn for every extension payload in a JSON document.
func forEachExtension(doc []byte, fn func(id string, data map[string]any) error) error {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return err
	}
	return walkExtensions(v, fn)
}

func walkExtensions(v any, fn func(id string, data map[string]any) error) error {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "extensions" {
				ext, ok := child.(map[string]any)
				if !ok {
					return fmt.Errorf("extensions is not an object")
				}
				for id, data := range ext {
					if !wellFormedExtensionID(id) {
						return fmt.Errorf("invalid extension id %q", id)
					}
					obj, ok := data.(map[string]any)
					if !ok {
						return fmt.Errorf("extension %q data is not an object", id)
					}
					if err := fn(id, obj); err != nil {
						return err
					}
				}
				// An extension's payload is that extension's business; its
				// own keys are not PAF objects.
				continue
			}
			if err := walkExtensions(child, fn); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range x {
			if err := walkExtensions(child, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Decode decodes one extension's data into v. It reports false when the
// extension is absent.
func (e Extensions) Decode(id string, v any) (bool, error) {
	raw, ok := e[id]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(raw, v)
}

// Set encodes v as one extension's data, creating the map when needed.
func (e *Extensions) Set(id string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if *e == nil {
		*e = Extensions{}
	}
	(*e)[id] = raw
	return nil
}

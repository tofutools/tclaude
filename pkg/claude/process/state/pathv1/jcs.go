package pathv1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type jcsObject map[string]any

type duplicateObjectKeyError struct{ key string }

func (e *duplicateObjectKeyError) Error() string {
	const maxDiagnosticRunes = 128
	runes := []rune(e.key)
	if len(runes) > maxDiagnosticRunes {
		return fmt.Sprintf("duplicate object key %q (name truncated)", string(runes[:maxDiagnosticRunes])+"…")
	}
	return fmt.Sprintf("duplicate object key %q", e.key)
}

// CanonicalCheckpointProjection applies the exact completion projection and
// emits RFC 8785 JSON Canonicalization Scheme bytes.
func CanonicalCheckpointProjection(checkpoint []byte, selfCommandID string) ([]byte, error) {
	if len(checkpoint) > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(checkpoint), Maximum: MaxCheckpointBytes}
	}
	value, err := parseJCS(checkpoint)
	if err != nil {
		return nil, err
	}
	object, ok := value.(jcsObject)
	if !ok {
		return nil, fmt.Errorf("checkpoint projection root is not an object")
	}
	delete(object, "status")
	delete(object, "lastLogSeq")
	delete(object, "logChecksum")
	if selfCommandID != "" {
		commands, ok := object["outstandingCommands"].(jcsObject)
		if !ok {
			return nil, fmt.Errorf("checkpoint outstandingCommands is not an object")
		}
		if _, ok := commands[selfCommandID]; !ok {
			return nil, fmt.Errorf("completion self command %q missing from checkpoint", selfCommandID)
		}
		delete(commands, selfCommandID)
	}
	var out bytes.Buffer
	if err := writeJCS(&out, object); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func parseJCS(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("checkpoint JSON is not valid UTF-8")
	}
	if err := validateJSONSurrogates(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := parseJCSValue(dec)
	if err != nil {
		return nil, fmt.Errorf("decode checkpoint projection: %w", err)
	}
	if token, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("decode checkpoint projection trailing token %v: %w", token, err)
	}
	return value, nil
}

func validateJSONSurrogates(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString {
				continue
			}
			i++
			if i >= len(data) {
				return fmt.Errorf("truncated JSON escape")
			}
			if data[i] != 'u' {
				continue
			}
			if i+4 >= len(data) {
				return fmt.Errorf("truncated JSON unicode escape")
			}
			value, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
			if err != nil {
				return fmt.Errorf("invalid JSON unicode escape")
			}
			i += 4
			if value >= 0xd800 && value <= 0xdbff {
				if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
					return fmt.Errorf("unpaired high surrogate")
				}
				low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return fmt.Errorf("unpaired high surrogate")
				}
				i += 6
			} else if value >= 0xdc00 && value <= 0xdfff {
				return fmt.Errorf("unpaired low surrogate")
			}
		}
	}
	return nil
}
func parseJCSValue(dec *json.Decoder) (any, error) {
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delimiter {
	case '{':
		object := jcsObject{}
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("object key is not a string")
			}
			if _, exists := object[key]; exists {
				return nil, &duplicateObjectKeyError{key: key}
			}
			value, err := parseJCSValue(dec)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("unterminated object")
		}
		return object, nil
	case '[':
		var array []any
		for dec.More() {
			value, err := parseJCSValue(dec)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("unterminated array")
		}
		return array, nil
	default:
		return nil, fmt.Errorf("unexpected delimiter %q", delimiter)
	}
}

func writeJCS(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if value {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		return writeJCSString(out, value)
	case json.Number:
		number, err := canonicalJCSNumber(string(value))
		if err != nil {
			return err
		}
		out.WriteString(number)
	case []any:
		out.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeJCS(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case jcsObject:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sortUTF16(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeJCSString(out, key); err != nil {
				return err
			}
			out.WriteByte(':')
			if err := writeJCS(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported checkpoint JSON value %T", value)
	}
	return nil
}

func writeJCSString(out *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("invalid UTF-8 JSON string")
	}
	out.WriteByte('"')
	const hex = "0123456789abcdef"
	for _, r := range value {
		switch r {
		case '"', '\\':
			out.WriteByte('\\')
			out.WriteRune(r)
		case '\b':
			out.WriteString(`\b`)
		case '\t':
			out.WriteString(`\t`)
		case '\n':
			out.WriteString(`\n`)
		case '\f':
			out.WriteString(`\f`)
		case '\r':
			out.WriteString(`\r`)
		default:
			if r < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte(hex[byte(r)>>4])
				out.WriteByte(hex[byte(r)&15])
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
	return nil
}

func sortUTF16(values []string) {
	type key struct {
		value string
		units []uint16
	}
	keys := make([]key, len(values))
	for i, value := range values {
		keys[i] = key{value: value, units: utf16.Encode([]rune(value))}
	}
	slices.SortFunc(keys, func(a, b key) int {
		left, right := a.units, b.units
		for i := 0; i < len(left) && i < len(right); i++ {
			if left[i] != right[i] {
				if left[i] < right[i] {
					return -1
				}
				return 1
			}
		}
		return len(left) - len(right)
	})
	for i := range keys {
		values[i] = keys[i].value
	}
}

func canonicalJCSNumber(raw string) (string, error) {
	number, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
		return "", fmt.Errorf("invalid I-JSON number %q", raw)
	}
	if number == 0 {
		return "0", nil
	}
	rendered := strconv.FormatFloat(number, 'g', -1, 64)
	lower := strings.ToLower(rendered)
	index := strings.IndexByte(lower, 'e')
	absolute := math.Abs(number)
	if absolute >= 1e-6 && absolute < 1e21 && index >= 0 {
		return expandExponent(lower), nil
	}
	if index < 0 {
		return lower, nil
	}
	mantissa, exponent := lower[:index], lower[index+1:]
	sign := "+"
	if strings.HasPrefix(exponent, "-") {
		sign = "-"
		exponent = exponent[1:]
	} else {
		exponent = strings.TrimPrefix(exponent, "+")
	}
	exponent = strings.TrimLeft(exponent, "0")
	if exponent == "" {
		exponent = "0"
	}
	return mantissa + "e" + sign + exponent, nil
}
func expandExponent(value string) string {
	index := strings.IndexByte(value, 'e')
	mantissa := value[:index]
	exponent, _ := strconv.Atoi(value[index+1:])
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = mantissa[1:]
	}
	dot := strings.IndexByte(mantissa, '.')
	digits := strings.ReplaceAll(mantissa, ".", "")
	if dot < 0 {
		dot = len(mantissa)
	}
	position := dot + exponent
	if position <= 0 {
		return sign + "0." + strings.Repeat("0", -position) + digits
	}
	if position >= len(digits) {
		return sign + digits + strings.Repeat("0", position-len(digits))
	}
	return sign + digits[:position] + "." + digits[position:]
}

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
	runes := 0
	for offset := range e.key {
		if runes == maxDiagnosticRunes {
			return fmt.Sprintf("duplicate object key %q (name truncated)", e.key[:offset]+"…")
		}
		runes++
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
	return encodeJCSBounded(object, MaxCheckpointBytes)
}

func encodeJCS(value any) ([]byte, error) {
	var size jcsSizer
	return encodeJCSMeasured(value, &size)
}

func encodeJCSBounded(value any, maximum int) ([]byte, error) {
	size := jcsSizer{maximum: maximum}
	return encodeJCSMeasured(value, &size)
}

func encodeJCSMeasured(value any, size *jcsSizer) ([]byte, error) {
	// Measure first so number normalization cannot grow a geometrically
	// over-allocated buffer, then allocate exactly the returned byte count.
	if err := writeJCS(size, value); err != nil {
		return nil, err
	}
	out := jcsBuffer{data: make([]byte, 0, size.size)}
	if err := writeJCS(&out, value); err != nil {
		return nil, err
	}
	if len(out.data) != size.size {
		return nil, fmt.Errorf("canonical JCS size changed during encoding")
	}
	return out.data, nil
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

type jcsSink interface {
	writeByte(byte) error
	writeString(string) error
	writeRune(rune) error
}

// A bounded jcsSizer stops at the first byte beyond its ceiling. Its reported
// value is saturated at limit+1 because the rest of an adversarial value is
// deliberately never materialized or traversed. A zero maximum is unbounded
// for internal canonical digest encodings that have their own container limit.
type jcsSizer struct {
	size    int
	maximum int
}

func (s *jcsSizer) add(size int) error {
	const maxInt = int(^uint(0) >> 1)
	if size < 0 || size > maxInt-s.size {
		return fmt.Errorf("canonical JCS size overflows int")
	}
	if s.maximum > 0 && size > s.maximum-s.size {
		s.size = s.maximum + 1
		return &OverBudgetError{Limit: "checkpoint_bytes", Value: s.size, Maximum: s.maximum}
	}
	s.size += size
	return nil
}

func (s *jcsSizer) writeByte(byte) error           { return s.add(1) }
func (s *jcsSizer) writeString(value string) error { return s.add(len(value)) }
func (s *jcsSizer) writeRune(value rune) error {
	size := utf8.RuneLen(value)
	if size < 0 {
		return fmt.Errorf("invalid UTF-8 JSON rune")
	}
	return s.add(size)
}

type jcsBuffer struct{ data []byte }

func (b *jcsBuffer) writeByte(value byte) error {
	b.data = append(b.data, value)
	return nil
}

func (b *jcsBuffer) writeString(value string) error {
	b.data = append(b.data, value...)
	return nil
}

func (b *jcsBuffer) writeRune(value rune) error {
	b.data = utf8.AppendRune(b.data, value)
	return nil
}

func writeJCS(out jcsSink, value any) error {
	switch value := value.(type) {
	case nil:
		return out.writeString("null")
	case bool:
		if value {
			return out.writeString("true")
		}
		return out.writeString("false")
	case string:
		return writeJCSString(out, value)
	case json.Number:
		number, err := canonicalJCSNumber(string(value))
		if err != nil {
			return err
		}
		return out.writeString(number)
	case []any:
		if err := out.writeByte('['); err != nil {
			return err
		}
		for i, item := range value {
			if i > 0 {
				if err := out.writeByte(','); err != nil {
					return err
				}
			}
			if err := writeJCS(out, item); err != nil {
				return err
			}
		}
		return out.writeByte(']')
	case jcsObject:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sortUTF16(keys)
		if err := out.writeByte('{'); err != nil {
			return err
		}
		for i, key := range keys {
			if i > 0 {
				if err := out.writeByte(','); err != nil {
					return err
				}
			}
			if err := writeJCSString(out, key); err != nil {
				return err
			}
			if err := out.writeByte(':'); err != nil {
				return err
			}
			if err := writeJCS(out, value[key]); err != nil {
				return err
			}
		}
		return out.writeByte('}')
	default:
		return fmt.Errorf("unsupported checkpoint JSON value %T", value)
	}
}

func writeJCSString(out jcsSink, value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("invalid UTF-8 JSON string")
	}
	if err := out.writeByte('"'); err != nil {
		return err
	}
	const hex = "0123456789abcdef"
	for _, r := range value {
		switch r {
		case '"', '\\':
			if err := out.writeByte('\\'); err != nil {
				return err
			}
			if err := out.writeRune(r); err != nil {
				return err
			}
		case '\b':
			if err := out.writeString(`\b`); err != nil {
				return err
			}
		case '\t':
			if err := out.writeString(`\t`); err != nil {
				return err
			}
		case '\n':
			if err := out.writeString(`\n`); err != nil {
				return err
			}
		case '\f':
			if err := out.writeString(`\f`); err != nil {
				return err
			}
		case '\r':
			if err := out.writeString(`\r`); err != nil {
				return err
			}
		default:
			if r < 0x20 {
				if err := out.writeString(`\u00`); err != nil {
					return err
				}
				if err := out.writeByte(hex[byte(r)>>4]); err != nil {
					return err
				}
				if err := out.writeByte(hex[byte(r)&15]); err != nil {
					return err
				}
			} else {
				if err := out.writeRune(r); err != nil {
					return err
				}
			}
		}
	}
	return out.writeByte('"')
}

func sortUTF16(values []string) {
	slices.SortFunc(values, compareUTF16)
}

type utf16Iterator struct {
	value   string
	pending uint16
}

func (i *utf16Iterator) next() (uint16, bool) {
	if i.pending != 0 {
		value := i.pending
		i.pending = 0
		return value, true
	}
	if i.value == "" {
		return 0, false
	}
	r, size := utf8.DecodeRuneInString(i.value)
	i.value = i.value[size:]
	if r <= 0xffff {
		return uint16(r), true
	}
	high, low := utf16.EncodeRune(r)
	i.pending = uint16(low)
	return uint16(high), true
}

func compareUTF16(left, right string) int {
	a, b := utf16Iterator{value: left}, utf16Iterator{value: right}
	for {
		leftUnit, leftOK := a.next()
		rightUnit, rightOK := b.next()
		switch {
		case !leftOK && !rightOK:
			return 0
		case !leftOK:
			return -1
		case !rightOK:
			return 1
		case leftUnit < rightUnit:
			return -1
		case leftUnit > rightUnit:
			return 1
		}
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

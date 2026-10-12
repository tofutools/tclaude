package seal

import (
	"errors"
	"strings"
)

// Bech32 (BIP 173), the encoding age uses for its keys. Only what age keys
// need: 8-bit data to 5-bit groups and back, and the checksum.

const charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

var gen = [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}

func polymod(values []byte) uint32 {
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func hrpExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc, bits uint32
	maxv := uint32(1)<<to - 1
	var out []byte
	for _, b := range data {
		if uint32(b)>>from != 0 {
			return nil, errors.New("bech32: invalid data")
		}
		acc = acc<<from | uint32(b)
		bits += uint32(from)
		for bits >= uint32(to) {
			bits -= uint32(to)
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(uint32(to)-bits)&maxv))
		}
	} else if bits >= uint32(from) || acc<<(uint32(to)-bits)&maxv != 0 {
		return nil, errors.New("bech32: invalid padding")
	}
	return out, nil
}

// encode returns hrp + "1" + data + checksum, in lower case.
func encode(hrp string, data []byte) (string, error) {
	hrp = strings.ToLower(hrp)
	values, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	mod := polymod(append(append(hrpExpand(hrp), values...), 0, 0, 0, 0, 0, 0)) ^ 1
	var b strings.Builder
	b.WriteString(hrp)
	b.WriteByte('1')
	for _, v := range values {
		b.WriteByte(charset[v])
	}
	for i := 0; i < 6; i++ {
		b.WriteByte(charset[(mod>>uint(5*(5-i)))&31])
	}
	return b.String(), nil
}

// decode parses a bech32 string of either case.
func decode(s string) (hrp string, data []byte, err error) {
	if strings.ToLower(s) != s && strings.ToUpper(s) != s {
		return "", nil, errors.New("bech32: mixed case")
	}
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return "", nil, errors.New("bech32: invalid separator")
	}
	hrp = s[:pos]
	var values []byte
	for i := pos + 1; i < len(s); i++ {
		v := strings.IndexByte(charset, s[i])
		if v < 0 {
			return "", nil, errors.New("bech32: invalid character")
		}
		values = append(values, byte(v))
	}
	if polymod(append(hrpExpand(hrp), values...)) != 1 {
		return "", nil, errors.New("bech32: invalid checksum")
	}
	data, err = convertBits(values[:len(values)-6], 5, 8, false)
	return hrp, data, err
}

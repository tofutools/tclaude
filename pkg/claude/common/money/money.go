// Package money renders USD figures the way the dashboard does, so the
// console, the status bar and the web UI all spell a dollar amount the same
// way.
//
// Amounts are billed in USD whatever the operator's locale is, so the
// separators follow the currency rather than the host: a four-figure total
// reads "$26,222.38", never "26 222,38 $".
package money

import (
	"fmt"
	"strings"
)

// USD formats a dollar figure to the cent. Real spend that would round to
// $0.00 reads "<1¢" rather than as free; nothing spent at all is a plain
// $0.00, matching the dashboard's fmtUSD.
func USD(usd float64) string {
	if !(usd > 0) {
		return "$0.00"
	}
	if usd < 0.005 {
		return "<1¢"
	}
	return "$" + group(fmt.Sprintf("%.2f", usd))
}

// USDExact spells the figure out to four decimals without the sub-cent
// collapse, for tooltips and other places that want the unrounded amount.
func USDExact(usd float64) string {
	if !(usd > 0) {
		usd = 0
	}
	return "$" + group(fmt.Sprintf("%.4f", usd))
}

// group inserts thousands separators into the integer part of a plain
// fixed-point decimal string.
func group(s string) string {
	sign := ""
	if rest, ok := strings.CutPrefix(s, "-"); ok {
		sign, s = "-", rest
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	var b strings.Builder
	b.WriteString(sign)
	for i := range len(whole) {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(whole[i])
	}
	if hasFrac {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}

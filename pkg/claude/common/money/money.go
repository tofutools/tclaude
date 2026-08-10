// Package money renders USD figures the way the dashboard does, so the
// console, the status bar and the web UI all spell a dollar amount the same
// way.
//
// Amounts are billed in USD whatever the operator's locale is, so the
// separators follow the currency rather than the host: a four-figure total
// reads "$26,222", never "26 222 $".
package money

import (
	"fmt"
	"math"
	"strings"
)

// centsBelow is where the cents stop being worth their two digits: from three
// figures up, nobody reconciles a spend total to the penny. Kept in step with
// the dashboard's own CENTS_BELOW.
const centsBelow = 100

// USD formats a dollar figure to the cent below $100 and whole above it. Real
// spend that would round to $0.00 reads "<1¢" rather than as free; nothing
// spent at all is a plain $0.00, matching the dashboard's fmtUSD.
func USD(usd float64) string {
	if !(usd > 0) {
		return "$0.00"
	}
	// The branch follows the figure as written, not the raw float: $99.999
	// is written "$100.00", which belongs with the whole dollars rather than
	// sitting in a column of them wearing cents.
	//
	// math.Round, not %.0f: Go rounds a half to even and the dashboard's Intl
	// formatter rounds it away from zero, so $102.50 would otherwise read
	// $102 in the terminal and $103 in the browser off one payload.
	if math.Round(usd*100)/100 >= centsBelow {
		return "$" + group(fmt.Sprintf("%.0f", math.Round(usd)))
	}
	if usd < 0.005 {
		return "<1¢"
	}
	return "$" + group(fmt.Sprintf("%.2f", usd))
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

package db

import "strings"

// Rank keys for the group task board: lexicographic strings over a base-36
// digit alphabet ordered the way SQLite's BINARY collation orders bytes, so
// ORDER BY rank is the operator's order. A move writes one key between its two
// neighbours; nothing is renumbered. A key never ends in the lowest digit, so
// there is always room below it.

const taskRankDigits = "0123456789abcdefghijklmnopqrstuvwxyz"

const taskRankBase = len(taskRankDigits)

func taskRankDigit(c byte) int { return strings.IndexByte(taskRankDigits, c) }

// taskRankBetween returns a key strictly between a and b. "" for a is the
// lowest bound and "" for b the highest; when both are set a must be < b.
func taskRankBetween(a, b string) string {
	if b != "" {
		// Carry the common prefix (a padded with the lowest digit).
		n := 0
		for n < len(b) {
			ca := taskRankDigits[0]
			if n < len(a) {
				ca = a[n]
			}
			if ca != b[n] {
				break
			}
			n++
		}
		if n > 0 {
			rest := ""
			if n < len(a) {
				rest = a[n:]
			}
			return b[:n] + taskRankBetween(rest, b[n:])
		}
	}
	da := 0
	if a != "" {
		da = taskRankDigit(a[0])
	}
	db := taskRankBase
	if b != "" {
		db = taskRankDigit(b[0])
	}
	if db-da > 1 {
		return string(taskRankDigits[(da+db)/2])
	}
	// Adjacent first digits: b's first digit alone is above a and, when b is
	// longer, below b.
	if b != "" && len(b) > 1 {
		return b[:1]
	}
	rest := ""
	if len(a) > 1 {
		rest = a[1:]
	}
	return string(taskRankDigits[da]) + taskRankBetween(rest, "")
}

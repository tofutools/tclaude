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

// taskRankMaxLen is the longest key a move writes as it is. Keys between the
// same two neighbours grow by about a digit every few moves (a run of "move
// up" clicks), so a longer one renumbers its stage instead.
const taskRankMaxLen = 24

// renumberStageRanksTx gives every task of a group's stage, closed ones too
// (a reopen puts a task back at its rank), a short, evenly spaced key in its
// present order, in the caller's transaction. Writes are serialised (the
// database's transactions are immediate), so no concurrent move sees a half
// renumbered stage.
func renumberStageRanksTx(q taskTx, groupID int64, stage string) error {
	rows, err := q.Query(`SELECT id FROM group_tasks WHERE group_id = ? AND stage = ? ORDER BY rank, id`, groupID, stage)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, key := range spacedTaskRanks(len(ids)) {
		if _, err := q.Exec(`UPDATE group_tasks SET rank = ? WHERE id = ?`, key, ids[i]); err != nil {
			return err
		}
	}
	return nil
}

// spacedTaskRanks returns n increasing keys spread evenly over the shortest
// width that leaves room (at least about 35 free keys) between each two.
func spacedTaskRanks(n int) []string {
	width, space := 1, taskRankBase
	for space < (n+1)*taskRankBase {
		width++
		space *= taskRankBase
	}
	step := space / (n + 1)
	keys := make([]string, n)
	for i := range keys {
		v := (i + 1) * step
		b := make([]byte, width)
		for j := width - 1; j >= 0; j-- {
			b[j] = taskRankDigits[v%taskRankBase]
			v /= taskRankBase
		}
		// A key never ends in the lowest digit; trimming keeps the order,
		// since a shorter key sorts as if padded with that digit.
		keys[i] = strings.TrimRight(string(b), taskRankDigits[:1])
	}
	return keys
}

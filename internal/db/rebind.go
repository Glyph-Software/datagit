package db

import (
	"strconv"
	"strings"
)

// Rebind rewrites PostgreSQL $N placeholders into the ? form MySQL uses, and
// reports the argument order the rewritten statement expects.
//
// The reordering matters and is easy to miss. `$1` may appear after `$2`, or
// more than once; `?` is strictly positional and cannot repeat. So the returned
// order is a mapping to apply to the caller's arguments, and a repeated $N
// yields a repeated argument rather than an error.
//
// Text inside single-quoted string literals is left alone: a `$` there is data,
// not a placeholder. Doubled quotes (” inside a literal) are handled, because
// that is how a literal escapes a quote in both engines.
func Rebind(sql string) (string, []int) {
	var b strings.Builder
	b.Grow(len(sql))
	var order []int

	inString := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case inString:
			b.WriteByte(c)
			if c == '\'' {
				// A doubled quote is an escaped quote, not the end of the literal.
				if i+1 < len(sql) && sql[i+1] == '\'' {
					b.WriteByte('\'')
					i++
				} else {
					inString = false
				}
			}
		case c == '\'':
			inString = true
			b.WriteByte(c)
		case c == '$' && i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9':
			j := i + 1
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			n, _ := strconv.Atoi(sql[i+1 : j])
			order = append(order, n)
			b.WriteByte('?')
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), order
}

// Reorder applies a Rebind order to an argument list.
func Reorder(args []any, order []int) []any {
	if len(order) == 0 {
		return args
	}
	out := make([]any, 0, len(order))
	for _, n := range order {
		if n >= 1 && n <= len(args) {
			out = append(out, args[n-1])
		} else {
			// Out of range means the statement and its arguments disagree. Passing
			// nil lets the driver report the mismatch with its own message rather
			// than panicking here with less context.
			out = append(out, nil)
		}
	}
	return out
}

// QuoteToBacktick rewrites "double-quoted" identifiers into `backticked` ones.
//
// MySQL only accepts double quotes as identifier quoting under ANSI_QUOTES,
// which is not the default and which DataGit does not set: changing a global SQL
// mode on a database DataGit does not own is exactly the kind of side effect the
// design refuses (§2). So the identifiers are rewritten instead.
//
// Single-quoted literals are skipped for the same reason as in Rebind.
func QuoteToBacktick(sql string) string {
	if !strings.Contains(sql, `"`) {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql))
	inString, inIdent := false, false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case inString:
			b.WriteByte(c)
			if c == '\'' {
				if i+1 < len(sql) && sql[i+1] == '\'' {
					b.WriteByte('\'')
					i++
				} else {
					inString = false
				}
			}
		case inIdent:
			if c == '"' {
				// A doubled quote inside an identifier is a literal quote; a
				// backticked identifier escapes it as a doubled backtick.
				if i+1 < len(sql) && sql[i+1] == '"' {
					b.WriteString("``")
					i++
				} else {
					b.WriteByte('`')
					inIdent = false
				}
			} else if c == '`' {
				b.WriteString("``")
			} else {
				b.WriteByte(c)
			}
		case c == '\'':
			inString = true
			b.WriteByte(c)
		case c == '"':
			inIdent = true
			b.WriteByte('`')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

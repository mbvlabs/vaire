package render

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// op is one diff line: ' ' context, '-' only in the current file, '+' only in
// the wanted bytes.
type op struct {
	kind byte
	line string
	old  int
	new  int
}

// Diff returns a unified diff between the file at path and want. A missing
// file is treated as empty. Secrets are redacted by the caller.
func Diff(path string, want []byte) string {
	have, err := os.ReadFile(path) //nolint:gosec // G304: managed file path from the step catalogue.
	if err != nil && !os.IsNotExist(err) {
		return ""
	}
	if bytes.Equal(have, want) {
		return ""
	}
	ops := diffOps(splitLines(have), splitLines(want))
	numberOps(ops)
	return formatDiff(path, ops)
}

// DiffBytes returns a unified diff from have to want, labelled name.
func DiffBytes(name string, have, want []byte) string {
	if bytes.Equal(have, want) {
		return ""
	}
	ops := diffOps(splitLines(have), splitLines(want))
	numberOps(ops)
	return formatDiff(name, ops)
}

// splitLines splits content into lines without their trailing newline. A
// trailing newline does not produce an empty final line.
func splitLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	s := string(raw)
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// diffOps computes a line diff with an LCS table.
func diffOps(a, b []string) []op {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{kind: ' ', line: a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, op{kind: '-', line: a[i]})
			i++
		default:
			ops = append(ops, op{kind: '+', line: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{kind: '-', line: a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{kind: '+', line: b[j]})
	}
	return ops
}

// numberOps fills each op's old and new line numbers.
func numberOps(ops []op) {
	oldLine, newLine := 1, 1
	for i := range ops {
		if ops[i].kind != '+' {
			ops[i].old = oldLine
			oldLine++
		}
		if ops[i].kind != '-' {
			ops[i].new = newLine
			newLine++
		}
	}
}

// formatDiff groups ops into hunks with 3 lines of context.
func formatDiff(path string, ops []op) string {
	const ctx = 3
	var b strings.Builder
	fmt.Fprintf(&b, "--- %s\n+++ %s (eru)\n", path, path)
	i := 0
	for i < len(ops) {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := maxInt(i-ctx, 0)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			k := end
			for k < len(ops) && ops[k].kind == ' ' {
				k++
			}
			if k < len(ops) && k-end <= ctx {
				end = k
				continue
			}
			break
		}
		end = minInt(end+ctx, len(ops))
		writeHunk(&b, ops[start:end])
		i = end
	}
	return b.String()
}

func writeHunk(b *strings.Builder, hunk []op) {
	oldStart, newStart := hunk[0].old, hunk[0].new
	if oldStart == 0 {
		oldStart = 1
	}
	if newStart == 0 {
		newStart = 1
	}
	oldCount, newCount := 0, 0
	for _, o := range hunk {
		if o.kind != '+' {
			oldCount++
		}
		if o.kind != '-' {
			newCount++
		}
	}
	fmt.Fprintf(b, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	for _, o := range hunk {
		b.WriteByte(o.kind)
		b.WriteString(o.line)
		b.WriteByte('\n')
	}
}

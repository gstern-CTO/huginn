package content

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

// newLineScanner returns a scanner sized for source files, which routinely
// exceed bufio's default 64KB line limit in minified or generated code.
func NewLineScanner(r io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return scanner
}

// sliceLines extracts a 1-based inclusive line range. A zero start or end means
// "from the beginning" / "to the end". It reports whether the result is a
// partial view of the input.
func SliceLines(content string, start, end int) (string, bool, *protocol.ToolError) {
	if start == 0 && end == 0 {
		return content, false, nil
	}
	if start < 0 || end < 0 {
		return "", false, protocol.ErrInvalidInput("line numbers must be positive")
	}
	if start > 0 && end > 0 && end < start {
		return "", false, protocol.ErrInvalidInput("endLine (%d) must not be before startLine (%d)", end, start)
	}

	lines := strings.Split(content, "\n")
	total := len(lines)

	from := start
	if from < 1 {
		from = 1
	}
	if from > total {
		return "", false, protocol.NewError(protocol.CodeInvalidInput, false,
			"Request a line range within the file, or omit the range to read from the start.",
			"startLine %d is past the end of the file (%d lines)", start, total).
			WithDetail("totalLines", total)
	}
	to := end
	if to < 1 || to > total {
		to = total
	}

	partial := from > 1 || to < total
	return strings.Join(lines[from-1:to], "\n"), partial, nil
}

// truncateToTokens cuts s to approximately maxTokens, preferring a line
// boundary so the output stays readable, and marks the cut so the caller can
// see the content is incomplete.
func TruncateToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 || protocol.EstimateTokens(s) <= maxTokens {
		return s
	}
	limit := maxTokens * 4
	if limit >= len(s) {
		return s
	}
	cut := s[:limit]
	if idx := strings.LastIndexByte(cut, '\n'); idx > limit/2 {
		cut = cut[:idx]
	}
	return cut + "\n... [truncated to fit the response token budget]"
}

// readAllLimited reads at most limit bytes, so a large remote object cannot be
// pulled entirely into memory.
func ReadAllLimited(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = defaultReadLimit
	}
	var buf bytes.Buffer
	_, err := io.Copy(&buf, io.LimitReader(r, limit))
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// looksBinary reports whether a byte sample is non-text. A NUL byte is the
// decisive signal; otherwise a high proportion of non-UTF-8 or control bytes
// indicates binary content.
func LooksBinary(sample []byte) bool {
	if len(sample) == 0 {
		return false
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	if !utf8.Valid(sample) {
		return true
	}
	suspicious := 0
	for _, b := range sample {
		if b < 0x09 || (b > 0x0d && b < 0x20) || b == 0x7f {
			suspicious++
		}
	}
	return suspicious*100/len(sample) > 5
}

// defaultReadLimit bounds an unbounded read when the caller supplies no limit.
const defaultReadLimit = 8 << 20

// isBinaryFile samples the head of a file to decide whether it is text.
func IsBinaryFile(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sample := make([]byte, 8000)
	n, err := f.Read(sample)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return LooksBinary(sample[:n]), nil
}

func ReadFileLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadAllLimited(f, limit)
}

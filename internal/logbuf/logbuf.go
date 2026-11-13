// Package logbuf is the log writer: zerolog events formatted as readable
// lines, written to an output (stdout) and kept in a ring buffer for the
// status page. Known secrets are masked in both.
package logbuf

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Size is the default number of lines kept.
const Size = 5000

// Buffer is a zerolog writer. It is safe for concurrent use.
type Buffer struct {
	out io.Writer

	mu      sync.Mutex
	lines   []string
	next    int // ring position of the next line
	full    bool
	masker  *strings.Replacer
	console zerolog.ConsoleWriter
	tmp     bytes.Buffer
}

// New keeps the last size lines (Size if size <= 0) and copies every line to
// out (may be nil).
func New(out io.Writer, size int) *Buffer {
	if size <= 0 {
		size = Size
	}
	b := &Buffer{out: out, lines: make([]string, size)}
	b.console = zerolog.ConsoleWriter{
		Out:        &b.tmp,
		NoColor:    true,
		TimeFormat: time.DateTime,
	}
	return b
}

// SetSecrets replaces the values masked in every following line.
func (b *Buffer) SetSecrets(secrets []string) {
	var pairs []string
	seen := map[string]bool{}
	add := func(s string) {
		if len(s) >= 3 && !seen[s] {
			seen[s] = true
			pairs = append(pairs, s, "***")
		}
	}
	for _, s := range secrets {
		add(s)
		add(url.QueryEscape(s))
		add(url.PathEscape(s))
		add(url.UserPassword("x", s).String()[2:]) // userinfo escaping
		// ConsoleWriter quotes values with spaces, quotes, backslashes or
		// control characters (strconv.Quote); raw JSON (non-JSON passthrough,
		// nested objects) has the JSON escaping.
		add(unquote(strconv.Quote(s)))
		add(unquote(strconv.QuoteToASCII(s)))
		add(jsonEscape(s, true))
		add(jsonEscape(s, false))
	}
	// Longer patterns first: at one position strings.Replacer takes the first
	// matching pair, and a secret's prefix must not leave the rest visible.
	type pair struct{ from, to string }
	ps := make([]pair, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		ps = append(ps, pair{pairs[i], pairs[i+1]})
	}
	sort.SliceStable(ps, func(i, j int) bool { return len(ps[i].from) > len(ps[j].from) })
	pairs = pairs[:0]
	for _, p := range ps {
		pairs = append(pairs, p.from, p.to)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(pairs) == 0 {
		b.masker = nil
	} else {
		b.masker = strings.NewReplacer(pairs...)
	}
}

// unquote strips the outer quotes of a quoted string.
func unquote(q string) string { return q[1 : len(q)-1] }

// jsonEscape is s as it appears inside a JSON string.
func jsonEscape(s string, html bool) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(html)
	if enc.Encode(s) != nil {
		return s
	}
	return unquote(strings.TrimRight(buf.String(), "\n"))
}

// Write takes one zerolog event (JSON). Non-JSON input is kept as is.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.tmp.Reset()
	if _, err := b.console.Write(p); err != nil || b.tmp.Len() == 0 {
		b.tmp.Reset()
		b.tmp.Write(p)
	}
	text := strings.TrimRight(b.tmp.String(), "\n")
	if b.masker != nil {
		text = b.masker.Replace(text)
	}
	for _, line := range strings.Split(text, "\n") {
		b.lines[b.next] = line
		b.next = (b.next + 1) % len(b.lines)
		if b.next == 0 {
			b.full = true
		}
	}
	if b.out != nil {
		_, _ = io.WriteString(b.out, text+"\n")
	}
	return len(p), nil
}

// Lines returns up to n of the newest lines, oldest first (all if n <= 0).
func (b *Buffer) Lines(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := b.next
	if b.full {
		count = len(b.lines)
	}
	if n <= 0 || n > count {
		n = count
	}
	out := make([]string, 0, n)
	for i := count - n; i < count; i++ {
		idx := i
		if b.full {
			idx = (b.next + i) % len(b.lines)
		}
		out = append(out, b.lines[idx])
	}
	return out
}

package logbuf

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestRing(t *testing.T) {
	var out bytes.Buffer
	b := New(&out, 3)
	log := zerolog.New(b)
	for i := range 5 {
		log.Info().Msgf("line %d", i)
	}
	lines := b.Lines(0)
	require.Len(t, lines, 3)
	require.Contains(t, lines[0], "line 2")
	require.Contains(t, lines[2], "line 4")
	require.Contains(t, lines[2], "INF")

	last := b.Lines(1)
	require.Len(t, last, 1)
	require.Contains(t, last[0], "line 4")
	require.Contains(t, out.String(), "line 0")
}

func TestPartial(t *testing.T) {
	b := New(nil, 10)
	l := zerolog.New(b)
	l.Warn().Msg("only")
	require.Len(t, b.Lines(5), 1)
}

func TestSecretsMasked(t *testing.T) {
	var out bytes.Buffer
	b := New(&out, 10)
	b.SetSecrets([]string{"p@ss word", "token123"})
	log := zerolog.New(b)
	log.Info().Str("url", "reolink://admin:p%40ss%20word@host/main").Msg("dial token123")
	log.Info().Msg("raw p@ss word")
	for _, l := range append(b.Lines(0), out.String()) {
		require.NotContains(t, l, "token123")
		require.NotContains(t, l, "p@ss word")
		require.NotContains(t, l, "p%40ss%20word")
	}
}

// ConsoleWriter quotes values with quotes, backslashes or control characters;
// the escaped form must be masked too, also in raw (non-JSON) lines.
func TestSecretsMaskedWhenQuoted(t *testing.T) {
	secrets := []string{`pa"ss\w0rd`, "tab\there", "html<&>pw", "ümläut pw"}
	var out bytes.Buffer
	b := New(&out, 20)
	b.SetSecrets(secrets)
	log := zerolog.New(b)
	for _, s := range secrets {
		log.Info().Str("password", s).Msg("field")
		log.Info().Err(errors.New("dial: " + s)).Msg("error")
		log.Info().Interface("nested", map[string]string{"pw": s}).Msg("nested")
		log.Info().Msg("message " + s)
		raw, _ := json.Marshal(map[string]string{"pw": s})
		_, _ = b.Write(append([]byte("not json "), raw...))
	}
	all := append(b.Lines(0), out.String())
	for _, s := range secrets {
		q := strconv.Quote(s)
		j, _ := json.Marshal(s)
		for _, l := range all {
			require.NotContains(t, l, s)
			require.NotContains(t, l, q[1:len(q)-1])
			require.NotContains(t, l, string(j[1:len(j)-1]))
		}
	}
	require.Contains(t, out.String(), "***")
}

func TestSecretPrefixNotLeaked(t *testing.T) {
	b := New(nil, 10)
	b.SetSecrets([]string{"abc", "abc!def"})
	l := zerolog.New(b)
	l.Info().Msg("x abc!def y")
	require.NotContains(t, b.Lines(0)[0], "def")
}

package logbuf

import (
	"bytes"
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

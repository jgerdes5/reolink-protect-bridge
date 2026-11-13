package baichuan

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const ledResponse = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<LedState version="1.1">
<channelId>0</channelId>
<ledVersion>2</ledVersion>
<state>auto</state>
<lightState>open</lightState>
<doorbellLightState>open</doorbellLightState>
<futureField a="1">x &amp; y</futureField>
</LedState>
</body>`

func TestNodeRoundTrip(t *testing.T) {
	body, err := ParseNode(ledResponse)
	require.NoError(t, err)
	require.Equal(t, "body", body.Name)
	require.Equal(t, "open", body.Value("LedState/doorbellLightState"))
	require.Equal(t, "open", body.Value("ledstate/DOORBELLLIGHTSTATE")) // case-insensitive like the camera

	require.True(t, body.Set("LedState/doorbellLightState", "keepOff"))
	require.False(t, body.Set("LedState/doorbellLightState", "keepOff")) // unchanged
	require.False(t, body.Set("LedState/missing", "1"))                  // not created

	out := string(body.Marshal())
	require.True(t, strings.HasPrefix(out, `<?xml version="1.0" encoding="UTF-8" ?>`))
	require.Contains(t, out, `<LedState version="1.1">`)
	require.Contains(t, out, `<doorbellLightState>keepOff</doorbellLightState>`)
	require.Contains(t, out, `<futureField a="1">x &amp; y</futureField>`) // unknown elements survive

	again, err := ParseNode(out)
	require.NoError(t, err)
	require.Equal(t, "keepOff", again.Value("LedState/doorbellLightState"))
	require.Equal(t, "2", again.Value("LedState/ledVersion"))
}

func TestArea(t *testing.T) {
	cols, rows := 96, 64
	cells := make([]bool, cols*rows)
	for i := range cells {
		cells[i] = true
	}
	full := EncodeArea(cells, cols, rows)
	require.Equal(t, strings.Repeat("////", 256), full) // spec: 768 bytes of 0xFF

	cells = make([]bool, 10)
	cells[0], cells[9] = true, true // MSB first, padded
	s := EncodeArea(cells, 5, 2)
	back, err := DecodeArea(s, 5, 2)
	require.NoError(t, err)
	require.Equal(t, cells, back)
	require.Equal(t, "gEA=", s) // 1000 0000 | 0100 0000

	_, err = DecodeArea("gA==", 96, 64)
	require.Error(t, err)
}

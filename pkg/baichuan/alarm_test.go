package baichuan

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAlarms(t *testing.T) {
	events := ParseAlarms(`<?xml version="1.0" encoding="UTF-8" ?>
<body><AlarmEventList>
<AlarmEvent version="1.1"><channelId>0</channelId><status>MD,visitor</status><recording>0</recording><timeStamp>0</timeStamp><AItype>people,dog_cat</AItype></AlarmEvent>
<AlarmEvent version="1.1"><channelId>1</channelId><status>none</status><AItype>none</AItype></AlarmEvent>
<AlarmEvent version="1.1"><status>MD</status></AlarmEvent>
</AlarmEventList></body>`)
	require.Equal(t, []AlarmEvent{
		{Channel: 0, Motion: true, Visitor: true, AI: []string{"people", "dog_cat"}},
		{Channel: 1},
	}, events)

	// AI "other" counts as motion; unrelated event lists are ignored
	require.Equal(t, []AlarmEvent{{Channel: 0, Motion: true}},
		ParseAlarms(`<body><AlarmEventList><AlarmEvent><channelId>0</channelId><status>none</status><AItype>other</AItype></AlarmEvent></AlarmEventList></body>`))
	require.Empty(t, ParseAlarms(`<body><DayNightEventList><DayNightEvent><channelId>0</channelId><mode>day</mode></DayNightEvent></DayNightEventList></body>`))
	require.Empty(t, ParseAlarms(`not xml`))
}

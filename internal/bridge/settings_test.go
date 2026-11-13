package bridge

import (
	"testing"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/stretchr/testify/require"
)

// Response layouts as the D340W firmware builds them (libnetpublic *_s2x).
const imageXML = `<?xml version="1.0" encoding="UTF-8" ?>
<body>
<VideoInput version="1.1"><channelId>0</channelId><bright>128</bright><contrast>128</contrast><saturation>128</saturation><hue>128</hue><sharpen>128</sharpen></VideoInput>
<InputAdvanceCfg version="1.1"><channelId>0</channelId>
<PowerLineFrequency><mode>50hz</mode><enable>0</enable></PowerLineFrequency>
<DayNight><mode>auto</mode><IrcutMode>auto</IrcutMode><Threshold>medium</Threshold></DayNight>
<mirror>0</mirror><flip>0</flip>
<nr3d><value>medium</value><enable>1</enable></nr3d>
<hdrSwitch>0</hdrSwitch>
</InputAdvanceCfg>
</body>`

const ledXML = `<body><LedState version="1.1"><channelId>0</channelId><ledVersion>2</ledVersion><state>auto</state><lightState>open</lightState><doorbellLightState>open</doorbellLightState></LedState></body>`

const audioXML = `<body><audioCfg version="1.1"><channelId>0</channelId><timeout>0</timeout><audioSelect>0</audioSelect><volume>80</volume><talkAndReplyVolume>80</talkAndReplyVolume><visitorVolume>80</visitorVolume><preAlarm>0</preAlarm><visitorLoudspeaker>1</visitorLoudspeaker></audioCfg></body>`

const mdXML = `<body><MD version="1.1"><channelId>0</channelId><enable>1</enable><usepir>0</usepir><width>96</width><height>64</height>
<scope><columns>96</columns><rows>64</rows><valueTable>AAAA</valueTable></scope>
<sensInfoNew><sensitivityDefault>10</sensitivityDefault></sensInfoNew></MD></body>`

const aiXML = `<body><AiDetectCfg version="1.1"><chn>0</chn><type>people</type><sensitivity>50</sensitivity><stayTime>0</stayTime><width>4</width><height>2</height><area>AA==</area></AiDetectCfg></body>`

func parse(t *testing.T, s string) *baichuan.Node {
	n, err := baichuan.ParseNode(s)
	require.NoError(t, err)
	return n
}

func iptr(v int) *int       { return &v }
func sptr(v string) *string { return &v }

func TestImageChange(t *testing.T) {
	b := parse(t, imageXML)
	isp := &unifi.ISP{
		Brightness: iptr(50), Contrast: iptr(100), Saturation: iptr(0), Mirror: iptr(1),
		Denoise: iptr(80), AeMode: sptr("flick60"),
		IrLedMode: sptr("manual"), IrLedLevel: iptr(255), // night vision on
	}
	require.True(t, imageChange(isp, "2")(b))
	require.Equal(t, "128", b.Value("VideoInput/bright"))
	require.Equal(t, "255", b.Value("VideoInput/contrast"))
	require.Equal(t, "0", b.Value("VideoInput/saturation"))
	require.Equal(t, "128", b.Value("VideoInput/hue")) // not sent: untouched
	require.Equal(t, "1", b.Value("InputAdvanceCfg/mirror"))
	require.Equal(t, "high", b.Value("InputAdvanceCfg/nr3d/value"))
	require.Equal(t, "1", b.Value("InputAdvanceCfg/PowerLineFrequency/enable"))
	require.Equal(t, "60hz", b.Value("InputAdvanceCfg/PowerLineFrequency/mode"))
	require.Equal(t, "blackAndWhite", b.Value("InputAdvanceCfg/DayNight/mode"))
	require.Equal(t, "2", b.Value("InputAdvanceCfg/hdrSwitch"))

	// Protect's defaults on an unchanged doorbell: nothing to write
	b = parse(t, imageXML)
	require.False(t, imageChange(&unifi.ISP{Brightness: iptr(50), Mirror: iptr(0), AeMode: sptr("auto")}, "")(b))
}

func TestHDRSwitch(t *testing.T) {
	require.Equal(t, "0", hdrSwitch(false, "superHdr"))
	require.Equal(t, "1", hdrSwitch(true, "normal"))
	require.Equal(t, "2", hdrSwitch(true, "superHdr"))

	s := &settings{}
	s.ISP(unifi.ISP{HdrMode: sptr("superHdr")})
	require.False(t, s.hdrDirty) // HDR switch unknown yet: don't touch the doorbell
	s.HDR(true)
	require.True(t, s.hdrDirty)
	require.Equal(t, "2", hdrSwitch(*s.hdrOn, s.hdrMode))
}

func TestLedChange(t *testing.T) {
	b := parse(t, ledXML)
	off := &unifi.ISP{IrLedMode: sptr("manual"), IrLedLevel: iptr(0)}
	require.True(t, ledChange(off, &unifi.SoundLed{LedFaceEnabled: iptr(0)})(b))
	require.Equal(t, "close", b.Value("LedState/state"))
	require.Equal(t, "keepOff", b.Value("LedState/doorbellLightState"))

	// status light back on: Reolink default (lit on events), not the old value
	require.True(t, ledChange(nil, &unifi.SoundLed{LedFaceEnabled: iptr(1)})(b))
	require.Equal(t, "close", b.Value("LedState/doorbellLightState"))

	// on while the user chose keepOn in the Reolink app: keep it
	b.Set("LedState/doorbellLightState", "keepOn")
	require.False(t, ledChange(nil, &unifi.SoundLed{LedFaceEnabled: iptr(1)})(b))
}

func TestAudioChange(t *testing.T) {
	b := parse(t, audioXML)
	require.True(t, audioChange(&unifi.SoundLed{SpeakerVolume: iptr(60), RingVolume: iptr(0)})(b))
	require.Equal(t, "60", b.Value("audioCfg/volume"))
	require.Equal(t, "60", b.Value("audioCfg/talkAndReplyVolume"))
	require.Equal(t, "80", b.Value("audioCfg/visitorVolume")) // volume kept, ringtone off
	require.Equal(t, "0", b.Value("audioCfg/visitorLoudspeaker"))
	require.Equal(t, "0", b.Value("audioCfg/preAlarm")) // untouched

	require.True(t, audioChange(&unifi.SoundLed{RingVolume: iptr(30)})(b))
	require.Equal(t, "30", b.Value("audioCfg/visitorVolume"))
	require.Equal(t, "1", b.Value("audioCfg/visitorLoudspeaker"))
}

func TestMotionChange(t *testing.T) {
	b := parse(t, mdXML)
	left := unifi.Zone{Coord: []float64{0, 0, 500, 0, 500, 1000, 0, 1000}, Level: 30} // sensitivity 70
	require.True(t, motionChange(&unifi.Motion{Enable: new(bool), Zones: map[string]unifi.Zone{"1": left}})(b))
	require.Equal(t, "0", b.Value("MD/enable"))
	require.Equal(t, "16", b.Value("MD/sensInfoNew/sensitivityDefault")) // ui 1+round(34.3)=35, wire 51-35
	cells, err := baichuan.DecodeArea(b.Value("MD/scope/valueTable"), 96, 64)
	require.NoError(t, err)
	require.True(t, cells[0])         // top-left
	require.True(t, cells[47])        // last column of the left half
	require.False(t, cells[48])       // right half
	require.True(t, cells[63*96+10])  // bottom row, left
	require.False(t, cells[63*96+95]) // bottom row, right
}

func TestAIAreaChange(t *testing.T) {
	b := parse(t, aiXML) // 4x2 grid
	top := unifi.Zone{Coord: []float64{0, 0, 1000, 0, 1000, 500, 0, 500}, Sensitivity: 75, ObjectTypes: []string{"person"}}
	require.True(t, aiAreaChange([]unifi.Zone{top}, 75)(b))
	require.Equal(t, "75", b.Value("AiDetectCfg/sensitivity"))
	cells, err := baichuan.DecodeArea(b.Value("AiDetectCfg/area"), 4, 2)
	require.NoError(t, err)
	require.Equal(t, []bool{true, true, true, true, false, false, false, false}, cells)

	// no zone with this type: empty area, sensitivity kept; on an already
	// empty grid that is no change, so nothing is written
	b = parse(t, aiXML)
	require.False(t, aiAreaChange(nil, -1)(b))
	require.Equal(t, "50", b.Value("AiDetectCfg/sensitivity"))
	require.Equal(t, "AA==", b.Value("AiDetectCfg/area"))
}

func TestMappings(t *testing.T) {
	require.Equal(t, 128, scale255(50))
	require.Equal(t, 0, scale255(-5))
	require.Equal(t, 255, scale255(100))
	require.Equal(t, 50, mdSensitivity(0)) // least sensitive
	require.Equal(t, 1, mdSensitivity(100))
	require.Equal(t, "auto", dayNightMode("custom"))
	require.Equal(t, "color", dayNightMode("off"))
	require.Equal(t, "", irState(""))
	require.True(t, inPolygon([]float64{0, 0, 10, 0, 10, 10, 0, 10}, 5, 5))
	require.False(t, inPolygon([]float64{0, 0, 10, 0, 10, 10, 0, 10}, 15, 5))
	require.False(t, inPolygon([]float64{0, 0, 10, 0}, 5, 0)) // a line is no area
}

func TestSmartEnabled(t *testing.T) {
	s := &settings{}
	require.True(t, s.smartEnabled("vehicle")) // nothing from Protect yet: all on
	s.SmartDetect(unifi.SmartDetect{Enabled: []string{"person"}})
	require.True(t, s.smartEnabled("person"))
	require.False(t, s.smartEnabled("vehicle"))
}

package bridge

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/stretchr/testify/require"
)

// Response layouts as the D340W firmware builds them.
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
	require.Equal(t, "1", b.Value("MD/enable"))                          // Protect's enable is the enhanced algorithm, not motion detection
	require.Equal(t, "16", b.Value("MD/sensInfoNew/sensitivityDefault")) // ui 1+round(34.3)=35, wire 51-35
	cells, err := baichuan.DecodeArea(b.Value("MD/scope/valueTable"), 96, 64)
	require.NoError(t, err)
	require.True(t, cells[0])         // top-left
	require.True(t, cells[47])        // last column of the left half
	require.False(t, cells[48])       // right half
	require.True(t, cells[63*96+10])  // bottom row, left
	require.False(t, cells[63*96+95]) // bottom row, right

	// no zones: the doorbell's area stays
	before := b.Value("MD/scope/valueTable")
	require.False(t, motionChange(&unifi.Motion{})(b))
	require.Equal(t, before, b.Value("MD/scope/valueTable"))
}

// The D340W's grid: an 80x60 picture in a 96x64 table. A full-frame zone
// gives exactly the table the doorbell itself stores for "everything".
func TestMotionChangeD340W(t *testing.T) {
	row := append(bytes.Repeat([]byte{0xff}, 10), 0, 0)
	want := append(bytes.Repeat(row, 60), make([]byte, 4*12)...)
	xml := strings.Replace(strings.Replace(mdXML, "<width>96</width>", "<width>80</width>", 1), "<height>64</height>", "<height>60</height>", 1)
	b := parse(t, xml)
	full := unifi.Zone{Coord: []float64{0, 0, 1000, 0, 1000, 1000, 0, 1000}, Level: 50}
	motionChange(&unifi.Motion{Zones: map[string]unifi.Zone{"1": full}})(b)
	require.Equal(t, base64.StdEncoding.EncodeToString(want), b.Value("MD/scope/valueTable"))
}

func TestAIAreaChange(t *testing.T) {
	b := parse(t, aiXML) // 4x2 grid
	top := unifi.Zone{Coord: []float64{0, 0, 1000, 0, 1000, 500, 0, 500}, Sensitivity: 75, ObjectTypes: []string{"person"}}
	require.True(t, aiAreaChange([]unifi.Zone{top}, nil, 75)(b))
	require.Equal(t, "75", b.Value("AiDetectCfg/sensitivity"))
	cells, err := baichuan.DecodeArea(b.Value("AiDetectCfg/area"), 4, 2)
	require.NoError(t, err)
	require.Equal(t, []bool{true, true, true, true, false, false, false, false}, cells)

	// no zone with this type: empty area, sensitivity kept; on an already
	// empty grid that is no change, so nothing is written
	b = parse(t, aiXML)
	require.False(t, aiAreaChange(nil, nil, -1)(b))
	require.Equal(t, "50", b.Value("AiDetectCfg/sensitivity"))
	require.Equal(t, "AA==", b.Value("AiDetectCfg/area"))
}

// Exclusion zones are cut out of the detection area.
func TestAIAreaExclude(t *testing.T) {
	b := parse(t, aiXML) // 4x2 grid
	full := unifi.Zone{Coord: []float64{0, 0, 1000, 0, 1000, 1000, 0, 1000}, ObjectTypes: []string{"person"}}
	left := unifi.Zone{Coord: []float64{0, 0, 500, 0, 500, 1000, 0, 1000}}
	require.True(t, aiAreaChange([]unifi.Zone{full}, []unifi.Zone{left}, -1)(b))
	cells, err := baichuan.DecodeArea(b.Value("AiDetectCfg/area"), 4, 2)
	require.NoError(t, err)
	require.Equal(t, []bool{false, false, true, true, false, false, true, true}, cells)
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

const encXML = `<body><Compression version="1.1"><channelId>0</channelId>
<mainStream><audio>1</audio><width>2560</width><height>1920</height><frame>20</frame><bitRate>6144</bitRate><gop><cur>10</cur><max>80</max><min>4</min></gop></mainStream>
<subStream><audio>1</audio><width>640</width><height>480</height><frame>15</frame><bitRate>512</bitRate><gop><cur>8</cur><max>12</max><min>4</min></gop></subStream>
</Compression></body>`

func TestGopChange(t *testing.T) {
	b := parse(t, encXML)
	require.True(t, gopChange(b))
	require.Equal(t, "20", b.Value("Compression/mainStream/gop/cur")) // 20 fps -> 1 s (from an old 0.5 s)
	require.Equal(t, "12", b.Value("Compression/subStream/gop/cur"))  // 15 fps, limited to the maximum 12
	require.Equal(t, "1", b.Value("Compression/mainStream/audio"))    // nothing else touched
	require.False(t, gopChange(b))
}

// The D340W counts the keyframe interval in seconds (main 1-2, sub 1-4): one
// second is 1, not the frame rate clamped to the maximum.
func TestGopChangeSeconds(t *testing.T) {
	xml := `<body><Compression version="1.1"><channelId>0</channelId>
<mainStream><frame>20</frame><gop><cur>2</cur><max>2</max><min>1</min></gop></mainStream>
<subStream><frame>15</frame><gop><cur>4</cur><max>4</max><min>1</min></gop></subStream>
</Compression></body>`
	b := parse(t, xml)
	require.True(t, gopChange(b))
	require.Equal(t, "1", b.Value("Compression/mainStream/gop/cur"))
	require.Equal(t, "1", b.Value("Compression/subStream/gop/cur"))
	require.False(t, gopChange(b))
}

const shelterXML = `<body><Shelter version="1.1"><channelId>0</channelId><enable>1</enable><shelterList>
<Shelter><id>0</id><enable>1</enable><layer>0</layer><color>0</color><topLeftX>6553600640</topLeftX><topLeftY>480</topLeftY><width>13107840</width><height>6553920</height></Shelter>
</shelterList></Shelter></body>`

func TestMaskChange(t *testing.T) {
	b := parse(t, `<body><Shelter version="1.1"><channelId>0</channelId><enable>0</enable><shelterList></shelterList></Shelter></body>`)
	masks := []unifi.Mask{
		{ID: 5, Coord: []float64{100, 200, 300, 200, 300, 400, 100, 400}},
		{ID: 9, Coord: []float64{0, 0, 250.00000000000003, 0, 250, 1000, 0, 1000}},
	}
	require.True(t, maskChange(masks)(b))
	require.Equal(t, "1", b.Value("Shelter/enable"))
	list := b.Find("Shelter/shelterList").Children
	require.Len(t, list, 4) // ids 0-3: two rectangles, two deleted
	first := list[0]
	require.Equal(t, "0", first.Value("id"))
	require.Equal(t, "1", first.Value("enable"))
	require.Equal(t, strconv.Itoa(100<<16|1000), first.Value("topLeftX"))
	require.Equal(t, strconv.Itoa(200<<16|1000), first.Value("width"))
	require.Equal(t, strconv.Itoa(251<<16|1000), list[1].Value("width")) // float edge rounded outwards
	require.Equal(t, "0", list[2].Value("enable"))
	require.Nil(t, list[2].Find("topLeftX"))

	// the doorbell now reports exactly these: nothing to write
	b = parse(t, string(b.Marshal()))
	for _, e := range b.Find("Shelter/shelterList").Children {
		if e.Value("enable") == "0" {
			b.Find("Shelter/shelterList").Children = b.Find("Shelter/shelterList").Children[:2]
			break
		}
	}
	require.False(t, maskChange(masks)(b))

	// no zones in Protect: masks off and deleted
	require.True(t, maskChange([]unifi.Mask{})(b))
	require.Equal(t, "0", b.Value("Shelter/enable"))

	// at most four
	many := make([]unifi.Mask, 6)
	for i := range many {
		many[i] = unifi.Mask{ID: i, Coord: []float64{0, 0, 10, 0, 10, 10}}
	}
	b = parse(t, shelterXML)
	require.True(t, maskChange(many)(b))
	require.Len(t, b.Find("Shelter/shelterList").Children, 4)
}

// Protect's Privacy Mode is a privacy zone over the whole frame: it becomes
// one mask over the whole picture, so the doorbell's video is black (its ring
// button and events keep working, unlike Reolink's sleep mode).
func TestPrivacyModeMask(t *testing.T) {
	b := parse(t, `<body><Shelter version="1.1"><channelId>0</channelId><enable>0</enable><shelterList></shelterList></Shelter></body>`)
	require.True(t, maskChange([]unifi.Mask{{ID: 1, Coord: []float64{0, 0, 1000, 0, 1000, 1000, 0, 1000}}})(b))
	require.Equal(t, "1", b.Value("Shelter/enable"))
	first := b.Find("Shelter/shelterList").Children[0]
	require.Equal(t, strconv.Itoa(0<<16|1000), first.Value("topLeftX"))
	require.Equal(t, strconv.Itoa(0<<16|1000), first.Value("topLeftY"))
	require.Equal(t, strconv.Itoa(1000<<16|1000), first.Value("width"))
	require.Equal(t, strconv.Itoa(1000<<16|1000), first.Value("height"))
}

const osdXML = `<body><OsdDatetime version="1.1"><channelId>0</channelId><enable>1</enable><topLeftX>1</topLeftX><topLeftY>65536</topLeftY><width>0</width><height>0</height><language>English</language></OsdDatetime>
<OsdChannelName version="1.1"><channelId>0</channelId><name>Doorbell</name><enable>1</enable><topLeftX>1</topLeftX><topLeftY>1</topLeftY><enWatermark>1</enWatermark><enBgcolor>0</enBgcolor></OsdChannelName></body>`

func TestOsdChange(t *testing.T) {
	b := parse(t, osdXML)
	require.True(t, osdChange(unifi.OSD{Name: "Haustür <Süd>", Date: false, Logo: false, Location: "topRight"})(b))
	require.Equal(t, "Haustuer Sued", b.Value("OsdChannelName/name"))
	require.Equal(t, "1", b.Value("OsdChannelName/enable"))
	require.Equal(t, "0", b.Value("OsdDatetime/enable"))
	require.Equal(t, "0", b.Value("OsdChannelName/enWatermark"))
	require.Equal(t, "65536", b.Value("OsdChannelName/topLeftX"))
	require.Equal(t, "1", b.Value("OsdChannelName/topLeftY"))
	require.Equal(t, "65536", b.Value("OsdDatetime/topLeftY")) // date at the other edge
	require.Equal(t, "English", b.Value("OsdDatetime/language"))

	// name off keeps the stored text
	require.True(t, osdChange(unifi.OSD{Name: "", Date: true, Logo: true, Location: "topRight"})(b))
	require.Equal(t, "0", b.Value("OsdChannelName/enable"))
	require.Equal(t, "Haustuer Sued", b.Value("OsdChannelName/name"))
	require.Equal(t, "0", b.Value("OsdChannelName/enWatermark")) // Protect's logo is not Reolink's
}

func TestOsdName(t *testing.T) {
	require.Equal(t, "Front Door", osdName("  Front Door "))
	require.Equal(t, "Strasse", osdName("Straße"))
	require.Equal(t, "ab", osdName("a$&*/:;<>?\\`|b"))
	require.Equal(t, "", osdName("🔔"))
	require.Len(t, osdName("abcdefghijklmnopqrstuvwxyz0123456789"), 31)
}

const dayNightXML = `<body><DayNightThreshold version="1.1"><channelId>0</channelId><threshold>default</threshold><stat>day</stat><thresholdval><min>0</min><max>100</max><cur>50</cur></thresholdval></DayNightThreshold></body>`

func TestDayNightChange(t *testing.T) {
	custom := func(v int) *unifi.ISP {
		return &unifi.ISP{IrLedMode: sptr("auto"), IrLedLevel: iptr(255), IcrSwitchMode: sptr("lux"), IcrCustomValue: iptr(v)}
	}
	b := parse(t, dayNightXML)
	require.True(t, dayNightChange(custom(5))(b)) // 10 lux of 30
	require.Equal(t, "custom", b.Value("DayNightThreshold/threshold"))
	require.Equal(t, "33", b.Value("DayNightThreshold/thresholdval/cur"))
	require.False(t, dayNightChange(custom(5))(b))

	require.True(t, dayNightChange(custom(10))(b))
	require.Equal(t, "100", b.Value("DayNightThreshold/thresholdval/cur"))

	// not custom: the doorbell's threshold stays (the D340W ignores "default")
	auto := &unifi.ISP{IrLedMode: sptr("auto"), IrLedLevel: iptr(255), IcrSwitchMode: sptr("sensitivity")}
	require.False(t, dayNightChange(auto)(b))
	require.Equal(t, "custom", b.Value("DayNightThreshold/threshold"))
	require.Equal(t, "100", b.Value("DayNightThreshold/thresholdval/cur"))
}

// Privacy mode on top of more zones than the doorbell stores: the full-frame
// zone has the highest id, and it alone is applied.
func TestPrivacyModeWithManyZones(t *testing.T) {
	var masks []unifi.Mask
	for i := range 5 {
		masks = append(masks, unifi.Mask{ID: i, Coord: []float64{0, 0, 10, 0, 10, 10}})
	}
	masks = append(masks, unifi.Mask{ID: 9, Coord: []float64{0, 0, 1000, 0, 1000, 1000, 0, 1000}})
	b := parse(t, `<body><Shelter version="1.1"><channelId>0</channelId><enable>0</enable><shelterList></shelterList></Shelter></body>`)
	require.True(t, maskChange(masks)(b))
	list := b.Find("Shelter/shelterList").Children
	require.Equal(t, "1", list[0].Value("enable"))
	require.Equal(t, strconv.Itoa(1000<<16|1000), list[0].Value("width"))
	for _, e := range list[1:] {
		require.Equal(t, "0", e.Value("enable"))
	}

	// as the doorbell reports it back, deleted entries included: no change
	b = parse(t, string(b.Marshal()))
	require.False(t, maskChange(masks)(b))
}

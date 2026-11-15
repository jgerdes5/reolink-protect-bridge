package unifi

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Message is the JSON envelope of the ubnt_avclient protocol, field order as a
// camera sends it.
type Message struct {
	From             string          `json:"from"`
	FunctionName     string          `json:"functionName"`
	InResponseTo     int             `json:"inResponseTo"`
	MessageID        int             `json:"messageId"`
	Payload          json.RawMessage `json:"payload"`
	ResponseExpected bool            `json:"responseExpected"`
	To               string          `json:"to"`
	TimeStamp        string          `json:"timeStamp,omitempty"` // events only
}

// Channel describes one of the three video tracks a UniFi camera offers
// (video1 = high, video2 = medium, video3 = low quality).
type Channel struct {
	Width, Height, FPS int
	Codec              string // what the source delivers: "h264" (default) or "h265"
	// What the source's encoder offers, if known: shown as the choices in
	// Protect's frame rate and bitrate settings.
	FPSValues              []int
	Bitrate                int // current, bit/s
	BitrateMin, BitrateMax int // bit/s
}

// Identity is what Protect uses to recognise the camera model: it keys on
// sysid + platform + firmwareBuild, not on the model string.
type Identity struct {
	Model         string // "UVC G4 Doorbell"
	SysID         uint32 // 0xa571
	Platform      string // "s5l"
	Firmware      string // short form, e.g. "5.5.45"
	FirmwareBuild string // e.g. "0dda191.261027.0531"
}

// G4Doorbell is the identity the bridge presents, from the official UniFi G4
// Doorbell firmware v5.5.45 header.
var G4Doorbell = Identity{"UVC G4 Doorbell", 0xa571, "s5l", "5.5.45", "0dda191.261027.0531"}

func rfc3339ms(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// videoCodecs offers H.265 only if the doorbell's main stream is H.265:
// Protect may switch an H.265-capable camera to H.265 by itself (on adoption,
// when most cameras use it), and the main stream is too large to transcode.
func (c *Camera) videoCodecs() []string {
	if c.channel("video1").Codec == "h265" {
		return []string{"h264", "h265"}
	}
	return []string{"h264"}
}

// featureFlags are the camera capabilities sent in the hello. Protect reads
// only the short keys (has*/can* are ignored; canAdjustIspSettings, hasWdr,
// hasMotionZones and hasIcrSensitivity are always on). Each key below unlocks
// a setting in Protect that the bridge applies to the doorbell.
func (c *Camera) featureFlags() map[string]any {
	mic := 0
	if c.HasAudio {
		mic = 1
	}
	flags := map[string]any{
		"mic":            mic,
		"aec":            []any{},
		"videoMode":      []string{"default"},
		"motionDetect":   []string{"enhanced"}, // ChangeSmartMotionSettings
		"smartDetect":    c.smartDetect(),
		"videoCodecs":    c.videoCodecs(),
		"audioCodecs":    c.audioCodecs(),
		"audioStyle":     []string{"nature"},
		"horizontalFlip": true, // mirror
		"verticalFlip":   true, // flip
		"hdr":            true, // HDR selector -> Reolink hdrSwitch
		"truedaynight":   true, // night vision block -> day/night mode and IR LEDs
		"ledStatus":      true, // status light -> doorbell button light; PATCH ledSettings fails without it
		// privacy zones -> the doorbell's masks (Shelter): at most 4, each
		// turned into its bounding rectangle. Not rectangleOnly: Protect's
		// web UI hides the privacy section for such cameras.
		"privacyMask":  true,
		"privacyMasks": map[string]any{"maxZones": 4, "rectangleOnly": false},
		// exclusion zones (Protect reads the key "excludeZone"): cut out of the
		// doorbell's AI detection area; the UI also offers "privacy mask" on
		// them, which turns them into privacy zones. Values as a G4 Instant.
		"excludeZone":          map[string]any{"maxZones": 16, "rectangleOnly": true},
		"squareEventThumbnail": true,
	}
	if c.Doorbell {
		flags["doorbell"] = true // isDoorbell
		// the doorbell tone section (ring volume); Protect falls back to
		// isDoorbell, not doorbell, so it must be sent
		flags["supportCustomRingtone"] = true
	}
	if c.Talk != nil {
		flags["speaker"] = true // hasSpeaker: talkback
	}
	if c.opus.Load() {
		flags["opusSampleRates"] = []int{16000} // what the bridge encodes
	}
	return flags
}

func (c *Camera) audioCodecs() []string {
	if c.opus.Load() {
		return []string{"aac", "opus"}
	}
	return []string{"aac"}
}

// smartDetect is what the doorbell's AI detects; lines and loitering are
// evaluated by the bridge from its boxes (lineCrossing* unlock the line tab).
func (c *Camera) smartDetect() []string {
	types := []string{"person", "vehicle", "animal"}
	if c.pkg.Load() {
		types = append(types, "package")
	}
	return append(types, "lineCrossing", "lineCrossingCounting")
}

func (c *Camera) helloPayload(token string) map[string]any {
	mac := strings.ToLower(c.MAC)
	hwaddr := mac[0:2] + ":" + mac[2:4] + ":" + mac[4:6] + ":" + mac[6:8] + ":" + mac[8:10] + ":" + mac[10:12]
	id := c.identity()
	return map[string]any{
		"adoptionCode":         token,
		"connectionHost":       c.Host,
		"connectionSecurePort": c.port(),
		"fwVersion":            id.Firmware,
		"hwrev":                19,
		"idleTime":             191.96,
		"ip":                   c.IP,
		"mac":                  c.MAC,
		"model":                id.Model,
		"name":                 c.Name,
		"protocolVersion":      67,
		"rebootTimeoutSec":     30,
		"semver":               "v" + id.Firmware,
		"totalLoad":            0.5474,
		"upgradeTimeoutSec":    150,
		"uptime":               int(c.uptime().Seconds()),
		"features":             c.featureFlags(),
		"sysid":                id.SysID,
		"platform":             id.Platform,
		"firmwareBuild":        id.FirmwareBuild,
		"hwaddr":               hwaddr,
		"lensmodel":            id.Model,
		"cameraName":           c.Name,
		"isGen5s":              true,
		"isDoorbellSeries":     c.Doorbell,
	}
}

// videoSettings is the ChangeVideoSettings reply: what the camera says it
// streams on each channel, plus the destinations Protect asked for.
func (c *Camera) videoSettings(dst map[string][]string) map[string]any {
	c.mu.Lock()
	streamNames := make(map[string]string, len(c.streamNames))
	for k, v := range c.streamNames {
		streamNames[k] = v
	}
	hdrMode := 0 // Protect re-reads video.hdrMode after switching HDR
	if c.hdr {
		hdrMode = 1
	}
	requested := make(map[string]string, len(c.codecs))
	for k, v := range c.codecs {
		requested[k] = v
	}
	fpsReq := make(map[string]int, len(c.fpsReq))
	for k, v := range c.fpsReq {
		fpsReq[k] = v
	}
	c.mu.Unlock()

	// The codec each channel is streamed in: what Protect asked for (the
	// bridge transcodes if the doorbell sends something else), else the source's.
	codec := func(name string, ch Channel) string {
		if v := requested[name]; v != "" {
			return v
		}
		return ch.Codec
	}

	track := func(name string, ch Channel, streamID, ordinal int, desc string, cbr, vbrMax, rangeMax int) map[string]any {
		var params any
		if streamName := streamNames[name]; streamName != "" {
			params = map[string]any{
				"audioId": nil, "streamName": streamName, "suppressAudio": nil,
				"suppressVideo": nil, "videoId": nil,
			}
		}
		t := map[string]any{
			"M": 1, "N": 30,
			"avSerializer": map[string]any{
				"destinations": dst[name],
				"parameters":   params,
				"type":         "extendedFlv",
			},
			"bitRateCbrAvg":            cbr,
			"bitRateVbrMax":            vbrMax,
			"bitRateVbrMin":            48000,
			"description":              desc,
			"enabled":                  true,
			"fps":                      ch.FPS,
			"gopModel":                 0,
			"height":                   ch.Height,
			"horizontalFlip":           false,
			"isCbr":                    false,
			"maxFps":                   30,
			"minClientAdaptiveBitRate": 0,
			"minMotionAdaptiveBitRate": 0,
			"nMultiplier":              6,
			"name":                     name,
			"sourceId":                 ordinal,
			"streamId":                 streamID,
			"streamOrdinal":            ordinal,
			"type":                     codec(name, ch),
			"validBitrateRangeMax":     rangeMax,
			"validBitrateRangeMin":     32000,
			"validFpsValues":           []int{1, 2, 3, 4, 5, 6, 8, 9, 10, 12, 15, 16, 18, 20, 24, 25, 30},
			"verticalFlip":             false,
			"width":                    ch.Width,
		}
		if ordinal > 0 {
			t["currentVbrBitrate"] = vbrMax
		}
		// the doorbell's real choices, so Protect offers only what it can do
		if len(ch.FPSValues) > 0 {
			t["validFpsValues"] = ch.FPSValues
			t["maxFps"] = slices.Max(ch.FPSValues)
			if f := fpsReq[name]; f > 0 {
				t["fps"] = closest(ch.FPSValues, f)
			}
		}
		if ch.Bitrate > 0 {
			t["bitRateVbrMax"] = ch.Bitrate
			if ordinal > 0 {
				t["currentVbrBitrate"] = ch.Bitrate
			}
		}
		if ch.BitrateMax > 0 {
			t["validBitrateRangeMin"] = ch.BitrateMin
			t["validBitrateRangeMax"] = ch.BitrateMax
		}
		return t
	}

	return map[string]any{
		"audio": map[string]any{
			"bitRate": 32000, "channels": 1, "description": "audio track",
			"enableTemporalNoiseShaping": false, "enabled": true, "mode": 0,
			"quality": 0, "sampleRate": 16000, "type": "aac", "volume": 100,
		},
		"firmwarePath": "/lib/firmware/",
		"video": map[string]any{
			"enableHrd": false, "hdrMode": hdrMode, "lowDelay": false,
			"videoMode": "default", "videoCodec": codec("video1", c.channel("video1")),
			"mjpg": map[string]any{
				"avSerializer": map[string]any{
					"destinations": []string{"file:///tmp/snap.jpeg", "file:///tmp/snap_av.jpg"},
					"parameters": map[string]any{
						"audioId": 1000, "enableTimestampsOverlapAvoidance": false,
						"suppressAudio": true, "suppressVideo": false, "videoId": 1001,
					},
					"type": "mjpg",
				},
				"bitRateCbrAvg": 500000, "bitRateVbrMax": 500000, "bitRateVbrMin": nil,
				"description": "JPEG pictures", "enabled": true, "fps": 5, "height": 720,
				"isCbr": false, "maxFps": 5, "minClientAdaptiveBitRate": 0,
				"minMotionAdaptiveBitRate": 0, "nMultiplier": nil, "name": "mjpg",
				"quality": 80, "sourceId": 3, "streamId": 8, "streamOrdinal": 3,
				"type": "mjpg", "validBitrateRangeMax": 6000000, "validBitrateRangeMin": 32000,
				"width": 1280,
			},
			"video1": track("video1", c.channel("video1"), 1, 0, "Hi quality video track", 6000000, 12000000, 16000000),
			"video2": track("video2", c.channel("video2"), 2, 1, "Medium quality video track", 500000, 1200000, 1500000),
			"video3": track("video3", c.channel("video3"), 4, 2, "Low quality video track", 300000, 200000, 750000),
			"vinFps": 30,
		},
	}
}

// ispSettings is the image-settings reply (ResetIspSettings / ChangeIspSettings).
func ispSettings(change bool) map[string]any {
	m := map[string]any{
		"aeMode": "auto", "aeTargetPercent": 50, "aggressiveAntiFlicker": 0,
		"brightness": 50, "contrast": 50, "criticalTmpOfProtect": 40,
		"darkAreaCompensateLevel": 0, "denoise": 50, "enable3dnr": 1,
		"enableMicroTmpProtect": 1, "enablePauseMotion": 0, "flip": 0,
		"focusMode": "ztrig", "focusPosition": 0, "forceFilterIrSwitchEvents": 0,
		"hue": 50, "icrLightSensorNightThd": 0, "icrSensitivity": 0,
		"irLedLevel": 215, "irLedMode": "auto",
		"irOnStsBrightness": 0, "irOnStsContrast": 0, "irOnStsDenoise": 0,
		"irOnStsHue": 0, "irOnStsSaturation": 0, "irOnStsSharpness": 0, "irOnStsWdr": 0,
		"irOnValBrightness": 50, "irOnValContrast": 50, "irOnValDenoise": 50,
		"irOnValHue": 50, "irOnValSaturation": 50, "irOnValSharpness": 50, "irOnValWdr": 1,
		"mirror": 0, "queryIrLedStatus": 0, "saturation": 50, "sharpness": 50,
		"touchFocusX": 1001, "touchFocusY": 1001, "wdr": 1, "zoomPosition": 0,
	}
	if change {
		m["dZoomCenterX"] = 50
		m["dZoomCenterY"] = 50
		m["dZoomScale"] = 0
		m["dZoomStreamId"] = 4
		m["enableExternalIr"] = 0
		m["lensDistortionCorrection"] = 1
		m["masks"] = nil
	}
	return m
}

func (c *Camera) osdSettings() map[string]any {
	ch := func() map[string]any {
		return map[string]any{
			"enableDate": 1, "enableLogo": 1, "enableReportdStatsLevel": 0,
			"enableStreamerStatsLevel": 0, "tag": c.Name,
		}
	}
	return map[string]any{
		"_1": ch(), "_2": ch(), "_3": ch(), "_4": ch(),
		"enableOverlay": 1, "logoScale": 50, "overlayColorId": 0,
		"textScale": 50, "useCustomLogo": 0,
	}
}

func (c *Camera) networkStatus() map[string]any {
	return map[string]any{
		"connectionState": 2, "connectionStateDescription": "CONNECTED",
		"defaultInterface": "eth0", "dhcpLeasetime": 86400,
		"dnsServer": "8.8.8.8 4.2.2.2", "gateway": "192.168.103.1",
		"ipAddress": c.IP, "linkDuplex": 1, "linkSpeedMbps": 100,
		"mode": "dhcp", "networkMask": "255.255.255.0",
	}
}

var soundLedSettings = map[string]any{
	"ledFaceAlwaysOnWhenManaged": 1, "ledFaceEnabled": 1, "speakerEnabled": 1,
	"speakerVolume": 100, "systemSoundsEnabled": 1, "userLedBlinkPeriodMs": 0,
	"userLedColorFg": "blue", "userLedOnNoff": 1,
}

// firmwareVersion finds the short version token (x.y.z) in a firmware push URI,
// e.g. .../uvc/firmware/5.5.31/<MAC> or .../e7fc-sav837gw-5.4.132-<uuid>.bin.
// Same rule as unifi-cam-proxy: the token starts the path or follows '/' or '-',
// and is followed by '/', '-', ".bin" at the end, or the end of the path.
func firmwareVersion(uri string) string {
	if i := strings.Index(uri, "://"); i >= 0 {
		uri = uri[i+3:]
		if j := strings.IndexByte(uri, '/'); j >= 0 {
			uri = uri[j:]
		} else {
			uri = ""
		}
	}
	if i := strings.IndexAny(uri, "?#"); i >= 0 {
		uri = uri[:i]
	}
	for _, m := range versionRE.FindAllStringIndex(uri, -1) {
		if m[0] > 0 && uri[m[0]-1] != '/' && uri[m[0]-1] != '-' {
			continue
		}
		rest := uri[m[1]:]
		if rest == "" || rest == ".bin" || rest[0] == '/' || rest[0] == '-' {
			return uri[m[0]:m[1]]
		}
	}
	return ""
}

var versionRE = regexp.MustCompile(`\d+\.\d+\.\d+`)

// closest is the value in values nearest to v.
func closest(values []int, v int) int {
	best := values[0]
	for _, x := range values {
		if d, bd := x-v, best-v; d*d < bd*bd || (d*d == bd*bd && x > best) {
			best = x
		}
	}
	return best
}

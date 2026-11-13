package unifi

import (
	"encoding/json"
	"sort"
	"strconv"
)

// Settings receives the camera settings Protect 7.3 sends, so they can be
// applied to the real camera. Calls arrive in the order Protect sends them,
// from one goroutine, never on the protocol loop; on every (re)connect
// Protect sends all of them once (applyLatestSettingsToCamera).
type Settings interface {
	ISP(ISP)
	SoundLed(SoundLed)
	SmartDetect(SmartDetect)
	Motion(Motion)
	HDR(on bool)
	OSD(OSD)
	Video(Video)
}

// Video is a channel's frame rate and bitrate from ChangeVideoSettings
// (serializeForCamera: fps for every channel, bitRateVbrMax only for the
// high-quality one). Zero means not sent.
type Video struct {
	Channel string // video1 | video2 | video3
	FPS     int
	Bitrate int // bit/s
}

// OSD is ChangeOsdSettings for cameras without the overlay API
// (updateOsdSettings): the same entry for each of the four streams.
type OSD struct {
	Name     string // "" = name off
	Date     bool
	Logo     bool
	Location string // topLeft | topMiddle | topRight | bottomLeft | bottomMiddle | bottomRight
}

// Mask is a privacy zone: a polygon [x1,y1,...] in 0-1000.
type Mask struct {
	ID    int
	Coord []float64
}

// ISP is ChangeIspSettings in Protect's wire names (updateIspSettings). Fields
// Protect did not send are nil.
type ISP struct {
	Brightness     *int    `json:"brightness"` // 0-100
	Contrast       *int    `json:"contrast"`
	Saturation     *int    `json:"saturation"`
	Hue            *int    `json:"hue"`
	Sharpness      *int    `json:"sharpness"`
	Denoise        *int    `json:"denoise"`
	Enable3DNR     *int    `json:"enable3dnr"`
	Mirror         *int    `json:"mirror"`
	Flip           *int    `json:"flip"`
	AeMode         *string `json:"aeMode"`         // auto | flick50 | flick60 | ...
	IrLedMode      *string `json:"irLedMode"`      // auto | manual
	IrLedLevel     *int    `json:"irLedLevel"`     // 0 = IR off (with manual: night vision off)
	IcrSwitchMode  *string `json:"icrSwitchMode"`  // sensitivity | lux
	IcrCustomValue *int    `json:"icrCustomValue"` // 0-10 with lux
	HdrMode        *string `json:"hdrMode"`        // normal | superHdr
	// Masks are the privacy zones; nil if Protect did not send masks.
	Masks []Mask `json:"-"`
}

// parseMasks reads ChangeIspSettings.masks: {"0": null} for none, else
// {"color": [...], "<zoneId>": {"update": bool, "coord": [...]}}.
func parseMasks(raw json.RawMessage) []Mask {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	masks := []Mask{}
	for k, v := range m {
		id, err := strconv.Atoi(k)
		if err != nil {
			continue // "color"
		}
		var z struct {
			Coord []float64 `json:"coord"`
		}
		if json.Unmarshal(v, &z) == nil && len(z.Coord) >= 6 {
			masks = append(masks, Mask{ID: id, Coord: z.Coord})
		}
	}
	sort.Slice(masks, func(i, j int) bool { return masks[i].ID < masks[j].ID })
	return masks
}

// NightVision decodes Protect's IR fields into its UI setting: "auto", "on",
// "off", or "custom" (switch at the icrCustomValue lux level).
func (i ISP) NightVision() string {
	if i.IrLedMode == nil {
		return ""
	}
	level := 255
	if i.IrLedLevel != nil {
		level = *i.IrLedLevel
	}
	switch {
	case *i.IrLedMode == "manual" && level == 0:
		return "off"
	case *i.IrLedMode == "manual":
		return "on"
	case i.IcrSwitchMode != nil && *i.IcrSwitchMode == "lux":
		return "custom"
	}
	return "auto"
}

// SoundLed is ChangeSoundLedSettings (updateSoundLedSettings). Requests that
// only manage custom ringtones (customSounds) leave these nil.
type SoundLed struct {
	LedFaceEnabled      *int `json:"ledFaceEnabled"` // status light
	SpeakerVolume       *int `json:"speakerVolume"`  // 0-100
	RingVolume          *int `json:"ringVolume"`     // 0-100, the doorbell's own ringtone
	SystemSoundsEnabled *int `json:"systemSoundsEnabled"`
}

// Zone is a zone in Protect's camera coordinates: a flat [x1,y1,x2,y2,...]
// polygon in 0-1000 from the top-left corner (serializeForCamera).
type Zone struct {
	Coord       []float64 `json:"coord"`
	Sensitivity int       `json:"sensitivity"` // 0-100
	ObjectTypes []string  `json:"objectTypes"`
	Level       int       `json:"level"` // motion zones: 100 - sensitivity
	// loiter zones: how long (ms) each object type must stay to trigger
	LoiterTimes map[string]LoiterTime `json:"loiterTriggerTimeMaps"`
}

// LoiterTime is one entry of a loiter zone's loiterTriggerTimeMaps.
type LoiterTime struct {
	MSec       int    `json:"loiterTriggerTime"`
	ObjectType string `json:"objectType"`
}

// Line is a line-crossing line: P1, P2 and a reference point A on the "A" side.
type Line struct {
	Coord       []float64 `json:"coord"`
	Direction   string    `json:"crosslineDirection"` // A2B | B2A | none
	Sensitivity int       `json:"sensitivity"`
	ObjectTypes []string  `json:"objectTypes"`
}

// SmartDetect is ChangeSmartDetectSettings (updateSmartDetectSettings).
type SmartDetect struct {
	Enabled     []string        `json:"enableSmartDetect"` // person, vehicle, animal
	Zones       map[string]Zone `json:"zones"`
	Lines       map[string]Line `json:"lines"`
	LoiterZones map[string]Zone `json:"loiterZones"`
	StartMSec   int             `json:"eventStartMSec"`
	StopMSec    int             `json:"eventStopMSec"`
}

// Motion is ChangeSmartMotionSettings (updateEnhancedMotionSettings).
type Motion struct {
	Enable *bool           `json:"enable"`
	Zones  map[string]Zone `json:"zones"`
}

// applySetting passes a settings message to c.Settings in order, off the
// protocol loop.
func (c *Camera) applySetting(fn string, payload json.RawMessage) {
	if c.Settings == nil {
		return
	}
	var job func()
	switch fn {
	case "ChangeIspSettings":
		var v ISP
		if json.Unmarshal(payload, &v) != nil {
			return
		}
		var raw struct {
			Masks json.RawMessage `json:"masks"`
		}
		if json.Unmarshal(payload, &raw) == nil && len(raw.Masks) > 0 && string(raw.Masks) != "null" {
			v.Masks = parseMasks(raw.Masks)
		}
		job = func() { c.Settings.ISP(v) }
	case "ChangeSoundLedSettings":
		var v SoundLed
		if json.Unmarshal(payload, &v) != nil || (v.LedFaceEnabled == nil && v.SpeakerVolume == nil && v.RingVolume == nil) {
			return
		}
		job = func() { c.Settings.SoundLed(v) }
	case "ChangeSmartDetectSettings":
		var v SmartDetect
		if json.Unmarshal(payload, &v) != nil {
			return
		}
		job = func() { c.Settings.SmartDetect(v) }
	case "ChangeSmartMotionSettings":
		var v Motion
		if json.Unmarshal(payload, &v) != nil {
			return
		}
		job = func() { c.Settings.Motion(v) }
	case "ChangeOsdSettings":
		var v struct {
			Stream *struct {
				Tag        string `json:"tag"`
				EnableDate int    `json:"enableDate"`
				EnableLogo int    `json:"enableLogo"`
			} `json:"_1"`
			Location string `json:"overlayLocation"`
		}
		if json.Unmarshal(payload, &v) != nil || v.Stream == nil {
			return
		}
		osd := OSD{Name: v.Stream.Tag, Date: v.Stream.EnableDate != 0, Logo: v.Stream.EnableLogo != 0, Location: v.Location}
		job = func() { c.Settings.OSD(osd) }
	case "ChangeVideoSettings":
		var v struct {
			Video struct {
				HdrMode *int `json:"hdrMode"`
			} `json:"video"`
		}
		var ch struct {
			Video map[string]json.RawMessage `json:"video"`
		}
		if json.Unmarshal(payload, &v) != nil || json.Unmarshal(payload, &ch) != nil {
			return
		}
		var jobs []func()
		for _, name := range []string{"video1", "video2", "video3"} {
			var t struct {
				FPS     int `json:"fps"`
				Bitrate int `json:"bitRateVbrMax"`
			}
			if raw := ch.Video[name]; raw == nil || json.Unmarshal(raw, &t) != nil || (t.FPS <= 0 && t.Bitrate <= 0) {
				continue
			}
			vs := Video{Channel: name, FPS: max(t.FPS, 0), Bitrate: max(t.Bitrate, 0)}
			jobs = append(jobs, func() { c.Settings.Video(vs) })
		}
		if v.Video.HdrMode != nil {
			on := *v.Video.HdrMode != 0
			c.mu.Lock()
			c.hdr = on
			c.mu.Unlock()
			jobs = append(jobs, func() { c.Settings.HDR(on) })
		}
		if len(jobs) == 0 {
			return
		}
		job = func() {
			for _, j := range jobs {
				j()
			}
		}
	default:
		return
	}

	c.settingsOnce.Do(func() {
		c.settingsQueue = make(chan func(), 64)
		go func() {
			for job := range c.settingsQueue {
				job()
			}
		}()
	})
	select {
	case c.settingsQueue <- job:
	default:
		c.Log.Warn().Msgf("[unifi] %s: settings queue full, %s dropped", c.Name, fn)
	}
}

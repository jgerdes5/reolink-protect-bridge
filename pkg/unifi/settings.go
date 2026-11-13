package unifi

import (
	"encoding/json"
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
	case "ChangeVideoSettings":
		var v struct {
			Video struct {
				HdrMode *int `json:"hdrMode"`
			} `json:"video"`
		}
		if json.Unmarshal(payload, &v) != nil || v.Video.HdrMode == nil {
			return
		}
		on := *v.Video.HdrMode != 0
		c.mu.Lock()
		c.hdr = on
		c.mu.Unlock()
		job = func() { c.Settings.HDR(on) }
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

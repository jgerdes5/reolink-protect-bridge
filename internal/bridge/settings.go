package bridge

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// Protect's camera settings, applied to the Reolink doorbell over Baichuan
// (msg IDs and XML from the D340W firmware, see pkg/baichuan/settings.go).
// Protect sends all of them on every connect and each change on its own; they
// are collected for settleDelay and applied together over one short
// connection, each group as GET -> modify -> SET, and only if a value differs.

const settleDelay = time.Second

// reolinkAI maps Protect object types to Reolink AI types.
var reolinkAI = map[string]string{"person": "people", "vehicle": "vehicle", "animal": "dog_cat"}

type settings struct {
	b     *Bridge
	apply bool // write to the doorbell (Options.Settings)

	mu       sync.Mutex
	isp      *unifi.ISP
	hdrOn    *bool  // Protect's HDR switch (ChangeVideoSettings), kept across flushes
	hdrMode  string // normal | superHdr (ChangeIspSettings), kept across flushes
	hdrDirty bool
	sound    *unifi.SoundLed
	smart    *unifi.SmartDetect
	motion   *unifi.Motion
	timer    *time.Timer
	enabled  map[string]bool // Protect object types with smart detection on; nil = all
}

func (s *settings) ISP(v unifi.ISP) {
	s.mu.Lock()
	s.isp = &v
	if v.HdrMode != nil && *v.HdrMode != s.hdrMode {
		s.hdrMode = *v.HdrMode
		s.hdrDirty = s.hdrOn != nil
	}
	s.mu.Unlock()
	s.schedule()
}

func (s *settings) HDR(on bool) {
	s.mu.Lock()
	s.hdrOn = &on
	s.hdrDirty = true
	s.mu.Unlock()
	s.schedule()
}

func (s *settings) SoundLed(v unifi.SoundLed) {
	s.mu.Lock()
	s.sound = &v
	s.mu.Unlock()
	s.schedule()
}

func (s *settings) SmartDetect(v unifi.SmartDetect) {
	s.mu.Lock()
	s.smart = &v
	s.enabled = map[string]bool{}
	for _, t := range v.Enabled {
		s.enabled[t] = true
	}
	s.mu.Unlock()
	if s.b != nil {
		s.b.tracker.setZones(v.Zones, v.Lines, v.LoiterZones)
	}
	s.schedule()
}

func (s *settings) Motion(v unifi.Motion) {
	s.mu.Lock()
	s.motion = &v
	s.mu.Unlock()
	s.schedule()
}

// smartEnabled reports whether Protect has smart detection on for an object
// type; events for disabled types are not sent.
func (s *settings) smartEnabled(object string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled == nil || s.enabled[object]
}

func (s *settings) schedule() {
	if !s.apply {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer == nil {
		s.timer = time.AfterFunc(settleDelay, s.flush)
	} else {
		s.timer.Reset(settleDelay)
	}
}

// flush applies everything pending.
func (s *settings) flush() {
	s.mu.Lock()
	isp, sound, smart, motion := s.isp, s.sound, s.smart, s.motion
	s.isp, s.sound, s.smart, s.motion = nil, nil, nil, nil
	hdr := ""
	if s.hdrDirty && s.hdrOn != nil {
		hdr = hdrSwitch(*s.hdrOn, s.hdrMode)
	}
	s.hdrDirty = false
	s.mu.Unlock()
	if isp == nil && hdr == "" && sound == nil && smart == nil && motion == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d := s.b.opts.Doorbell
	client, err := baichuan.Dial(ctx, baichuan.Config{Host: d.Host, Port: d.Port, Username: d.Username, Password: d.Password})
	if err != nil {
		log.Warn().Err(err).Msg("[settings] can't reach the doorbell")
		return
	}
	defer client.Close()
	ch := uint8(d.Channel)

	if isp != nil || hdr != "" {
		s.run(ctx, "image", func() error { return applyImage(ctx, client, ch, isp, hdr) })
	}
	if isp != nil || sound != nil {
		s.run(ctx, "leds", func() error { return applyLeds(ctx, client, ch, isp, sound) })
	}
	if sound != nil {
		s.run(ctx, "audio", func() error { return applyAudio(ctx, client, ch, sound) })
	}
	if smart != nil {
		s.run(ctx, "ai", func() error { return applyAI(ctx, client, ch, smart) })
	}
	if motion != nil {
		s.run(ctx, "motion", func() error { return applyMotion(ctx, client, ch, motion) })
	}
}

func (s *settings) run(ctx context.Context, what string, f func() error) {
	if ctx.Err() != nil {
		return
	}
	if err := f(); err != nil {
		log.Warn().Err(err).Msgf("[settings] %s", what)
	}
}

// update runs GET -> change -> SET for one config group; change returns
// whether anything differs.
func update(ctx context.Context, c *baichuan.Client, get, set uint32, ch uint8, req []byte, change func(*baichuan.Node) bool, what string) error {
	body, err := c.GetConfig(ctx, get, ch, req)
	if err != nil {
		return err
	}
	if !change(body) {
		return nil
	}
	if err = c.SetConfig(ctx, set, ch, body); err != nil {
		return err
	}
	log.Info().Msgf("[settings] %s applied to the doorbell", what)
	return nil
}

// applyImage: picture preferences, mirror/flip, 3DNR, anti-flicker,
// day/night mode and HDR (msg 26/25: VideoInput + InputAdvanceCfg).
func applyImage(ctx context.Context, c *baichuan.Client, ch uint8, isp *unifi.ISP, hdr string) error {
	return update(ctx, c, baichuan.MsgImageGet, baichuan.MsgImageSet, ch, nil, imageChange(isp, hdr), "picture settings")
}

// imageChange returns the change for picture settings.
func imageChange(isp *unifi.ISP, hdr string) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		changed := false
		set := func(path, v string) {
			if b.Set(path, v) {
				changed = true
			}
		}
		if isp != nil {
			for path, v := range map[string]*int{
				"VideoInput/bright": isp.Brightness, "VideoInput/contrast": isp.Contrast,
				"VideoInput/saturation": isp.Saturation, "VideoInput/hue": isp.Hue,
				"VideoInput/sharpen": isp.Sharpness,
			} {
				if v != nil {
					set(path, strconv.Itoa(scale255(*v)))
				}
			}
			if isp.Mirror != nil {
				set("InputAdvanceCfg/mirror", boolStr(*isp.Mirror != 0))
			}
			if isp.Flip != nil {
				set("InputAdvanceCfg/flip", boolStr(*isp.Flip != 0))
			}
			if isp.Enable3DNR != nil {
				set("InputAdvanceCfg/nr3d/enable", boolStr(*isp.Enable3DNR != 0))
			}
			if isp.Denoise != nil {
				set("InputAdvanceCfg/nr3d/value", denoiseLevel(*isp.Denoise))
			}
			if isp.AeMode != nil {
				switch *isp.AeMode {
				case "flick50":
					set("InputAdvanceCfg/PowerLineFrequency/enable", "1")
					set("InputAdvanceCfg/PowerLineFrequency/mode", "50hz")
				case "flick60":
					set("InputAdvanceCfg/PowerLineFrequency/enable", "1")
					set("InputAdvanceCfg/PowerLineFrequency/mode", "60hz")
				} // "auto": Reolink has no automatic mode, leave it
			}
			if mode := dayNightMode(isp.NightVision()); mode != "" {
				set("InputAdvanceCfg/DayNight/mode", mode)
			}
		}
		if hdr != "" {
			set("InputAdvanceCfg/hdrSwitch", hdr)
		}
		return changed
	}
}

// applyLeds: IR LEDs from night vision, button light from the status light
// (msg 208/209 LedState).
func applyLeds(ctx context.Context, c *baichuan.Client, ch uint8, isp *unifi.ISP, sound *unifi.SoundLed) error {
	return update(ctx, c, baichuan.MsgLedGet, baichuan.MsgLedSet, ch, nil, ledChange(isp, sound), "LED settings")
}

// ledChange returns the change for LED settings.
func ledChange(isp *unifi.ISP, sound *unifi.SoundLed) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		changed := false
		if isp != nil {
			if state := irState(isp.NightVision()); state != "" && b.Set("LedState/state", state) {
				changed = true
			}
		}
		if sound != nil && sound.LedFaceEnabled != nil {
			cur := b.Value("LedState/doorbellLightState")
			if want := buttonLight(*sound.LedFaceEnabled != 0, cur); want != cur && b.Set("LedState/doorbellLightState", want) {
				changed = true
			}
		}
		return changed
	}
}

// applyAudio: speaker volume and the doorbell's own ringtone (msg 264/265
// audioCfg). Changing a volume does not play anything.
func applyAudio(ctx context.Context, c *baichuan.Client, ch uint8, sound *unifi.SoundLed) error {
	return update(ctx, c, baichuan.MsgAudioGet, baichuan.MsgAudioSet, ch, nil, audioChange(sound), "audio settings")
}

// audioChange returns the change for audio settings.
func audioChange(sound *unifi.SoundLed) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		changed := false
		set := func(path, v string) {
			if b.Set(path, v) {
				changed = true
			}
		}
		if v := sound.SpeakerVolume; v != nil {
			set("audioCfg/volume", strconv.Itoa(clamp(*v, 0, 100)))
			set("audioCfg/talkAndReplyVolume", strconv.Itoa(clamp(*v, 0, 100)))
		}
		if v := sound.RingVolume; v != nil {
			if *v > 0 {
				set("audioCfg/visitorVolume", strconv.Itoa(clamp(*v, 0, 100)))
			}
			set("audioCfg/visitorLoudspeaker", boolStr(*v > 0)) // 0 = the doorbell stays silent when pressed
		}
		return changed
	}
}

// applyAI: AI types on/off (299/300 AiCfg) and, per type, sensitivity and the
// detection area from Protect's smart-detect zones (342/343 AiDetectCfg).
func applyAI(ctx context.Context, c *baichuan.Client, ch uint8, sd *unifi.SmartDetect) error {
	var types []string
	for _, t := range []string{"person", "vehicle", "animal"} {
		for _, e := range sd.Enabled {
			if e == t {
				types = append(types, reolinkAI[t])
			}
		}
	}
	detect := strings.Join(types, ",")
	if detect == "" {
		detect = "none"
	}
	if err := update(ctx, c, baichuan.MsgAiCfgGet, baichuan.MsgAiCfgSet, ch, nil, func(b *baichuan.Node) bool {
		return b.Set("AiCfg/detectType", detect)
	}, "AI detection types"); err != nil {
		return err
	}

	for _, object := range []string{"person", "vehicle", "animal"} {
		aiType := reolinkAI[object]
		var zones []unifi.Zone
		sens := -1
		for _, z := range sd.Zones {
			if contains(z.ObjectTypes, object) {
				zones = append(zones, z)
				sens = max(sens, z.Sensitivity)
			}
		}
		err := update(ctx, c, baichuan.MsgAiDetectGet, baichuan.MsgAiDetectSet, ch, baichuan.AiDetectBody(ch, aiType),
			aiAreaChange(zones, sens), aiType+" detection area")
		if err != nil {
			return err
		}
	}
	return nil
}

// applyMotion: motion detection on/off, sensitivity and area from Protect's
// motion zones (46/47 MD).
func applyMotion(ctx context.Context, c *baichuan.Client, ch uint8, m *unifi.Motion) error {
	return update(ctx, c, baichuan.MsgMotionGet, baichuan.MsgMotionSet, ch, nil, motionChange(m), "motion detection")
}

// motionChange returns the change for motion detection.
func motionChange(m *unifi.Motion) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		changed := false
		if m.Enable != nil && b.Set("MD/enable", boolStr(*m.Enable)) {
			changed = true
		}
		var zones []unifi.Zone
		sens := -1
		for _, z := range m.Zones {
			zones = append(zones, z)
			sens = max(sens, 100-z.Level) // Protect sends level = 100 - sensitivity
		}
		if sens >= 0 && b.Set("MD/sensInfoNew/sensitivityDefault", strconv.Itoa(mdSensitivity(sens))) {
			changed = true
		}
		cols, _ := strconv.Atoi(b.Value("MD/scope/columns"))
		rows, _ := strconv.Atoi(b.Value("MD/scope/rows"))
		if cols > 0 && rows > 0 && b.Set("MD/scope/valueTable", baichuan.EncodeArea(rasterize(zones, cols, rows), cols, rows)) {
			changed = true
		}
		return changed
	}
}

// scale255 maps Protect's 0-100 to Reolink's 0-255 (50 -> 128).
func scale255(v int) int {
	return int(math.Round(float64(clamp(v, 0, 100)) * 255 / 100))
}

func denoiseLevel(v int) string {
	switch {
	case v < 34:
		return "low"
	case v < 67:
		return "medium"
	}
	return "high"
}

func dayNightMode(nv string) string {
	switch nv {
	case "auto", "custom":
		return "auto"
	case "on":
		return "blackAndWhite"
	case "off":
		return "color"
	}
	return ""
}

func irState(nv string) string {
	switch nv {
	case "auto", "custom":
		return "auto"
	case "on":
		return "open"
	case "off":
		return "close"
	}
	return ""
}

// hdrSwitch: Reolink 0 off, 1 auto, 2 on; Protect's "high" HDR is superHdr.
func hdrSwitch(on bool, mode string) string {
	switch {
	case !on:
		return "0"
	case mode == "superHdr":
		return "2"
	}
	return "1"
}

// buttonLight maps Protect's status light to the doorbell's button light:
// off = keepOff; on keeps the user's Reolink choice unless it was keepOff,
// then the Reolink default (lit on events).
func buttonLight(on bool, cur string) string {
	if !on {
		return "keepOff"
	}
	if cur == "keepOff" || cur == "" {
		return "close"
	}
	return cur
}

// mdSensitivity maps Protect's 0-100 (higher = more sensitive) to Reolink's
// MD wire value 1-50 (1 = most sensitive; reolink_aio: wire = 51 - ui).
func mdSensitivity(s int) int {
	ui := 1 + int(math.Round(float64(clamp(s, 0, 100))*49/100))
	return 51 - ui
}

// rasterize marks the grid cells whose centre lies in any of the zones
// (Protect polygons in 0-1000).
func rasterize(zones []unifi.Zone, cols, rows int) []bool {
	cells := make([]bool, cols*rows)
	for r := 0; r < rows; r++ {
		y := (float64(r) + 0.5) * 1000 / float64(rows)
		for c := 0; c < cols; c++ {
			x := (float64(c) + 0.5) * 1000 / float64(cols)
			for _, z := range zones {
				if inPolygon(z.Coord, x, y) {
					cells[r*cols+c] = true
					break
				}
			}
		}
	}
	return cells
}

// inPolygon is the even-odd rule for a flat [x1,y1,x2,y2,...] polygon.
func inPolygon(coord []float64, x, y float64) bool {
	n := len(coord) / 2
	if n < 3 {
		return false
	}
	in := false
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		xi, yi := coord[2*i], coord[2*i+1]
		xj, yj := coord[2*j], coord[2*j+1]
		if (yi > y) != (yj > y) && x < (xj-xi)*(y-yi)/(yj-yi)+xi {
			in = !in
		}
	}
	return in
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// aiAreaChange sets one AI type's sensitivity (-1 = keep) and detection area.
func aiAreaChange(zones []unifi.Zone, sens int) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		changed := false
		if sens >= 0 && b.Set("AiDetectCfg/sensitivity", strconv.Itoa(clamp(sens, 0, 100))) {
			changed = true
		}
		cols, _ := strconv.Atoi(b.Value("AiDetectCfg/width"))
		rows, _ := strconv.Atoi(b.Value("AiDetectCfg/height"))
		if cols > 0 && rows > 0 && b.Set("AiDetectCfg/area", baichuan.EncodeArea(rasterize(zones, cols, rows), cols, rows)) {
			changed = true
		}
		return changed
	}
}

// MsgEncGet/Set: Compression (encoder: resolution, fps, bitrate, gop) per stream.
const (
	msgEncGet = 56
	msgEncSet = 57
)

// applyKeyframes sets the keyframe interval (gop, in frames) of both streams
// to half a second with FastSwitch, else to one second, within the range the
// doorbell reports. Protect's live view starts a stream at its next keyframe
// when the viewer switches quality; a UniFi camera makes one on request
// (ForceIDR, from Protect's feedback on the stream socket), the Reolink
// can't, so the interval is the wait.
func (b *Bridge) applyKeyframes(ctx context.Context) {
	d := b.opts.Doorbell
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := baichuan.Dial(ctx, baichuan.Config{Host: d.Host, Port: d.Port, Username: d.Username, Password: d.Password})
	if err != nil {
		log.Debug().Err(err).Msg("[settings] keyframe interval: can't reach the doorbell")
		return
	}
	defer client.Close()
	err = update(ctx, client, msgEncGet, msgEncSet, uint8(d.Channel), nil, gopChange(b.opts.FastSwitch), "keyframe interval")
	if err != nil {
		log.Warn().Err(err).Msg("[settings] keyframe interval")
	}
}

// gopChange sets mainStream/subStream gop/cur to fps/2 (fast) or fps frames,
// clamped to gop/min..max.
func gopChange(fast bool) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		changed := false
		for _, stream := range []string{"mainStream", "subStream"} {
			fps, _ := strconv.Atoi(b.Value("Compression/" + stream + "/frame"))
			if fps <= 0 {
				continue
			}
			want := fps
			if fast {
				want = (fps + 1) / 2
			}
			if lo, err := strconv.Atoi(b.Value("Compression/" + stream + "/gop/min")); err == nil && want < lo {
				want = lo
			}
			if hi, err := strconv.Atoi(b.Value("Compression/" + stream + "/gop/max")); err == nil && hi > 0 && want > hi {
				want = hi
			}
			if b.Set("Compression/"+stream+"/gop/cur", strconv.Itoa(want)) {
				changed = true
			}
		}
		return changed
	}
}

package bridge

import (
	"context"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// Protect's camera settings, applied to the doorbell over Baichuan (msg IDs
// and XML from the D340W firmware, see pkg/baichuan/settings.go). Changes are
// debounced (settleQuiet after the last, at most settleMax after the first)
// and applied per group as GET -> modify -> SET, only if a value differs. The
// control connection is kept open for settingsIdle between flushes.

const (
	settleQuiet   = 250 * time.Millisecond
	settleMax     = time.Second
	settleDelay   = settleMax // longest wait for a pending change (tests)
	settingsIdle  = 30 * time.Second
	settingsRetry = time.Minute // backoff cap while the doorbell is unreachable
)

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
	hdrDone  []func() // run after the next flush wrote (or tried) the HDR switch
	sound    *unifi.SoundLed
	smart    *unifi.SmartDetect
	motion   *unifi.Motion
	osd      *unifi.OSD
	video    map[string]unifi.Video // channel -> frame rate / bitrate
	timer    *time.Timer
	first    time.Time     // first change since the last flush (settleMax)
	retry    time.Duration // current backoff after a failed dial, 0 = none
	closed   bool
	enabled  map[string]bool // Protect object types with smart detection on; nil = all

	// flushMu is held for a whole flush: a timer re-armed while a flush runs
	// fires a second one, which must not GET before the first one's SET.
	flushMu sync.Mutex
	ctx     context.Context // cancelled by close; nil = never
	cancel  context.CancelFunc
	dial    func(ctx context.Context) (*baichuan.Client, error) // for tests; nil = Bridge.dialControl
	client  *baichuan.Client                                    // kept between flushes; guarded by flushMu
	idle    *time.Timer                                         // closes client after settingsIdle
}

func newSettings(b *Bridge, apply bool) *settings {
	ctx, cancel := context.WithCancel(context.Background())
	return &settings{b: b, apply: apply, ctx: ctx, cancel: cancel}
}

// mergeFields copies the fields src has (non-nil pointers and slices) into
// dst: Protect sends changes as partial messages, which add to what is still
// pending instead of replacing it.
func mergeFields[T any](dst *T, src *T) {
	d, v := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem()
	for i := range v.NumField() {
		if !v.Type().Field(i).IsExported() {
			continue
		}
		f := v.Field(i)
		switch f.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			if !f.IsNil() {
				d.Field(i).Set(f)
			}
		default:
			d.Field(i).Set(f)
		}
	}
}

// merged returns old with new's fields on top (old may be nil).
func merged[T any](old, new *T) *T {
	if old == nil {
		return new
	}
	if new == nil {
		return old
	}
	out := *old
	mergeFields(&out, new)
	return &out
}

func (s *settings) ISP(v unifi.ISP) {
	s.mu.Lock()
	s.isp = merged(s.isp, &v)
	if v.HdrMode != nil && *v.HdrMode != s.hdrMode {
		s.hdrMode = *v.HdrMode
		s.hdrDirty = s.hdrOn != nil
	}
	s.mu.Unlock()
	s.schedule()
}

func (s *settings) HDR(on bool, done func()) {
	s.mu.Lock()
	s.hdrOn = &on
	s.hdrDirty = true
	if done != nil {
		if s.apply {
			s.hdrDone = append(s.hdrDone, done)
		} else {
			defer done()
		}
	}
	s.mu.Unlock()
	s.schedule()
}

func (s *settings) SoundLed(v unifi.SoundLed) {
	s.mu.Lock()
	s.sound = merged(s.sound, &v)
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

func (s *settings) OSD(v unifi.OSD) {
	s.mu.Lock()
	s.osd = &v
	s.mu.Unlock()
	s.schedule()
}

func (s *settings) Video(v unifi.Video) {
	s.mu.Lock()
	if s.video == nil {
		s.video = map[string]unifi.Video{}
	}
	s.video[v.Channel] = v
	s.mu.Unlock()
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
	s.retry = 0 // new values: try them soon
	now := time.Now()
	if s.first.IsZero() {
		s.first = now
	}
	s.arm(max(min(settleQuiet, settleMax-now.Sub(s.first)), 0))
}

// arm (re)starts the flush timer. Must be called with s.mu held.
func (s *settings) arm(d time.Duration) {
	if s.closed {
		return
	}
	if s.timer == nil {
		s.timer = time.AfterFunc(d, s.flush)
	} else {
		s.timer.Reset(d)
	}
}

// close stops the timer and waits for a running flush, which is cancelled:
// nothing of a stopped bridge writes to the doorbell.
func (s *settings) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.flushMu.Lock()
	s.dropClient()
	s.flushMu.Unlock()
}

// conn returns the kept control connection or dials one; reused tells
// which. Must be called with flushMu held.
func (s *settings) conn(ctx context.Context) (c *baichuan.Client, reused bool, err error) {
	if s.client != nil {
		return s.client, true, nil
	}
	dial := s.dial
	if dial == nil {
		dial = s.b.dialControl
	}
	if c, err = dial(ctx); err != nil {
		return nil, false, err
	}
	s.client = c
	return c, false, nil
}

// dropClient closes the kept connection. Must be called with flushMu held.
func (s *settings) dropClient() {
	if s.idle != nil {
		s.idle.Stop()
	}
	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
}

// keepClient closes the connection after settingsIdle without a flush. Must
// be called with flushMu held.
func (s *settings) keepClient() {
	if s.idle == nil {
		s.idle = time.AfterFunc(settingsIdle, func() {
			s.flushMu.Lock()
			defer s.flushMu.Unlock()
			s.dropClient()
		})
		return
	}
	s.idle.Reset(settingsIdle)
}

// pendingSet is what one flush applies.
type pendingSet struct {
	isp    *unifi.ISP
	hdr    bool // the HDR switch changed
	sound  *unifi.SoundLed
	smart  *unifi.SmartDetect
	motion *unifi.Motion
	osd    *unifi.OSD
	video  map[string]unifi.Video
}

func (p pendingSet) empty() bool {
	return p.isp == nil && !p.hdr && p.sound == nil && p.smart == nil && p.motion == nil && p.osd == nil && p.video == nil
}

// take empties the pending values. Must be called with s.mu held.
func (s *settings) take() pendingSet {
	p := pendingSet{isp: s.isp, hdr: s.hdrDirty && s.hdrOn != nil, sound: s.sound, smart: s.smart, motion: s.motion, osd: s.osd, video: s.video}
	s.isp, s.sound, s.smart, s.motion, s.osd, s.video = nil, nil, nil, nil, nil, nil
	s.hdrDirty = false
	return p
}

// putBack returns values a flush could not apply; values that arrived
// meanwhile are newer and win. Must be called with s.mu held.
func (s *settings) putBack(p pendingSet) {
	s.isp = merged(p.isp, s.isp)
	s.sound = merged(p.sound, s.sound)
	if s.smart == nil {
		s.smart = p.smart
	}
	if s.motion == nil {
		s.motion = p.motion
	}
	if s.osd == nil {
		s.osd = p.osd
	}
	for ch, v := range p.video {
		if s.video == nil {
			s.video = map[string]unifi.Video{}
		}
		if _, ok := s.video[ch]; !ok {
			s.video[ch] = v
		}
	}
	if p.hdr {
		s.hdrDirty = true
	}
}

// flush applies everything pending. Flushes run one at a time.
func (s *settings) flush() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	p := s.take()
	done := s.hdrDone
	s.hdrDone = nil
	defer func() {
		for _, f := range done {
			f()
		}
	}()
	s.first = time.Time{}
	hdr := ""
	if p.hdr {
		hdr = hdrSwitch(*s.hdrOn, s.hdrMode)
	}
	s.mu.Unlock()
	if p.empty() {
		return
	}

	base := s.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 60*time.Second)
	defer cancel()

	unreachable := func(err error) {
		s.mu.Lock()
		s.putBack(p)
		s.retry = min(max(2*s.retry, 5*time.Second), settingsRetry)
		retry := s.retry
		s.arm(retry)
		s.mu.Unlock()
		if base.Err() == nil {
			log.Warn().Err(err).Msgf("[settings] can't reach the doorbell, retrying in %s", retry)
		}
	}
	client, reused, err := s.conn(ctx)
	if err != nil {
		unreachable(err)
		return
	}
	s.mu.Lock()
	s.retry = 0
	s.mu.Unlock()

	ok := s.applyAll(ctx, client, p, hdr, reused)
	if !ok && reused && ctx.Err() == nil {
		// the kept connection may have gone stale (the doorbell drops idle
		// ones): all of it again on a fresh one; GET-modify-SET repeats safely
		s.dropClient()
		if client, _, err = s.conn(ctx); err != nil {
			unreachable(err)
			return
		}
		ok = s.applyAll(ctx, client, p, hdr, false)
	}
	if ok {
		s.keepClient()
	} else {
		s.dropClient()
	}
}

// applyAll writes one flush's values; it returns false if any group failed.
// quiet suppresses the warnings (a retry follows).
func (s *settings) applyAll(ctx context.Context, client *baichuan.Client, p pendingSet, hdr string, quiet bool) bool {
	isp, sound, smart, motion, osd, video := p.isp, p.sound, p.smart, p.motion, p.osd, p.video
	ch := uint8(s.b.opts.Doorbell.Channel)
	ok := true
	run := func(ctx context.Context, what string, f func() error) {
		if ctx.Err() != nil {
			ok = false
			return
		}
		if err := f(); err != nil {
			ok = false
			if quiet {
				log.Debug().Err(err).Msgf("[settings] %s (retrying on a new connection)", what)
			} else {
				log.Warn().Err(err).Msgf("[settings] %s", what)
			}
		}
	}

	if isp != nil || hdr != "" {
		run(ctx, "image", func() error { return applyImage(ctx, client, ch, isp, hdr) })
	}
	if isp != nil || sound != nil {
		run(ctx, "leds", func() error { return applyLeds(ctx, client, ch, isp, sound) })
	}
	if isp != nil && isp.NightVision() != "" {
		run(ctx, "day/night threshold", func() error {
			return update(ctx, client, baichuan.MsgDayNightGet, baichuan.MsgDayNightSet, ch, nil, dayNightChange(isp), "night vision threshold")
		})
	}
	if sound != nil {
		run(ctx, "audio", func() error { return applyAudio(ctx, client, ch, sound) })
	}
	if smart != nil {
		run(ctx, "ai", func() error { return applyAI(ctx, client, ch, smart) })
	}
	if motion != nil {
		run(ctx, "motion", func() error { return applyMotion(ctx, client, ch, motion) })
	}
	if isp != nil && isp.Masks != nil {
		masks := isp.Masks
		run(ctx, "privacy masks", func() error {
			return update(ctx, client, msgMaskGet, msgMaskSet, ch, nil, maskChange(masks), "privacy masks")
		})
	}
	if video != nil {
		run(ctx, "frame rate and bitrate", func() error { return s.b.applyVideo(ctx, client, ch, video) })
	}
	if osd != nil {
		run(ctx, "overlay", func() error {
			return update(ctx, client, msgOsdGet, msgOsdSet, ch, nil, osdChange(*osd), "overlay")
		})
	}
	return ok
}

// update runs GET -> change -> SET for one config group; change returns
// whether anything differs.
func update(ctx context.Context, c *baichuan.Client, get, set uint32, ch uint8, req []byte, change func(*baichuan.Node) bool, what string) error {
	body, err := c.GetConfig(ctx, get, ch, req)
	if err != nil {
		return err
	}
	debug := log.Debug().Enabled()
	var before []byte
	if debug {
		before = body.Marshal()
	}
	if !change(body) {
		return nil
	}
	if debug {
		log.Debug().Msgf("[settings] %s: doorbell had %s", what, before)
		log.Debug().Msgf("[settings] %s: writing %s", what, body.Marshal())
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
			if b.Find("InputAdvanceCfg/hdrSwitch") == nil {
				log.Info().Msg("[settings] the doorbell's picture settings have no HDR switch: HDR not applied")
			}
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
		var zones, exclude []unifi.Zone
		sens := -1
		for _, z := range sd.Zones {
			if contains(z.ObjectTypes, object) {
				zones = append(zones, z)
				sens = max(sens, z.Sensitivity)
			}
		}
		for _, z := range sd.ExcludeZones {
			if len(z.ObjectTypes) == 0 || contains(z.ObjectTypes, object) {
				exclude = append(exclude, z)
			}
		}
		err := update(ctx, c, baichuan.MsgAiDetectGet, baichuan.MsgAiDetectSet, ch, baichuan.AiDetectBody(ch, aiType),
			aiAreaChange(zones, exclude, sens), aiType+" detection area")
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
		// m.Enable is not "motion detection on" (Protect sends
		// enableMotionDetection && algorithm == enhanced). Protect's motion
		// events need the doorbell's motion detection, so it stays on.
		changed := b.Set("MD/enable", "1")
		var zones []unifi.Zone
		sens := -1
		for _, z := range m.Zones {
			zones = append(zones, z)
			sens = max(sens, 100-z.Level) // Protect sends level = 100 - sensitivity
		}
		if sens >= 0 && b.Set("MD/sensInfoNew/sensitivityDefault", strconv.Itoa(mdSensitivity(sens))) {
			changed = true
		}
		// The table is columns x rows, of which only the first width x height
		// cells are the picture (D340W: 80x60 in 96x64); the rest stays 0.
		// Without zones the doorbell's area is left alone.
		cols, _ := strconv.Atoi(b.Value("MD/scope/columns"))
		rows, _ := strconv.Atoi(b.Value("MD/scope/rows"))
		w, _ := strconv.Atoi(b.Value("MD/width"))
		h, _ := strconv.Atoi(b.Value("MD/height"))
		if w <= 0 || w > cols || h <= 0 || h > rows {
			w, h = cols, rows
		}
		if len(zones) > 0 && cols > 0 && rows > 0 && b.Set("MD/scope/valueTable", baichuan.EncodeArea(rasterizeIn(zones, w, h, cols, rows), cols, rows)) {
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
	return rasterizeIn(zones, cols, rows, cols, rows)
}

// rasterizeIn marks the cells of a w x h picture grid stored in a cols x rows
// table (row stride cols); cells outside the picture stay unset.
func rasterizeIn(zones []unifi.Zone, w, h, cols, rows int) []bool {
	cells := make([]bool, cols*rows)
	for r := 0; r < h; r++ {
		y := (float64(r) + 0.5) * 1000 / float64(h)
		for c := 0; c < w; c++ {
			x := (float64(c) + 0.5) * 1000 / float64(w)
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

// aiAreaChange sets one AI type's sensitivity (-1 = keep) and detection area:
// the cells of its zones without those of the exclusion zones.
func aiAreaChange(zones, exclude []unifi.Zone, sens int) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		changed := false
		if sens >= 0 && b.Set("AiDetectCfg/sensitivity", strconv.Itoa(clamp(sens, 0, 100))) {
			changed = true
		}
		cols, _ := strconv.Atoi(b.Value("AiDetectCfg/width"))
		rows, _ := strconv.Atoi(b.Value("AiDetectCfg/height"))
		if cols <= 0 || rows <= 0 {
			return changed
		}
		cells := rasterize(zones, cols, rows)
		for i, out := range rasterize(exclude, cols, rows) {
			if out {
				cells[i] = false
			}
		}
		if b.Set("AiDetectCfg/area", baichuan.EncodeArea(cells, cols, rows)) {
			changed = true
		}
		return changed
	}
}

// gopChange sets mainStream/subStream gop/cur to one second. Reolink
// firmware counts it either in frames or, like the D340W (dvr.xml
// main_gop_tm 1-2, gop_tm 1-4), in seconds: a range that tops out at 8 or
// less is seconds (no frame-counting encoder caps a GOP that low).
const gopSecondsMax = 8

func gopChange(b *baichuan.Node) bool {
	changed := false
	for _, stream := range []string{"mainStream", "subStream"} {
		fps, _ := strconv.Atoi(b.Value("Compression/" + stream + "/frame"))
		if fps <= 0 {
			continue
		}
		want := fps
		if hi, err := strconv.Atoi(b.Value("Compression/" + stream + "/gop/max")); err == nil && hi > 0 && hi <= gopSecondsMax {
			want = 1 // seconds
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

// Privacy masks (Shelter, 52/53) and the overlay (OSD, 44/45), from the D340W
// firmware (nets_shelter_get / nets_pic_shelter_x2s, nets_osd_*).
const (
	msgOsdGet  = 44
	msgOsdSet  = 45
	msgMaskGet = 52
	msgMaskSet = 53

	maxMasks = 4 // the firmware stores at most four rectangles
)

// maskChange replaces the doorbell's privacy masks with Protect's zones, each
// as its bounding box. Coordinates are packed (value << 16) | reference size,
// with Protect's 0-1000 space as the reference; ids not used are deleted.
func maskChange(masks []unifi.Mask) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		root := b.Find("Shelter")
		if root == nil {
			return false
		}
		list := root.Find("shelterList")
		if list == nil {
			list = &baichuan.Node{Name: "shelterList"}
			root.Children = append(root.Children, list)
		}

		want := map[int][4]int{}
		for i, m := range masks {
			r := boundingBox(m.Coord)
			if fullFrame(r) {
				// Protect's privacy mode adds a zone over the whole frame,
				// with a new (highest) id: it alone hides everything.
				want = map[int][4]int{0: r}
				break
			}
			if i >= maxMasks {
				log.Warn().Msgf("[settings] the doorbell takes %d privacy zones, zone %d ignored", maxMasks, m.ID)
				continue
			}
			want[i] = r
		}
		have := map[int][4]int{}
		for _, e := range list.Children {
			if e.Value("enable") == "0" {
				continue // deleted
			}
			id, _ := strconv.Atoi(e.Value("id"))
			have[id] = [4]int{unpack(e.Value("topLeftX")), unpack(e.Value("topLeftY")), unpack(e.Value("width")), unpack(e.Value("height"))}
		}

		enable := boolStr(len(want) > 0)
		if root.Value("enable") == enable && len(want) == len(have) && sameMasks(want, have) {
			return false
		}

		root.Set("enable", enable)
		list.Children = nil
		for id := 0; id < maxMasks; id++ {
			r, ok := want[id]
			e := &baichuan.Node{Name: "Shelter", Children: []*baichuan.Node{
				{Name: "id", Text: strconv.Itoa(id)},
				{Name: "enable", Text: boolStr(ok)}, // 0 deletes the entry
			}}
			if ok {
				e.Children = append(e.Children,
					&baichuan.Node{Name: "topLeftX", Text: strconv.Itoa(pack(r[0]))},
					&baichuan.Node{Name: "topLeftY", Text: strconv.Itoa(pack(r[1]))},
					&baichuan.Node{Name: "width", Text: strconv.Itoa(pack(r[2]))},
					&baichuan.Node{Name: "height", Text: strconv.Itoa(pack(r[3]))},
				)
			}
			list.Children = append(list.Children, e)
		}
		return true
	}
}

const maskRef = 1000 // reference size: Protect's coordinate space

func pack(v int) int { return v<<16 | maskRef }

// unpack returns a value in the 0-1000 space from a packed coordinate.
func unpack(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	ref := v & 0xffff
	if ref == 0 {
		return -1
	}
	return (v >> 16) * maskRef / ref
}

// fullFrame reports a box covering the whole picture (in 0-1000, allowing
// for rounding).
func fullFrame(r [4]int) bool {
	return r[0] <= 1 && r[1] <= 1 && r[0]+r[2] >= 999 && r[1]+r[3] >= 999
}

func sameMasks(a, b map[int][4]int) bool {
	for id, r := range a {
		o, ok := b[id]
		if !ok {
			return false
		}
		for i := range r {
			if abs(r[i]-o[i]) > 1 { // rounding from another reference size
				return false
			}
		}
	}
	return true
}

// boundingBox returns [x, y, w, h] of a polygon in 0-1000.
func boundingBox(coord []float64) [4]int {
	minX, minY, maxX, maxY := 1000.0, 1000.0, 0.0, 0.0
	for i := 0; i+1 < len(coord); i += 2 {
		minX, maxX = math.Min(minX, coord[i]), math.Max(maxX, coord[i])
		minY, maxY = math.Min(minY, coord[i+1]), math.Max(maxY, coord[i+1])
	}
	x0, y0 := clamp(int(math.Floor(minX)), 0, 1000), clamp(int(math.Floor(minY)), 0, 1000)
	x1, y1 := clamp(int(math.Ceil(maxX)), 0, 1000), clamp(int(math.Ceil(maxY)), 0, 1000)
	return [4]int{x0, y0, max(x1-x0, 0), max(y1-y0, 0)}
}

// OSD anchor codes (topLeftX, topLeftY) of the six Reolink positions.
var osdAnchors = map[string][2]string{
	"topLeft": {"1", "1"}, "topMiddle": {"65537", "1"}, "topRight": {"65536", "1"},
	"bottomLeft": {"1", "65536"}, "bottomMiddle": {"65537", "65536"}, "bottomRight": {"65536", "65536"},
}

// osdChange sets the name and date overlay: the name at Protect's position,
// the date on the other vertical edge. Protect's "logo" means the UniFi logo,
// so the Reolink watermark stays off.
func osdChange(o unifi.OSD) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		changed := false
		set := func(path, v string) {
			if b.Set(path, v) {
				changed = true
			}
		}
		if name := osdName(o.Name); name != "" {
			set("OsdChannelName/name", name)
			set("OsdChannelName/enable", "1")
		} else {
			set("OsdChannelName/enable", "0")
		}
		set("OsdDatetime/enable", boolStr(o.Date))
		set("OsdChannelName/enWatermark", "0")
		if pos, ok := osdAnchors[o.Location]; ok {
			set("OsdChannelName/topLeftX", pos[0])
			set("OsdChannelName/topLeftY", pos[1])
			date := "65536" // bottom
			if pos[1] == "65536" {
				date = "1" // top
			}
			set("OsdDatetime/topLeftX", pos[0])
			set("OsdDatetime/topLeftY", date)
		}
		return changed
	}
}

// osdName makes a camera name acceptable to the doorbell: ASCII only (umlauts
// transliterated), none of $&*/:;<>?\`| , at most 31 bytes.
func osdName(s string) string {
	s = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "Ä", "Ae", "Ö", "Oe", "Ü", "Ue", "ß", "ss").Replace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r >= 0x7f || strings.ContainsRune("$&*/:;<>?\\`|", r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > 31 {
		out = strings.TrimSpace(out[:31])
	}
	return out
}

// Protect's custom night vision: a slider 0-10, meaning "switch to night below
// this many lux" (swai: 0 = <1 lux, then 1, 3, 5, 7, 10, 12, 15, 20, 25, 30).
var nightLux = [11]float64{0.5, 1, 3, 5, 7, 10, 12, 15, 20, 25, 30}

// dayNightChange sets the doorbell's day/night switch threshold (296/297
// DayNightThreshold): with Protect's custom night vision a custom value, the
// lux level mapped proportionally onto the doorbell's range (higher switches
// to night earlier); otherwise the doorbell's default threshold.
func dayNightChange(isp *unifi.ISP) func(*baichuan.Node) bool {
	return func(b *baichuan.Node) bool {
		if isp.NightVision() != "custom" || isp.IcrCustomValue == nil {
			// The D340W ignores "default" (it keeps "custom"), so writing it
			// only repeats on every change: leave the doorbell's threshold.
			return false
		}
		lo, err1 := strconv.Atoi(b.Value("DayNightThreshold/thresholdval/min"))
		hi, err2 := strconv.Atoi(b.Value("DayNightThreshold/thresholdval/max"))
		if err1 != nil || err2 != nil || hi <= lo {
			lo, hi = 0, 100
		}
		lux := nightLux[clamp(*isp.IcrCustomValue, 0, 10)]
		cur := lo + int(math.Round(lux/nightLux[10]*float64(hi-lo)))
		changed := b.Set("DayNightThreshold/threshold", "custom")
		if b.Set("DayNightThreshold/thresholdval/cur", strconv.Itoa(cur)) {
			changed = true
		}
		return changed
	}
}

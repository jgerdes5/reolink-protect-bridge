package unifi

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

type settingsLog struct {
	mu    sync.Mutex
	isp   []ISP
	sound []SoundLed
	smart []SmartDetect
	mot   []Motion
	hdr   []bool
}

func (s *settingsLog) ISP(v ISP)           { s.mu.Lock(); s.isp = append(s.isp, v); s.mu.Unlock() }
func (s *settingsLog) SoundLed(v SoundLed) { s.mu.Lock(); s.sound = append(s.sound, v); s.mu.Unlock() }
func (s *settingsLog) SmartDetect(v SmartDetect) {
	s.mu.Lock()
	s.smart = append(s.smart, v)
	s.mu.Unlock()
}
func (s *settingsLog) Motion(v Motion) { s.mu.Lock(); s.mot = append(s.mot, v); s.mu.Unlock() }
func (s *settingsLog) HDR(on bool)     { s.mu.Lock(); s.hdr = append(s.hdr, on); s.mu.Unlock() }

func TestApplySettings(t *testing.T) {
	log := &settingsLog{}
	cam := &Camera{Settings: log, Log: zerolog.Nop()}

	cam.applySetting("ChangeIspSettings", json.RawMessage(`{"brightness":70,"mirror":1,"irLedMode":"manual","irLedLevel":0,"hdrMode":"normal"}`))
	cam.applySetting("ChangeSoundLedSettings", json.RawMessage(`{}`)) // syncCustomSound: not a setting
	cam.applySetting("ChangeSoundLedSettings", json.RawMessage(`{"ledFaceEnabled":0,"speakerVolume":80,"ringVolume":40}`))
	cam.applySetting("ChangeSmartDetectSettings", json.RawMessage(`{"enableSmartDetect":["person"],
		"zones":{"1":{"coord":[0,0,1000,0,1000,1000,0,1000],"sensitivity":60,"objectTypes":["person"]}},
		"lines":{"2":{"coord":[200,600,800,600,500,0],"crosslineDirection":"A2B","objectTypes":["person"]}}}`))
	cam.applySetting("ChangeSmartMotionSettings", json.RawMessage(`{"enable":true,"zones":{"1":{"coord":[0,0,500,0,500,500,0,500],"level":30}}}`))
	cam.applySetting("ChangeVideoSettings", json.RawMessage(`{"video":{"video1":{"fps":20}}}`)) // no HDR in it
	cam.applySetting("ChangeVideoSettings", json.RawMessage(`{"video":{"hdrMode":1}}`))

	require.Eventually(t, func() bool {
		log.mu.Lock()
		defer log.mu.Unlock()
		return len(log.hdr) == 1
	}, time.Second, time.Millisecond)

	log.mu.Lock()
	defer log.mu.Unlock()
	require.Len(t, log.isp, 1)
	require.Equal(t, 70, *log.isp[0].Brightness)
	require.Nil(t, log.isp[0].Contrast)
	require.Equal(t, "off", log.isp[0].NightVision())
	require.Len(t, log.sound, 1)
	require.Equal(t, 40, *log.sound[0].RingVolume)
	require.Equal(t, []string{"person"}, log.smart[0].Enabled)
	require.Equal(t, 60, log.smart[0].Zones["1"].Sensitivity)
	require.Equal(t, "A2B", log.smart[0].Lines["2"].Direction)
	require.True(t, *log.mot[0].Enable)
	require.Equal(t, 30, log.mot[0].Zones["1"].Level)
	require.Equal(t, []bool{true}, log.hdr)

	cam.mu.Lock()
	require.True(t, cam.hdr) // echoed in the next ChangeVideoSettings reply
	cam.mu.Unlock()
}

func TestNightVision(t *testing.T) {
	s := func(v string) *string { return &v }
	i := func(v int) *int { return &v }
	require.Equal(t, "", ISP{}.NightVision())
	require.Equal(t, "auto", ISP{IrLedMode: s("auto"), IrLedLevel: i(255), IcrSwitchMode: s("sensitivity")}.NightVision())
	require.Equal(t, "custom", ISP{IrLedMode: s("auto"), IrLedLevel: i(255), IcrSwitchMode: s("lux")}.NightVision())
	require.Equal(t, "on", ISP{IrLedMode: s("manual"), IrLedLevel: i(255)}.NightVision())
	require.Equal(t, "off", ISP{IrLedMode: s("manual"), IrLedLevel: i(0)}.NightVision())
}

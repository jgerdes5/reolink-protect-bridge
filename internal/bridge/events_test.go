package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/stretchr/testify/require"
)

type sinkLog struct{ calls []string }

func (s *sinkLog) Motion(on bool) {
	if on {
		s.calls = append(s.calls, "motion")
	} else {
		s.calls = append(s.calls, "motion end")
	}
}
func (s *sinkLog) SmartDetect(o string) { s.calls = append(s.calls, "smart "+o) }
func (s *sinkLog) Ring(context.Context) { s.calls = append(s.calls, "ring") }
func (s *sinkLog) ClearEvents()         { s.calls = append(s.calls, "clear") }

func TestAlarmState(t *testing.T) {
	var st alarmState
	sink := &sinkLog{}
	ctx := context.Background()
	apply := func(ev baichuan.AlarmEvent) { st.apply(ctx, ev, sink) }

	apply(baichuan.AlarmEvent{Motion: true})
	apply(baichuan.AlarmEvent{Motion: true, AI: []string{"dog_cat", "people"}}) // person wins
	apply(baichuan.AlarmEvent{Motion: true, AI: []string{"vehicle"}})           // still the same event
	apply(baichuan.AlarmEvent{Motion: true, Visitor: true})                     // AI gone, button
	apply(baichuan.AlarmEvent{Motion: true, Visitor: true})                     // held: one ring
	apply(baichuan.AlarmEvent{})
	apply(baichuan.AlarmEvent{Visitor: true}) // second press
	require.Equal(t, []string{
		"motion", "smart person", "ring", "smart ", "motion end", "ring",
	}, sink.calls)

	// quiet camera: open events are cleared after staleAfter
	sink.calls = nil
	apply(baichuan.AlarmEvent{Motion: true})
	st.expire(sink)
	require.Equal(t, []string{"motion"}, sink.calls)
	st.last = time.Now().Add(-staleAfter - time.Second)
	st.expire(sink)
	require.Equal(t, []string{"motion", "clear"}, sink.calls)
	require.False(t, st.motion)
}

func TestAlarmStatePackage(t *testing.T) {
	sink := &sinkLog{}
	ctx := context.Background()

	var st alarmState
	st.apply(ctx, baichuan.AlarmEvent{AI: []string{"package"}}, sink)
	st.apply(ctx, baichuan.AlarmEvent{AI: []string{"package", "people"}}, sink) // same event
	st.apply(ctx, baichuan.AlarmEvent{}, sink)
	require.Equal(t, []string{"smart package", "smart "}, sink.calls)

	// switched off in Protect: not forwarded
	sink.calls = nil
	off := alarmState{enabled: func(o string) bool { return o != "package" }}
	off.apply(ctx, baichuan.AlarmEvent{AI: []string{"package"}}, sink)
	require.Empty(t, sink.calls)
}

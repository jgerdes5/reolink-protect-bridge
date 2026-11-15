package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// Events come from the Reolink doorbell itself over Baichuan push (port 9000),
// one extra connection next to the video: motion, the doorbell's AI classes
// (people/vehicle/dog_cat) and the button ("visitor").

// eventSink is what alarms are turned into (unifi.Camera).
type eventSink interface {
	Motion(on bool)
	SmartDetect(object string)
	Ring(ctx context.Context)
	ClearEvents()
}

// aiObjects maps Reolink AI types to Protect object types, in priority order.
var aiObjects = []struct{ reolink, protect string }{
	{"people", "person"}, {"vehicle", "vehicle"}, {"dog_cat", "animal"}, {"package", "package"},
}

// staleAfter is the safety net for an event whose end was lost (a dropped
// push). Reolink pushes only on changes, so a person standing still sends
// nothing for a long time; a dropped connection clears events by itself.
const staleAfter = 10 * time.Minute

type alarmState struct {
	motion  bool
	smart   string
	visitor bool
	last    time.Time

	// enabled filters the AI types before one is chosen: a type switched
	// off in Protect must not hold the event and hide a later person.
	enabled func(object string) bool // nil = all
}

func (s *alarmState) apply(ctx context.Context, ev baichuan.AlarmEvent, sink eventSink) {
	s.last = time.Now()

	if ev.Visitor && !s.visitor {
		sink.Ring(ctx) // once per press
	}
	s.visitor = ev.Visitor

	smart := ""
	for _, o := range aiObjects {
		if smart != "" || (s.enabled != nil && !s.enabled(o.protect)) {
			continue
		}
		for _, t := range ev.AI {
			if t == o.reolink {
				smart = o.protect
			}
		}
	}
	if (smart == "") != (s.smart == "") {
		s.smart = smart
		sink.SmartDetect(smart)
	}

	if ev.Motion != s.motion {
		s.motion = ev.Motion
		sink.Motion(ev.Motion)
	}
}

func (s *alarmState) expire(sink eventSink) {
	if (s.motion || s.smart != "") && time.Since(s.last) > staleAfter {
		log.Debug().Msg("[events] no doorbell events for 10 min, clearing")
		s.clear(sink)
	}
}

func (s *alarmState) clear(sink eventSink) {
	s.motion, s.smart, s.visitor = false, "", false
	sink.ClearEvents()
}

// runEvents keeps the Baichuan push connection up, reconnecting with backoff.
func (b *Bridge) runEvents(ctx context.Context) {
	d := b.opts.Doorbell
	cfg := baichuan.Config{Host: d.Host, Port: d.Port, Username: d.Username, Password: d.Password}
	sink := &recordingSink{eventSink: b.proto, b: b, detect: b.proto.Detections}

	backoff := 2 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := b.eventsOnce(ctx, cfg, d.Channel, sink)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 5*time.Minute {
			backoff = 2 * time.Second
		}
		log.Warn().Err(err).Msgf("[events] doorbell events down, retrying in %s", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

func (b *Bridge) eventsOnce(ctx context.Context, cfg baichuan.Config, channel int, sink eventSink) error {
	client, err := baichuan.Dial(ctx, cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	alarms, stop, err := client.SubscribeAlarms(ctx)
	if err != nil {
		return err
	}
	defer stop()
	log.Info().Msgf("[events] doorbell events connected (%s)", cfg.Host)
	b.setEventsUp(true)
	defer b.setEventsUp(false)

	state := alarmState{enabled: b.settings.smartEnabled}
	defer state.clear(sink)

	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case events, ok := <-alarms:
			if !ok {
				if err = client.Err(); err == nil {
					err = errors.New("connection closed")
				}
				return err
			}
			for _, ev := range events {
				if ev.Channel == channel {
					log.Trace().Msgf("[events] alarm %+v", ev)
					state.apply(ctx, ev, sink)
				}
			}
		case <-tick.C:
			state.expire(sink)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (b *Bridge) setEventsUp(up bool) {
	b.mu.Lock()
	b.eventsUp = up
	b.mu.Unlock()
}

// recordingSink passes events to Protect and notes them for the status page.
type recordingSink struct {
	eventSink
	b      *Bridge
	detect func(unifi.Detection) // Camera.Detections; nil in tests
}

func (r *recordingSink) Motion(on bool) {
	if on {
		r.b.addEvent("motion")
	}
	r.eventSink.Motion(on)
}

func (r *recordingSink) SmartDetect(object string) {
	if object != "" {
		if !r.b.settings.smartEnabled(object) {
			return // switched off in Protect
		}
		r.b.addEvent(object)
	}
	// zones still occupied when the object is gone are left: report that
	// while the event is still open
	if last := r.b.tracker.setObject(object); r.detect != nil && len(last.Boxes) > 0 {
		last.Final = true
		r.detect(last)
	}
	r.eventSink.SmartDetect(object)
}

func (r *recordingSink) Ring(ctx context.Context) {
	r.b.addEvent("ring")
	r.eventSink.Ring(ctx)
}

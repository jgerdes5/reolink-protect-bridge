package baichuan

import (
	"context"
	"encoding/xml"
	"errors"
	"log"
	"strings"
	"sync"
)

// AlarmEvent is one channel's state from a push event (msg 33): motion, the
// on-camera AI classes and, on doorbells, the button ("visitor").
type AlarmEvent struct {
	Channel int
	Motion  bool     // "MD" in status
	Visitor bool     // doorbell button pressed
	AI      []string // e.g. "people", "vehicle", "dog_cat"
}

// alarmChannel is the channel id the subscription is sent on (as reolink_aio does).
const alarmChannel = 251

// SubscribeAlarms asks the camera to push alarm events (msg 31) and returns
// them parsed. The channel closes when the connection ends; stop unsubscribes.
func (c *Client) SubscribeAlarms(ctx context.Context) (<-chan []AlarmEvent, func(), error) {
	if err := c.Login(ctx); err != nil {
		return nil, nil, err
	}

	sub, unsubscribe := c.Subscribe(msgIDMotion)
	// Cameras answer the subscription; one that only starts pushing is fine too.
	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	_, err := c.sendRequest(reqCtx, request{
		MsgID:     msgIDMotionRequest,
		ChannelID: alarmChannel,
		Class:     classModernWithOffset,
	})
	cancel()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		unsubscribe()
		return nil, nil, err
	}

	out := make(chan []AlarmEvent, 16)
	stop := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case msg := <-sub:
				events := ParseAlarms(msg.XML)
				if len(events) == 0 {
					continue
				}
				select {
				case out <- events:
				default: // a stalled reader must not block the connection
					log.Printf("[baichuan] WARNING: alarm reader busy, event dropped (msgNum=%d)", msg.Header.MsgNum)
				}
			case <-stop:
				return
			case <-c.closed:
				return
			}
		}
	}()

	var once sync.Once
	return out, func() {
		once.Do(func() {
			close(stop)
			unsubscribe()
		})
	}, nil
}

// ParseAlarms decodes the AlarmEvent entries of a msg 33 body:
//
//	<body><AlarmEventList><AlarmEvent version="1.1">
//	  <channelId>0</channelId><status>MD,visitor</status><AItype>people</AItype>
//	</AlarmEvent></AlarmEventList></body>
//
// Other event kinds in the same message (e.g. DayNightEvent) are skipped.
func ParseAlarms(s string) []AlarmEvent {
	var body struct {
		Events []struct {
			Channel *int   `xml:"channelId"`
			Status  string `xml:"status"`
			AIType  string `xml:"AItype"`
		} `xml:"AlarmEventList>AlarmEvent"`
	}
	if err := xml.Unmarshal([]byte(s), &body); err != nil {
		return nil
	}

	var events []AlarmEvent
	for _, e := range body.Events {
		if e.Channel == nil {
			continue
		}
		ev := AlarmEvent{Channel: *e.Channel}
		for _, s := range strings.Split(e.Status, ",") {
			switch strings.TrimSpace(s) {
			case "MD":
				ev.Motion = true
			case "visitor":
				ev.Visitor = true
			}
		}
		for _, t := range strings.Split(e.AIType, ",") {
			switch t = strings.TrimSpace(t); t {
			case "", "none":
			case "other":
				ev.Motion = true // reolink_aio: "other" counts as motion
			default:
				ev.AI = append(ev.AI, t)
			}
		}
		events = append(events, ev)
	}
	return events
}

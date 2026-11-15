package unifi

import (
	"strconv"
	"time"
)

// Real detection boxes. A UniFi camera does the geometry itself (Protect only
// reads it): each object's box ([x, y, w, h] in 0-1000), the zones it is in and
// the lines it crossed. The caller computes these against Protect's zones;
// during a smart-detect event they go to Protect as "moving" frames.

// Box is one detected object.
type Box struct {
	X, Y, W, H int   // 0-1000
	Confidence int   // 0-100
	TrackerID  int   // stable per object; 0 = assign from the event
	Zones      []int // smart-detect zone ids the object is in
	Lines      []int // line ids crossed in this frame
	Loiter     []int // loiter zone ids the object has stayed in long enough
}

func (b Box) coord() []int { return []int{b.X, b.Y, b.W, b.H} }

// LineCross is a line's crossing counts for the event's object type.
type LineCross struct {
	A2B, B2A       int    // totals in this event
	A2BAdd, B2AAdd int    // this frame
	Status         string // enter | moving | leave | none
}

// Detection is one frame's objects and zone/line states.
type Detection struct {
	Boxes  []Box
	Zones  map[int]string    // zone id -> enter | moving | leave
	Lines  map[int]LineCross // line id -> counts
	Loiter map[int]string    // loiter zone id -> enter | moving | leave
	// Final marks the last frame of an event (zones left as the object is
	// gone): sent even right after another frame, and not kept as the boxes
	// the next event starts with.
	Final bool
}

const (
	detectionFresh = 1500 * time.Millisecond // boxes this old still describe the event start
	movingInterval = 500 * time.Millisecond  // live box updates; displayTimeoutMSec is 600
)

// Detections reports the latest boxes. Without an open smart-detect event they
// are only kept for the next event's start frame and crop.
func (c *Camera) Detections(d Detection) {
	if len(d.Boxes) == 0 {
		return
	}
	c.ev.mu.Lock()
	defer c.ev.mu.Unlock()
	if !d.Final {
		c.ev.det, c.ev.detAt = d, time.Now()
	}

	e := c.ev.smart
	if e == nil || (!d.Final && time.Since(c.ev.movingAt) < movingInterval) {
		return
	}
	c.ev.movingAt = time.Now()
	e.box = d.Boxes[largest(d.Boxes)].coord()

	p := c.motionPayload(e, "moving")
	p["objectTypes"] = []string{e.object}
	p["zonesStatus"] = zoneStatuses(d.Zones)
	if len(d.Lines) > 0 {
		p["linesStatus"] = lineStatuses(d.Lines, e.object)
	}
	if len(d.Loiter) > 0 {
		p["loiterZonesStatus"] = zoneStatuses(d.Loiter)
	}
	p["displayTimeoutMSec"] = smartDisplayMs
	p["descriptors"] = descriptors(e, d.Boxes)
	p["smartDetectSnapshotFullFoV"] = ""
	p["smartDetectSnapshotFullFoVHeight"] = 0
	p["smartDetectSnapshotFullFoVWidth"] = 0
	p["smartDetectSnapshots"] = []any{}
	c.enqueue(func() { c.sendEvent("EventSmartDetect", p) })
}

// freshDetection returns the latest boxes if they are recent. Must be called
// with c.ev.mu held.
func (c *Camera) freshDetection() (Detection, bool) {
	if len(c.ev.det.Boxes) == 0 || time.Since(c.ev.detAt) > detectionFresh {
		return Detection{}, false
	}
	return c.ev.det, true
}

func largest(boxes []Box) int {
	best := 0
	for i, b := range boxes {
		if b.W*b.H > boxes[best].W*boxes[best].H {
			best = i
		}
	}
	return best
}

// descriptors turns boxes into descriptors of the event's object type.
func descriptors(e *event, boxes []Box) []any {
	out := make([]any, 0, len(boxes))
	for i, b := range boxes {
		d := descriptor(e)
		id := b.TrackerID
		if id == 0 {
			id = trackerID(e) + i
		}
		d["trackerID"] = id
		d["coord"] = b.coord()
		if b.Confidence > 0 {
			d["confidenceLevel"] = b.Confidence
		}
		d["zones"] = nonNil(b.Zones)
		d["lines"] = nonNil(b.Lines)
		d["loiterZones"] = nonNil(b.Loiter)
		out = append(out, d)
	}
	return out
}

func nonNil(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}

func zoneStatuses(zones map[int]string) map[string]any {
	out := map[string]any{}
	for id, status := range zones {
		out[strconv.Itoa(id)] = map[string]any{"level": smartZoneLevel, "status": status}
	}
	return out
}

func lineStatuses(lines map[int]LineCross, object string) map[string]any {
	out := map[string]any{}
	for id, l := range lines {
		status := l.Status
		if status == "" {
			status = "none"
		}
		out[strconv.Itoa(id)] = map[string]any{
			object: map[string]any{
				"crosslineA2B": l.A2B, "crosslineB2A": l.B2A,
				"crosslineA2BAdd": l.A2BAdd, "crosslineB2AAdd": l.B2AAdd,
			},
			"level":  smartZoneLevel,
			"status": status,
		}
	}
	return out
}

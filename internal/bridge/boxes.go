package bridge

import (
	"strconv"
	"sync"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// tracker turns the doorbell's AI boxes into what a UniFi camera reports:
// boxes in 0-1000, the smart-detect zones each object is in (enter / moving /
// leave) and line crossings, evaluated against Protect's zones and lines. The
// test point is the bottom centre of a box, where a person stands; Protect
// does no geometry itself. State is per smart-detect event.
type tracker struct {
	mu     sync.Mutex
	zones  map[int]unifi.Zone
	lines  map[int]unifi.Line
	loiter map[int]unifi.Zone
	object string // current smart-detect object type, "" between events

	inZone   map[int]bool
	side     map[[2]int]float64 // (track, line) -> side of the line last frame
	counts   map[int]*unifi.LineCross
	since    map[int]time.Time // loiter zone -> since when an object is inside
	loiterOn map[int]bool      // loiter zones triggered in this event
	now      func() time.Time  // for tests
}

func newTracker() *tracker {
	t := &tracker{now: time.Now}
	t.reset()
	return t
}

func (t *tracker) reset() {
	t.inZone = map[int]bool{}
	t.side = map[[2]int]float64{}
	t.counts = map[int]*unifi.LineCross{}
	t.since = map[int]time.Time{}
	t.loiterOn = map[int]bool{}
}

// setZones takes Protect's smart-detect zones and lines.
func (t *tracker) setZones(zones map[string]unifi.Zone, lines map[string]unifi.Line, loiter map[string]unifi.Zone) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.zones, t.lines, t.loiter = map[int]unifi.Zone{}, map[int]unifi.Line{}, map[int]unifi.Zone{}
	for k, z := range loiter {
		if id, err := strconv.Atoi(k); err == nil {
			t.loiter[id] = z
		}
	}
	for k, z := range zones {
		if id, err := strconv.Atoi(k); err == nil {
			t.zones[id] = z
		}
	}
	for k, l := range lines {
		if id, err := strconv.Atoi(k); err == nil && len(l.Coord) >= 6 {
			t.lines[id] = l
		}
	}
}

// setObject starts ("person", ...) or ends ("") a smart-detect event.
func (t *tracker) setObject(object string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if object == "" || t.object == "" {
		t.reset()
	}
	t.object = object
}

// frame evaluates one frame's boxes (in the w x h space of the AI data).
func (t *tracker) frame(raw []baichuan.AIBox, w, h int) unifi.Detection {
	t.mu.Lock()
	defer t.mu.Unlock()

	det := unifi.Detection{Zones: map[int]string{}, Lines: map[int]unifi.LineCross{}, Loiter: map[int]string{}}
	now := map[int]bool{}
	inLoiter := map[int]bool{}
	clock := t.now()
	for i, r := range raw {
		b := unifi.Box{
			X: r.X1 * 1000 / w, Y: r.Y1 * 1000 / h,
			W: (r.X2 - r.X1) * 1000 / w, H: (r.Y2 - r.Y1) * 1000 / h,
			Confidence: clamp(r.Confidence, 0, 100),
		}
		track := i
		if r.TrackID >= 0 {
			track = r.TrackID
			b.TrackerID = 1000 + r.TrackID // stable across frames
		}
		px, py := float64(b.X)+float64(b.W)/2, float64(b.Y+b.H)

		for id, z := range t.zones {
			if t.object != "" && len(z.ObjectTypes) > 0 && !contains(z.ObjectTypes, t.object) {
				continue
			}
			if inPolygon(z.Coord, px, py) {
				b.Zones = append(b.Zones, id)
				now[id] = true
			}
		}

		for id, z := range t.loiter {
			wait, ok := loiterTime(z, t.object)
			if !ok || !inPolygon(z.Coord, px, py) {
				continue
			}
			inLoiter[id] = true
			if _, in := t.since[id]; !in {
				t.since[id] = clock
			}
			if clock.Sub(t.since[id]) >= wait {
				b.Loiter = append(b.Loiter, id)
			}
		}

		for id, l := range t.lines {
			if t.object != "" && len(l.ObjectTypes) > 0 && !contains(l.ObjectTypes, t.object) {
				continue
			}
			key := [2]int{track, id}
			s := sideOf(l.Coord, px, py)
			prev, seen := t.side[key]
			t.side[key] = s
			if !seen || s == 0 || prev == 0 || (s > 0) == (prev > 0) || !withinSegment(l.Coord, px, py) {
				continue
			}
			// crossed: from the reference point A's side towards the other is A2B
			fromA := (prev > 0) == (sideOf(l.Coord, l.Coord[4], l.Coord[5]) > 0)
			if (fromA && l.Direction == "B2A") || (!fromA && l.Direction == "A2B") {
				continue // not the direction this line counts
			}
			c := t.counts[id]
			if c == nil {
				c = &unifi.LineCross{}
				t.counts[id] = c
			}
			cross := unifi.LineCross{Status: "enter"}
			if fromA {
				c.A2B++
				cross.A2BAdd = 1
			} else {
				c.B2A++
				cross.B2AAdd = 1
			}
			cross.A2B, cross.B2A = c.A2B, c.B2A
			det.Lines[id] = cross
			b.Lines = append(b.Lines, id)
		}
		det.Boxes = append(det.Boxes, b)
	}

	for id, start := range t.since {
		wait, _ := loiterTime(t.loiter[id], t.object)
		switch {
		case !inLoiter[id]:
			delete(t.since, id)
			if t.loiterOn[id] {
				det.Loiter[id] = "leave"
				delete(t.loiterOn, id)
			}
		case clock.Sub(start) >= wait && !t.loiterOn[id]:
			t.loiterOn[id] = true
			det.Loiter[id] = "enter"
		case t.loiterOn[id]:
			det.Loiter[id] = "moving"
		}
	}

	for id := range now {
		if t.inZone[id] {
			det.Zones[id] = "moving"
		} else {
			det.Zones[id] = "enter"
		}
	}
	for id := range t.inZone {
		if !now[id] {
			det.Zones[id] = "leave"
		}
	}
	t.inZone = now
	return det
}

// sideOf is the cross product sign of a point against the line P1->P2
// (coord = [x1,y1,x2,y2,ax,ay]).
func sideOf(c []float64, x, y float64) float64 {
	return (c[2]-c[0])*(y-c[1]) - (c[3]-c[1])*(x-c[0])
}

// withinSegment reports whether the point projects onto the segment P1-P2
// (crossing the line beside its ends does not count).
func withinSegment(c []float64, x, y float64) bool {
	dx, dy := c[2]-c[0], c[3]-c[1]
	l2 := dx*dx + dy*dy
	if l2 == 0 {
		return false
	}
	t := ((x-c[0])*dx + (y-c[1])*dy) / l2
	return t >= 0 && t <= 1
}

// loiterTime is how long the object type must stay in a loiter zone; ok is
// false if the zone does not watch that type. Without a type (between events)
// the shortest time counts.
func loiterTime(z unifi.Zone, object string) (time.Duration, bool) {
	best, ok := 0, false
	for _, lt := range z.LoiterTimes {
		if object != "" && lt.ObjectType != object {
			continue
		}
		if !ok || lt.MSec < best {
			best, ok = lt.MSec, true
		}
	}
	return time.Duration(best) * time.Millisecond, ok
}

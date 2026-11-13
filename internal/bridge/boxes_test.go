package bridge

import (
	"testing"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/stretchr/testify/require"
)

// a person walking right across a 1000x1000 frame
func walk(x int) []baichuan.AIBox {
	return []baichuan.AIBox{{X1: x, Y1: 200, X2: x + 100, Y2: 700, Confidence: 80, TrackID: 3}}
}

// stepClock advances a tracker's clock by step on every frame.
func stepClock(tr *tracker, step time.Duration) {
	clock := time.Unix(1000, 0)
	tr.now = func() time.Time {
		clock = clock.Add(step)
		return clock
	}
}

func TestTrackerZonesAndLines(t *testing.T) {
	tr := newTracker()
	stepClock(tr, time.Second) // frames further apart than latchFor
	tr.setZones(map[string]unifi.Zone{
		"1": {Coord: []float64{0, 0, 500, 0, 500, 1000, 0, 1000}, ObjectTypes: []string{"person"}}, // left half
		"2": {Coord: []float64{0, 0, 1000, 0, 1000, 1000, 0, 1000}, ObjectTypes: []string{"vehicle"}},
	}, map[string]unifi.Line{
		// vertical line at x=600, A side = left (reference point at x<600)
		"5": {Coord: []float64{600, 0, 600, 1000, 100, 500}, Direction: "none", ObjectTypes: []string{"person"}},
		"6": {Coord: []float64{600, 0, 600, 1000, 100, 500}, Direction: "B2A"},
	}, nil)
	tr.setObject("person")

	d := tr.frame(walk(100), 1000, 1000) // foot point x=150
	require.Equal(t, unifi.Box{X: 100, Y: 200, W: 100, H: 500, Confidence: 80, TrackerID: 1003, Zones: []int{1}}, d.Boxes[0])
	require.Equal(t, map[int]string{1: "enter"}, d.Zones) // zone 2 is for vehicles
	require.Empty(t, d.Lines)

	d = tr.frame(walk(300), 1000, 1000) // x=350, still left
	require.Equal(t, map[int]string{1: "moving"}, d.Zones)

	d = tr.frame(walk(560), 1000, 1000) // x=610: crossed x=600 from A to B, left zone 1
	require.Equal(t, map[int]string{1: "leave"}, d.Zones)
	require.Equal(t, map[int]unifi.LineCross{5: {A2B: 1, A2BAdd: 1, Status: "enter"}}, d.Lines) // line 6 counts B2A only
	require.Equal(t, []int{5}, d.Boxes[0].Lines)

	d = tr.frame(walk(700), 1000, 1000) // x=750: no new crossing
	require.Empty(t, d.Lines)
	require.Empty(t, d.Zones)

	d = tr.frame(walk(400), 1000, 1000) // x=450: back across, B to A, into zone 1
	require.Equal(t, map[int]unifi.LineCross{
		5: {A2B: 1, B2A: 1, B2AAdd: 1, Status: "enter"},
		6: {B2A: 1, B2AAdd: 1, Status: "enter"},
	}, d.Lines)
	require.Equal(t, map[int]string{1: "enter"}, d.Zones)

	// a new event starts counting again; its end leaves zone 1
	last := tr.setObject("")
	require.Equal(t, map[int]string{1: "leave"}, last.Zones)
	require.Len(t, last.Boxes, 1)
	require.Empty(t, last.Boxes[0].Zones)
	require.Empty(t, tr.setObject("person").Boxes)
	d = tr.frame(walk(560), 1000, 1000)
	require.Empty(t, d.Lines) // first frame: side unknown
}

func TestTrackerScalesBoxes(t *testing.T) {
	tr := newTracker()
	d := tr.frame([]baichuan.AIBox{{X1: 448, Y1: 240, X2: 896, Y2: 480, Confidence: 150, TrackID: -1}}, 896, 480)
	require.Equal(t, unifi.Box{X: 500, Y: 500, W: 500, H: 500, Confidence: 100}, d.Boxes[0])
}

func TestWithinSegment(t *testing.T) {
	line := []float64{600, 0, 600, 500, 100, 250}
	require.True(t, withinSegment(line, 650, 250))
	require.False(t, withinSegment(line, 650, 800)) // beside the end of the line
}

func TestTrackerLoiter(t *testing.T) {
	tr := newTracker()
	clock := time.Unix(1000, 0)
	tr.now = func() time.Time { return clock }
	tr.setZones(nil, nil, map[string]unifi.Zone{
		"7": {Coord: []float64{0, 0, 500, 0, 500, 1000, 0, 1000},
			LoiterTimes: map[string]unifi.LoiterTime{"0": {MSec: 10000, ObjectType: "person"}}},
	})
	tr.setObject("person")

	d := tr.frame(walk(100), 1000, 1000)
	require.Empty(t, d.Loiter)
	clock = clock.Add(9 * time.Second)
	d = tr.frame(walk(120), 1000, 1000)
	require.Empty(t, d.Loiter) // not yet 10 s

	clock = clock.Add(2 * time.Second)
	d = tr.frame(walk(130), 1000, 1000)
	require.Equal(t, map[int]string{7: "enter"}, d.Loiter)
	require.Equal(t, []int{7}, d.Boxes[0].Loiter)

	clock = clock.Add(time.Second)
	d = tr.frame(walk(140), 1000, 1000)
	require.Equal(t, map[int]string{7: "moving"}, d.Loiter)

	d = tr.frame(walk(700), 1000, 1000) // walked out
	require.Equal(t, map[int]string{7: "leave"}, d.Loiter)

	// a vehicle loiter time does not apply to people
	tr.setZones(nil, nil, map[string]unifi.Zone{"8": {Coord: []float64{0, 0, 1000, 0, 1000, 1000, 0, 1000},
		LoiterTimes: map[string]unifi.LoiterTime{"0": {MSec: 0, ObjectType: "vehicle"}}}})
	d = tr.frame(walk(100), 1000, 1000)
	require.Empty(t, d.Loiter)
}

// Protect hears about one frame per 500 ms: transitions are repeated for
// latchFor, so one seen in a single frame is not lost.
func TestTrackerLatch(t *testing.T) {
	tr := newTracker()
	stepClock(tr, 100*time.Millisecond)
	tr.setZones(map[string]unifi.Zone{
		"1": {Coord: []float64{0, 0, 500, 0, 500, 1000, 0, 1000}},
	}, map[string]unifi.Line{
		"5": {Coord: []float64{600, 0, 600, 1000, 100, 500}, Direction: "none"},
	}, nil)
	tr.setObject("person")

	require.Equal(t, map[int]string{1: "enter"}, tr.frame(walk(100), 1000, 1000).Zones) // t=100ms
	for range 4 {                                                                       // up to t=500ms
		require.Equal(t, map[int]string{1: "enter"}, tr.frame(walk(150), 1000, 1000).Zones)
	}
	d := tr.frame(walk(150), 1000, 1000) // t=600ms: latched 500 ms ago, still
	require.Equal(t, map[int]string{1: "enter"}, d.Zones)
	d = tr.frame(walk(150), 1000, 1000) // t=700ms: 600 ms
	require.Equal(t, map[int]string{1: "moving"}, d.Zones)

	// out of the zone and across the line: both kept for the next frames
	d = tr.frame(walk(560), 1000, 1000)
	require.Equal(t, map[int]string{1: "leave"}, d.Zones)
	require.Equal(t, unifi.LineCross{A2B: 1, A2BAdd: 1, Status: "enter"}, d.Lines[5])
	d = tr.frame(walk(700), 1000, 1000)
	require.Equal(t, map[int]string{1: "leave"}, d.Zones)
	require.Equal(t, unifi.LineCross{A2B: 1, A2BAdd: 1, Status: "enter"}, d.Lines[5])

	// back across within the window: the counts add up
	d = tr.frame(walk(400), 1000, 1000)
	require.Equal(t, map[int]string{1: "enter"}, d.Zones) // supersedes the leave
	require.Equal(t, unifi.LineCross{A2B: 1, B2A: 1, A2BAdd: 1, B2AAdd: 1, Status: "enter"}, d.Lines[5])

	for range 6 {
		d = tr.frame(walk(400), 1000, 1000)
	}
	require.Equal(t, map[int]string{1: "moving"}, d.Zones)
	require.Empty(t, d.Lines)

	// the event ends with the object still inside: leave on the last boxes
	last := tr.setObject("")
	require.Equal(t, map[int]string{1: "leave"}, last.Zones)
	require.Equal(t, 400, last.Boxes[0].X)
}

package bridge

import (
	"testing"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
	"github.com/stretchr/testify/require"
)

// a person walking right across a 1000x1000 frame
func walk(x int) []baichuan.AIBox {
	return []baichuan.AIBox{{X1: x, Y1: 200, X2: x + 100, Y2: 700, Confidence: 80, TrackID: 3}}
}

func TestTrackerZonesAndLines(t *testing.T) {
	tr := newTracker()
	tr.setZones(map[string]unifi.Zone{
		"1": {Coord: []float64{0, 0, 500, 0, 500, 1000, 0, 1000}, ObjectTypes: []string{"person"}}, // left half
		"2": {Coord: []float64{0, 0, 1000, 0, 1000, 1000, 0, 1000}, ObjectTypes: []string{"vehicle"}},
	}, map[string]unifi.Line{
		// vertical line at x=600, A side = left (reference point at x<600)
		"5": {Coord: []float64{600, 0, 600, 1000, 100, 500}, Direction: "none", ObjectTypes: []string{"person"}},
		"6": {Coord: []float64{600, 0, 600, 1000, 100, 500}, Direction: "B2A"},
	})
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

	// a new event starts counting again
	tr.setObject("")
	tr.setObject("person")
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

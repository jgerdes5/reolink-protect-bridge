package unifi

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Events a camera reports to Protect 7.3. Protect's "ds" daemon (package ds
// 7.3.6, /usr/bin/ds) parses camera events with a strict typed schema and
// dispatches exactly these function names: EventAnalytics (motion),
// EventSmartDetect (person/vehicle/animal), EventSmartMotion, MCUEventMessage
// (the doorbell button, see Ring) and GetRequest. Payload shapes follow what
// woutersteegers/reolink-unifi-protect validated against ds; a frame ds can't
// parse is passed to Protect, which logs "not expected over avclient" and drops
// it. Event envelopes need timeStamp as an RFC 3339 string.

// ringCooldown suppresses repeated rings from one press (ds has its own cooldown too).
const ringCooldown = 10 * time.Second

// smartBox is the detection box sent when the source has no coordinates
// (Reolink push events): centred, in Protect's 0-1000 [x, y, w, h] space.
var smartBox = []int{250, 250, 500, 500}

const (
	smartZoneLevel   = 80  // ds requires a level next to every zone status
	smartDisplayMs   = 600 // displayTimeoutMSec, required by ds
	snapshotKeep     = 10 * time.Minute
	eventCaptureWait = 5 * time.Second
)

// event is an open motion or smart-detect event.
type event struct {
	id     uint32
	start  time.Time
	object string // "" for plain motion

	done chan struct{} // closed when shot is set (or the capture failed)
	shot *eventShot
	box  []int // [x, y, w, h] 0-1000: the object, for the crop; smartBox without real boxes
}

// eventShot is an event's snapshot, published by closing event.done.
type eventShot struct {
	snap  []byte
	snapW int
	snapH int
	crop  []byte
	side  int
	box   []int // the box crop was cut from
}

type events struct {
	mu       sync.Mutex
	nextID   uint32 // ds needs eventId (and trackerID = (id+1)*100) to fit 32 bits
	seeded   bool
	motion   *event
	smart    *event
	files    map[string]eventFile // announced snapshots Protect fetches by name
	lastSnap []byte               // newest event snapshot, for motionSnapshot requests
	lastRing time.Time
	det      Detection // latest real boxes
	detAt    time.Time
	movingAt time.Time // last "moving" frame

	queueOnce sync.Once
	queue     chan func() // event frames, sent in order by one goroutine
}

// eventQueue is the length of the frame queue; frames beyond it are dropped.
const eventQueue = 64

// enqueue sends event frames in order. A closing frame waits for its snapshot,
// so a start that follows quickly stays behind it, and nothing here blocks the
// caller (the camera's event loop). Must be called with c.ev.mu held.
func (c *Camera) enqueue(job func()) {
	c.ev.queueOnce.Do(func() {
		c.ev.queue = make(chan func(), eventQueue)
		go c.worker(c.ev.queue)
	})
	select {
	case c.ev.queue <- job:
	default:
		c.Log.Warn().Msgf("[unifi] %s: event queue full, frame dropped", c.Name)
	}
}

// resetEvents forgets open events without closing frames, when the Protect
// session ended: Protect drops a camera's open events on disconnect, and a
// late stop for an id it never saw would only confuse it.
func (c *Camera) resetEvents() {
	c.ev.mu.Lock()
	c.ev.motion, c.ev.smart = nil, nil
	c.ev.mu.Unlock()
}

type eventFile struct {
	data []byte
	at   time.Time
}

// Motion reports plain motion start (true) or end (false) as EventAnalytics.
func (c *Camera) Motion(on bool) {
	c.ev.mu.Lock()
	defer c.ev.mu.Unlock()

	switch {
	case on && c.ev.motion == nil:
		e := c.newEvent("", nil)
		c.ev.motion = e
		c.Log.Debug().Msgf("[unifi] %s: motion start (event %d)", c.Name, e.id)
		p := c.motionPayload(e, "start")
		c.enqueue(func() { c.sendEvent("EventAnalytics", p) })
	case !on && c.ev.motion != nil:
		e := c.ev.motion
		c.ev.motion = nil
		c.Log.Debug().Msgf("[unifi] %s: motion end (event %d)", c.Name, e.id)
		c.enqueue(func() { c.finishEvent(e, "EventAnalytics") })
	}
}

// SmartDetect reports an object ("person", "vehicle", "animal") entering, or
// with "" the end of the current detection, as EventSmartDetect. A change of
// object type during an event is kept as the first type, as cameras do.
func (c *Camera) SmartDetect(object string) {
	c.ev.mu.Lock()
	defer c.ev.mu.Unlock()

	switch {
	case object != "" && c.ev.smart == nil:
		det, fresh := c.freshDetection()
		box := smartBox
		if fresh {
			box = det.Boxes[largest(det.Boxes)].coord()
		}
		e := c.newEvent(object, box)
		c.ev.smart = e
		c.Log.Info().Msgf("[unifi] %s: %s detected (event %d)", c.Name, object, e.id)
		p := c.motionPayload(e, "enter")
		p["objectTypes"] = []string{object}
		p["zonesStatus"] = zones("enter")
		if fresh && len(det.Zones) > 0 {
			p["zonesStatus"] = zoneStatuses(det.Zones)
		}
		p["smartDetectSnapshot"] = ""
		p["smartDetectSnapshotFullFoV"] = ""
		p["smartDetectSnapshotFullFoVHeight"] = 0
		p["smartDetectSnapshotFullFoVWidth"] = 0
		p["smartDetectSnapshots"] = []any{}
		p["displayTimeoutMSec"] = smartDisplayMs
		p["descriptors"] = []any{descriptor(e)}
		if fresh {
			p["descriptors"] = descriptors(e, det.Boxes)
		}
		c.enqueue(func() { c.sendEvent("EventSmartDetect", p) })
	case object == "" && c.ev.smart != nil:
		e := c.ev.smart
		c.ev.smart = nil
		c.Log.Info().Msgf("[unifi] %s: %s gone (event %d)", c.Name, e.object, e.id)
		c.enqueue(func() { c.finishEvent(e, "EventSmartDetect") })
	}
}

// ClearEvents ends all open events, e.g. when the event source disconnects.
func (c *Camera) ClearEvents() {
	c.SmartDetect("")
	c.Motion(false)
}

// Ring reports a press of the doorbell button. A real G4 Doorbell's
// ubnt_ctlserver (RingButtonReactor) broadcasts MCUEventMessage with eventType
// "EventRingButtonPressed", ubnt_avclient forwards it, and ds's RingHandler
// turns it into a ring event: notification, call screen, lastRing. With
// RingAPI set, Protect's HTTP route is used instead (as unifi-cam-proxy does).
// Ring returns at once; the ring is sent in the background.
func (c *Camera) Ring(ctx context.Context) {
	c.ev.mu.Lock()
	if time.Since(c.ev.lastRing) < ringCooldown {
		c.ev.mu.Unlock()
		return
	}
	c.ev.lastRing = time.Now()
	c.ev.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ringTimeout)
		defer cancel()
		if err := c.ring(ctx); err != nil {
			c.Log.Warn().Err(err).Msgf("[unifi] %s: ring", c.Name)
			c.ev.mu.Lock()
			c.ev.lastRing = time.Time{} // a failed ring must not block the next press
			c.ev.mu.Unlock()
		}
	}()
}

const ringTimeout = 30 * time.Second

func (c *Camera) ring(ctx context.Context) error {
	if c.RingAPI != nil {
		if err := c.RingAPI(ctx); err != nil {
			return err
		}
		c.Log.Info().Msgf("[unifi] %s: ring (Protect API)", c.Name)
		return nil
	}
	if err := c.sendMessage(nil, "MCUEventMessage", 0,
		map[string]any{"eventType": "EventRingButtonPressed"}, rfc3339ms(time.Now())); err != nil {
		return err
	}
	c.Log.Info().Msgf("[unifi] %s: ring", c.Name)
	return nil
}

// newEvent opens an event and captures its snapshot in the background. Must be
// called with c.ev.mu held.
func (c *Camera) newEvent(object string, box []int) *event {
	if !c.ev.seeded {
		// not 0 after every restart, which could collide with ids Protect still
		// holds; below 2*10^7 so trackerID ((id+1)*100) stays inside int32
		c.ev.seeded = true
		c.ev.nextID = uint32(time.Now().Unix()%1_000_000) * 10
	}
	if c.ev.nextID >= 20_000_000 {
		c.ev.nextID = 0
	}
	e := &event{id: c.ev.nextID, start: time.Now(), object: object, done: make(chan struct{}), box: box}
	c.ev.nextID++
	go c.captureEvent(e, slices.Clone(box)) // Detections moves e.box meanwhile
	return e
}

func (c *Camera) captureEvent(e *event, box []int) {
	defer close(e.done)
	if c.Snapshot == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventCaptureWait)
	defer cancel()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		c.Log.Debug().Err(err).Msgf("[unifi] %s: event snapshot", c.Name)
		return
	}
	shot := &eventShot{snap: snap}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(snap)); err == nil {
		shot.snapW, shot.snapH = cfg.Width, cfg.Height
	}
	if e.object != "" {
		shot.crop, shot.side = squareCrop(snap, box)
		shot.box = box
	}
	e.shot = shot // published by close(e.done)

	c.ev.mu.Lock()
	c.ev.lastSnap = snap
	c.ev.mu.Unlock()
}

// finishEvent sends the closing frame once the event's snapshot is ready (or
// given up on), announcing the files Protect then fetches with GetRequest.
func (c *Camera) finishEvent(e *event, fn string) {
	var shot eventShot // empty when the capture is late or failed
	select {
	case <-e.done:
		if e.shot != nil {
			shot = *e.shot
		}
	case <-time.After(eventCaptureWait + time.Second):
	}

	if e.object == "" {
		p := c.motionPayload(e, "stop")
		p["clockBestWall"] = e.start.UnixMilli()
		p["levels"] = map[string]int{"0": 49}
		p["motionHeatmap"] = ""
		p["motionSnapshot"] = "motionsnap.jpg"
		c.sendEvent(fn, p)
		return
	}

	p := c.motionPayload(e, "leave")
	p["clockBestWall"] = e.start.UnixMilli()
	p["levels"] = map[string]int{"0": 49}
	p["motionHeatmap"] = "" // no heatmap: Protect would fetch a JPEG as its PNG
	p["motionSnapshot"] = "motionsnap.jpg"
	p["objectTypes"] = []string{e.object}
	p["zonesStatus"] = zones("leave")
	p["smartDetectSnapshot"] = "motionsnap.jpg"
	p["displayTimeoutMSec"] = smartDisplayMs
	p["descriptors"] = []any{}

	// The full frame and a square crop of the object: ds stores them as the
	// event's thumbnails, and Protect's Person/Vehicle/Animal filters only list
	// events with a crop (detectedThumbnails).
	if shot.snap != nil {
		name := fmt.Sprintf("fullfov_%d.jpg", e.id)
		c.addFile(name, shot.snap)
		p["smartDetectSnapshotFullFoV"] = name
		p["smartDetectSnapshotFullFoVWidth"] = shot.snapW
		p["smartDetectSnapshotFullFoVHeight"] = shot.snapH
	}
	if shot.crop != nil {
		name := fmt.Sprintf("detect_%d.jpg", trackerID(e))
		c.addFile(name, shot.crop)
		p["smartDetectSnapshots"] = []any{map[string]any{
			"trackerID":                 trackerID(e),
			"clockBestMonotonic":        c.uptime().Milliseconds(),
			"clockBestWall":             e.start.UnixMilli(),
			"confidenceLevel":           90,
			"coord":                     shot.box,
			"framingRect":               shot.box,
			"reVerifyEligible":          false,
			"smartDetectSnapshot":       name,
			"smartDetectSnapshotHeight": shot.side,
			"smartDetectSnapshotType":   e.object,
			"smartDetectSnapshotName":   name,
			"smartDetectSnapshotWidth":  shot.side,
		}}
		p["trackerIDAttrMap"] = map[string]any{
			strconv.Itoa(trackerID(e)): map[string]any{"objectType": e.object, "zone": []int{1}},
		}
	}
	c.sendEvent(fn, p)
}

func (c *Camera) motionPayload(e *event, edge string) map[string]any {
	now := time.Now()
	up := c.uptime().Milliseconds()
	return map[string]any{
		"clockBestMonotonic": up,
		"clockBestWall":      now.UnixMilli(),
		"clockMonotonic":     up,
		"clockStream":        up,
		"clockStreamRate":    1000,
		"clockWall":          now.UnixMilli(),
		"edgeType":           edge,
		"eventId":            e.id,
		"eventType":          "motion",
		"levels":             map[string]int{"0": 47},
		"motionHeatmap":      "",
		"motionSnapshot":     "",
	}
}

func (c *Camera) sendEvent(fn string, payload map[string]any) {
	if err := c.sendMessage(nil, fn, 0, payload, rfc3339ms(time.Now())); err != nil {
		c.Log.Debug().Err(err).Msgf("[unifi] %s: %s", c.Name, fn)
	}
}

func zones(status string) map[string]any {
	return map[string]any{"1": map[string]any{"level": smartZoneLevel, "status": status}}
}

// trackerID must stay the same for one object across an event's frames.
func trackerID(e *event) int {
	return int(e.id+1) * 100
}

// descriptor is a detection box as a real G5's smart-detect frames carry it.
func descriptor(e *event) map[string]any {
	return map[string]any{
		"trackerID":         trackerID(e),
		"name":              "",
		"confidenceLevel":   90,
		"coord":             e.box,
		"objectType":        e.object,
		"zones":             []int{1},
		"lines":             []any{},
		"loiterZones":       []any{},
		"intelligenceZones": []any{},
		"secondLensZones":   []any{},
		"stationary":        false,
		"attributes":        nil,
		"coord3d":           []int{-1, -1},
		"boxColor":          boxColors[e.object],
		"firstShownTimeMs":  e.start.UnixMilli(),
		"idleSinceTimeMs":   0,
		"tag":               "",
	}
}

var boxColors = map[string]string{"person": "red", "vehicle": "cyan", "animal": "white"}

func (c *Camera) addFile(name string, data []byte) {
	c.ev.mu.Lock()
	defer c.ev.mu.Unlock()
	if c.ev.files == nil {
		c.ev.files = map[string]eventFile{}
	}
	now := time.Now()
	for k, f := range c.ev.files {
		if now.Sub(f.at) > snapshotKeep {
			delete(c.ev.files, k)
		}
	}
	c.ev.files[name] = eventFile{data: data, at: now}
}

// eventFile returns an announced file, or for motion snapshots the newest
// event snapshot.
func (c *Camera) eventFile(what, name string) []byte {
	c.ev.mu.Lock()
	defer c.ev.mu.Unlock()
	if f, ok := c.ev.files[name]; ok && name != "" {
		return f.data
	}
	if what == "motionSnapshot" || what == "smartDetectZoneSnapshot" {
		return c.ev.lastSnap
	}
	return nil
}

// squareCrop cuts a square around box (0-1000 [x, y, w, h]) out of a JPEG,
// padded for context like unifi-cam-proxy's crops; Protect's isRealCropped
// needs it square.
func squareCrop(src []byte, box []int) ([]byte, int) {
	img, err := jpeg.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, 0
	}
	b := img.Bounds()
	fw, fh := float64(b.Dx()), float64(b.Dy())
	x, y, w, h := float64(box[0]), float64(box[1]), float64(box[2]), float64(box[3])
	boxW, boxH := w*fw/1000, h*fh/1000
	cx, cy := (x+w/2)*fw/1000, (y+h/2)*fh/1000
	side := min(fw, fh, max(320, min(fh, max(boxW, boxH)*2.2)))
	left := min(max(cx-side/2, 0), fw-side)
	top := min(max(cy-side/2, 0), fh-side)

	sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	})
	if !ok {
		return nil, 0
	}
	r := image.Rect(int(left), int(top), int(left+side), int(top+side)).Add(b.Min)
	var out bytes.Buffer
	if err = jpeg.Encode(&out, sub.SubImage(r), &jpeg.Options{Quality: 85}); err != nil {
		return nil, 0
	}
	return out.Bytes(), int(side)
}

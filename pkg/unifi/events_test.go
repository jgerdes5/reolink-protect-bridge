package unifi

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func testJPEG(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x += 7 {
		img.Set(x, h/2, color.White)
	}
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, img, nil))
	return b.Bytes()
}

// connectedCamera runs a camera against the fake Protect and returns the
// Protect side of the socket after the hello.
func connectedCamera(t *testing.T, cam *Camera) (*fakeProtect, *websocket.Conn) {
	p := newFakeProtect(t)
	host, port := p.hostPort()
	pemText, err := GenerateCert()
	require.NoError(t, err)
	cam.Cert, err = LoadCert(pemText)
	require.NoError(t, err)
	cam.Host, cam.Port = host, port
	cam.Name, cam.MAC, cam.IP = "Klingel", "02A1B2C3D4E5", "127.0.0.1"
	cam.Doorbell, cam.Identity = true, Identities["g5pro"]
	cam.Token = func(context.Context, bool) (string, error) { return "good", nil }
	cam.Channel = func(string) Channel { return Channel{Width: 640, Height: 480, FPS: 15} }
	cam.Start = func(string, string, string) {}
	cam.Stop = func(string) {}
	cam.Log = zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.DebugLevel)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cam.Run(ctx)

	conn := p.accept()
	msg, _ := readMsg(t, conn)
	require.Equal(t, "ubnt_avclient_hello", msg.FunctionName)
	return p, conn
}

func TestRingCameraProtocol(t *testing.T) {
	cam := &Camera{}
	_, conn := connectedCamera(t, cam)

	cam.Ring(context.Background())
	msg, payload := readMsg(t, conn)
	// what ds 7.3.6 matches: functionName MCUEventMessage, payload.eventType
	require.Equal(t, "MCUEventMessage", msg.FunctionName)
	require.Equal(t, map[string]any{"eventType": "EventRingButtonPressed"}, payload)
	_, err := time.Parse(time.RFC3339, msg.TimeStamp) // ds wants RFC 3339
	require.NoError(t, err)

	// a second press within the cooldown is not sent
	cam.Ring(context.Background())
	_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	_, _, err = conn.ReadMessage()
	require.Error(t, err)
}

func TestRingAPI(t *testing.T) {
	var calls atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	cam := &Camera{Log: zerolog.Nop(), RingAPI: func(context.Context) error {
		calls.Add(1)
		if fail.Load() {
			return errors.New("protect down")
		}
		return nil
	}}

	// a failed ring does not use up the cooldown
	cam.Ring(context.Background())
	require.Eventually(t, func() bool { return calls.Load() == 1 }, time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		cam.ev.mu.Lock()
		defer cam.ev.mu.Unlock()
		return cam.ev.lastRing.IsZero()
	}, time.Second, time.Millisecond)

	fail.Store(false)
	cam.Ring(context.Background())
	cam.Ring(context.Background()) // within the cooldown
	require.Eventually(t, func() bool { return calls.Load() == 2 }, time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.Equal(t, int32(2), calls.Load())
}

func TestSmartDetectEvent(t *testing.T) {
	snap := testJPEG(t, 2560, 1920)
	cam := &Camera{Snapshot: func(context.Context) ([]byte, error) { return snap, nil }}
	p, conn := connectedCamera(t, cam)

	cam.SmartDetect("person")
	msg, enter := readMsg(t, conn)
	require.Equal(t, "EventSmartDetect", msg.FunctionName)
	require.NotEmpty(t, msg.TimeStamp)
	require.Equal(t, "enter", enter["edgeType"])
	require.Equal(t, "motion", enter["eventType"])
	require.Equal(t, []any{"person"}, enter["objectTypes"])
	require.Equal(t, float64(600), enter["displayTimeoutMSec"])
	zone := enter["zonesStatus"].(map[string]any)["1"].(map[string]any)
	require.Equal(t, map[string]any{"level": float64(80), "status": "enter"}, zone)
	desc := enter["descriptors"].([]any)[0].(map[string]any)
	require.Equal(t, "person", desc["objectType"])
	require.Equal(t, []any{float64(250), float64(250), float64(500), float64(500)}, desc["coord"])
	id := enter["eventId"].(float64)

	// a second class during the event does not start another one
	cam.SmartDetect("vehicle")

	cam.SmartDetect("")
	msg, leave := readMsg(t, conn)
	require.Equal(t, "EventSmartDetect", msg.FunctionName)
	require.Equal(t, "leave", leave["edgeType"])
	require.Equal(t, id, leave["eventId"])
	tracker := strconv.Itoa((int(id) + 1) * 100)
	require.Equal(t, "fullfov_"+strconv.Itoa(int(id))+".jpg", leave["smartDetectSnapshotFullFoV"])
	require.Equal(t, float64(2560), leave["smartDetectSnapshotFullFoVWidth"])
	shots := leave["smartDetectSnapshots"].([]any)
	require.Len(t, shots, 1)
	shot := shots[0].(map[string]any)
	require.Equal(t, "detect_"+tracker+".jpg", shot["smartDetectSnapshot"])
	require.Equal(t, "person", shot["smartDetectSnapshotType"])
	require.Equal(t, float64(1920), shot["smartDetectSnapshotWidth"]) // square, full height
	require.Contains(t, leave["trackerIDAttrMap"], tracker)
	require.Less(t, (id+1)*100, float64(1<<32)) // ds: trackerID is a u32

	// Protect then fetches the crop by name
	sendMsg(t, conn, 7, "GetRequest", true, map[string]any{
		"what": "smartDetectSnapshot", "filename": "detect_" + tracker + ".jpg", "uri": p.srv.URL + "/upload",
	})
	body := <-p.bodies
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, [2]int{1920, 1920}, [2]int{cfg.Width, cfg.Height})
	msg, _ = readMsg(t, conn)
	require.Equal(t, "GetRequest", msg.FunctionName)
	require.Equal(t, 7, msg.InResponseTo)

	// and the full frame / motion snapshot
	sendMsg(t, conn, 8, "GetRequest", true, map[string]any{"what": "motionSnapshot", "uri": p.srv.URL + "/upload"})
	require.Equal(t, snap, <-p.bodies)
}

func TestMotionEvent(t *testing.T) {
	cam := &Camera{Snapshot: func(context.Context) ([]byte, error) { return testJPEG(t, 64, 48), nil }}
	_, conn := connectedCamera(t, cam)

	cam.Motion(true)
	cam.Motion(true) // no second start
	msg, start := readMsg(t, conn)
	require.Equal(t, "EventAnalytics", msg.FunctionName)
	require.Equal(t, "start", start["edgeType"])

	cam.ClearEvents()
	msg, stop := readMsg(t, conn)
	require.Equal(t, "EventAnalytics", msg.FunctionName)
	require.Equal(t, "stop", stop["edgeType"])
	require.Equal(t, start["eventId"], stop["eventId"])
	require.Equal(t, "motionsnap.jpg", stop["motionSnapshot"])
}

func TestTalkback(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	cam := &Camera{Talk: func(b []byte) { mu.Lock(); got = append(got, b); mu.Unlock() }}
	_, conn := connectedCamera(t, cam)

	// a free port instead of 7004
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.LocalAddr().(*net.UDPAddr).Port
	require.NoError(t, l.Close())

	sendMsg(t, conn, 3, "ChangeTalkbackSettings", true, map[string]any{
		"typeFmt": "aac", "typeIn": "serverudp", "bindAddr": "0.0.0.0", "bindPort": port,
		"filterAddr": nil, "filterPort": nil, "channels": 1, "samplingRate": 22050,
		"bitsPerSample": 16, "quality": 100,
	})
	msg, reply := readMsg(t, conn)
	require.Equal(t, "ChangeTalkbackSettings", msg.FunctionName)
	require.Equal(t, 3, msg.InResponseTo)
	// every key of Protect's TalkbackSettingsSchema
	for _, k := range []string{"typeFmt", "typeIn", "bindAddr", "bindPort", "filterAddr", "filterPort",
		"channels", "samplingRate", "bitsPerSample", "quality", "url"} {
		require.Contains(t, reply, k)
	}
	require.Equal(t, float64(port), reply["bindPort"])

	out, err := net.Dial("udp", "127.0.0.1:"+strconv.Itoa(port))
	require.NoError(t, err)
	defer out.Close()
	adts := []byte{0xFF, 0xF1, 0x60, 0x40, 0x10, 0x9F, 0xFC, 0x21, 0x00}
	_, _ = out.Write([]byte("not adts, dropped"))
	_, _ = out.Write(adts)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	}, 2*time.Second, 10*time.Millisecond)
	mu.Lock()
	require.Equal(t, adts, got[0])
	mu.Unlock()
}

func TestFeatureFlags(t *testing.T) {
	cam := &Camera{Doorbell: true}
	f := cam.featureFlags()
	require.NotContains(t, f, "speaker") // no talkback configured
	require.Equal(t, true, f["doorbell"])
	require.Equal(t, true, f["supportCustomRingtone"]) // doorbell tone section; Protect would fall back to isDoorbell
	require.Equal(t, true, f["ledStatus"])             // without it Protect rejects LED changes
	require.Equal(t, []string{"person", "vehicle", "animal"}, f["smartDetect"])

	cam.Talk = func([]byte) {}
	require.Equal(t, true, cam.featureFlags()["speaker"])

	for k := range cam.featureFlags() {
		// Protect only reads short keys: has*/can* would be dead weight
		require.False(t, strings.HasPrefix(k, "has") || strings.HasPrefix(k, "can"), k)
		// the Reolink doorbell has no display: no LCD capability is ever claimed
		require.NotContains(t, strings.ToLower(k), "lcd")
	}
}

// A start right after a stop stays behind the stop, which waits for its snapshot.
func TestEventOrder(t *testing.T) {
	release := make(chan struct{})
	cam := &Camera{Snapshot: func(ctx context.Context) ([]byte, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return testJPEG(t, 64, 48), nil
	}}
	_, conn := connectedCamera(t, cam)

	cam.Motion(true)
	cam.Motion(false)
	cam.Motion(true) // flapping
	time.AfterFunc(200*time.Millisecond, func() { close(release) })

	var edges []string
	for i := 0; i < 3; i++ {
		_, p := readMsg(t, conn)
		edges = append(edges, p["edgeType"].(string)+strconv.Itoa(int(p["eventId"].(float64))))
	}
	require.Len(t, edges, 3)
	id := edges[0][len("start"):]
	next, _ := strconv.Atoi(id)
	require.Equal(t, []string{"start" + id, "stop" + id, "start" + strconv.Itoa(next+1)}, edges)
}

func TestSmartDetectRealBoxes(t *testing.T) {
	cam := &Camera{Snapshot: func(context.Context) ([]byte, error) { return testJPEG(t, 1000, 1000), nil }}
	_, conn := connectedCamera(t, cam)

	person := Box{X: 600, Y: 200, W: 100, H: 400, Confidence: 77, TrackerID: 9, Zones: []int{3}}
	cam.Detections(Detection{Boxes: []Box{person}, Zones: map[int]string{3: "enter"}})
	cam.SmartDetect("person")

	_, enter := readMsg(t, conn)
	require.Equal(t, "enter", enter["edgeType"])
	desc := enter["descriptors"].([]any)[0].(map[string]any)
	require.Equal(t, []any{600.0, 200.0, 100.0, 400.0}, desc["coord"])
	require.Equal(t, 9.0, desc["trackerID"])
	require.Equal(t, 77.0, desc["confidenceLevel"])
	require.Equal(t, []any{3.0}, desc["zones"])
	require.Contains(t, enter["zonesStatus"], "3")

	// a later frame: the box moved and crossed line 5
	person.X, person.Lines = 650, []int{5}
	cam.Detections(Detection{
		Boxes: []Box{person},
		Zones: map[int]string{3: "moving"},
		Lines: map[int]LineCross{5: {A2B: 1, A2BAdd: 1, Status: "enter"}},
	})
	msg, moving := readMsg(t, conn)
	require.Equal(t, "EventSmartDetect", msg.FunctionName)
	require.Equal(t, "moving", moving["edgeType"])
	require.Equal(t, enter["eventId"], moving["eventId"])
	line := moving["linesStatus"].(map[string]any)["5"].(map[string]any)
	require.Equal(t, "enter", line["status"])
	require.Equal(t, 1.0, line["person"].(map[string]any)["crosslineA2B"])
	require.Equal(t, []any{5.0}, moving["descriptors"].([]any)[0].(map[string]any)["lines"])

	// the crop follows the real box
	cam.SmartDetect("")
	_, leave := readMsg(t, conn)
	shot := leave["smartDetectSnapshots"].([]any)[0].(map[string]any)
	require.Equal(t, []any{650.0, 200.0, 100.0, 400.0}, shot["coord"])
}

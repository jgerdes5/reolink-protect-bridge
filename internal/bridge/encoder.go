package bridge

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jgerdes5/reolink-protect-bridge/pkg/baichuan"
	"github.com/jgerdes5/reolink-protect-bridge/pkg/unifi"
)

// The doorbell's video encoders: frame rate, bitrate and keyframe interval of
// the main and sub stream (Compression, msg 56/57), and the values it offers
// (StreamInfoList, msg 146: an encodeTable per stream and resolution with
// comma-separated framerateTable and bitrateTable in kbit/s).

const (
	msgEncGet    = 56
	msgEncSet    = 57
	msgEncCapGet = 146
)

var encStreams = [2]string{"mainStream", "subStream"}

type encStream struct {
	fps, kbps int   // current
	fpsTable  []int // offered, descending as the doorbell lists them
	kbpsTable []int // offered, ascending
}

// loadEncoder reads the encoders and their choices and sets the keyframe
// interval to one second, as a UniFi camera streams. It runs before Protect
// connects, so the first ChangeVideoSettings reply carries the real values.
func (b *Bridge) loadEncoder(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := b.dialControl(ctx)
	if err != nil {
		log.Debug().Err(err).Msg("[settings] encoder: can't reach the doorbell")
		return
	}
	defer client.Close()
	ch := uint8(b.opts.Doorbell.Channel)

	caps, err := client.GetConfig(ctx, msgEncCapGet, ch, nil)
	if err != nil {
		log.Debug().Err(err).Msg("[settings] encoder choices")
	}
	body, err := client.GetConfig(ctx, msgEncGet, ch, nil)
	if err != nil {
		log.Warn().Err(err).Msg("[settings] encoder")
		return
	}
	enc := encoderState(body, caps)
	// Written even when Options.Settings is off: Protect cuts its segments at
	// keyframes and expects one every second, as a UniFi camera sends.
	if gopChange(body) {
		if err = client.SetConfig(ctx, msgEncSet, ch, body); err != nil {
			log.Warn().Err(err).Msg("[settings] keyframe interval")
		} else {
			log.Info().Msg("[settings] keyframe interval applied to the doorbell")
		}
	}
	b.setEncoder(enc)

	// The doorbell's HDR, so Protect sees the real state after a restart
	// instead of switching it again.
	if img, err := client.GetConfig(ctx, baichuan.MsgImageGet, ch, nil); err == nil {
		if v := img.Value("InputAdvanceCfg/hdrSwitch"); v != "" && b.proto != nil {
			b.proto.SetHDR(v != "0")
		}
	}
	for i, s := range enc {
		log.Info().Msgf("[settings] %s: %d fps %d kbit/s, keyframe interval %s, offers %v fps and %v kbit/s",
			encStreams[i], s.fps, s.kbps, gopText(body, encStreams[i]), s.fpsTable, s.kbpsTable)
	}
}

// setEncoder stores the encoder state; the frame rate the encoder is set to
// replaces the measured one.
func (b *Bridge) setEncoder(enc [2]encStream) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.enc = enc
	for i, s := range enc {
		if s.fps > 0 {
			b.info[i].fps = s.fps
		}
	}
}

// encoderState reads the current values from Compression and the choices for
// each stream's current resolution from StreamInfoList (caps may be nil).
func encoderState(body, caps *baichuan.Node) [2]encStream {
	var enc [2]encStream
	for i, name := range encStreams {
		s := &enc[i]
		s.fps, _ = strconv.Atoi(body.Value("Compression/" + name + "/frame"))
		s.kbps, _ = strconv.Atoi(body.Value("Compression/" + name + "/bitRate"))
		if caps == nil {
			continue
		}
		w := body.Value("Compression/" + name + "/width")
		h := body.Value("Compression/" + name + "/height")
		var first, match *baichuan.Node
		walkNodes(caps, func(n *baichuan.Node) {
			if !strings.EqualFold(n.Name, "encodeTable") || !strings.EqualFold(n.Value("type"), name) {
				return
			}
			if first == nil {
				first = n
			}
			if match == nil && n.Value("resolution/width") == w && n.Value("resolution/height") == h {
				match = n
			}
		})
		if match == nil {
			match = first
		}
		if match != nil {
			s.fpsTable = intList(match.Value("framerateTable"))
			s.kbpsTable = intList(match.Value("bitrateTable"))
			sort.Ints(s.kbpsTable)
		}
	}
	return enc
}

func walkNodes(n *baichuan.Node, fn func(*baichuan.Node)) {
	fn(n)
	for _, c := range n.Children {
		walkNodes(c, fn)
	}
}

func intList(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if v, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// nearest is the offered value closest to v (the higher one on a tie).
func nearest(table []int, v int) int {
	best := 0
	for _, t := range table {
		if best == 0 || abs(t-v) < abs(best-v) || (abs(t-v) == abs(best-v) && t > best) {
			best = t
		}
	}
	return best
}

// atMost is the highest offered value not above v, else the lowest.
func atMost(table []int, v int) int {
	if len(table) == 0 {
		return 0
	}
	best := slices.Min(table)
	for _, t := range table {
		if t <= v && t > best {
			best = t
		}
	}
	return best
}

// encoderChannel fills a Protect channel's encoder choices.
func (b *Bridge) encoderChannel(ch *unifi.Channel, i int) {
	s := b.enc[min(i, 1)]
	ch.FPSValues = ascending(s.fpsTable)
	if s.kbps > 0 {
		ch.Bitrate = s.kbps * 1000
	}
	if len(s.kbpsTable) > 0 {
		ch.BitrateMin, ch.BitrateMax = s.kbpsTable[0]*1000, s.kbpsTable[len(s.kbpsTable)-1]*1000
	}
}

func ascending(v []int) []int {
	if len(v) == 0 {
		return nil
	}
	out := slices.Clone(v)
	sort.Ints(out)
	return slices.Compact(out)
}

// videoChange applies Protect's frame rate and bitrate to Compression: video1
// sets the main stream, video2 (else video3) the sub stream; values are
// rounded to what the doorbell offers, and without its list of choices
// nothing is changed. The keyframe interval follows the frame rate.
func videoChange(req map[string]unifi.Video, enc [2]encStream) func(*baichuan.Node) bool {
	return func(body *baichuan.Node) bool {
		changed := false
		for i, name := range encStreams {
			v, ok := req["video1"]
			if i == 1 {
				if v, ok = req["video2"]; !ok {
					v, ok = req["video3"]
				}
			}
			if !ok {
				continue
			}
			if f := nearest(enc[i].fpsTable, v.FPS); v.FPS > 0 && f > 0 {
				changed = body.Set("Compression/"+name+"/frame", strconv.Itoa(f)) || changed
			}
			if k := atMost(enc[i].kbpsTable, v.Bitrate/1000); v.Bitrate > 0 && k > 0 {
				changed = body.Set("Compression/"+name+"/bitRate", strconv.Itoa(k)) || changed
			}
		}
		return gopChange(body) || changed
	}
}

// applyVideo writes pending frame rate and bitrate changes.
func (b *Bridge) applyVideo(ctx context.Context, c *baichuan.Client, ch uint8, req map[string]unifi.Video) error {
	b.mu.Lock()
	enc := b.enc
	b.mu.Unlock()
	body, err := c.GetConfig(ctx, msgEncGet, ch, nil)
	if err != nil {
		return err
	}
	if videoChange(req, enc)(body) {
		if err = c.SetConfig(ctx, msgEncSet, ch, body); err != nil {
			return err
		}
		log.Info().Msg("[settings] frame rate and bitrate applied to the doorbell")
	}
	// keep the choices, take the doorbell's current values
	now := encoderState(body, nil)
	for i := range enc {
		enc[i].fps, enc[i].kbps = now[i].fps, now[i].kbps
	}
	b.setEncoder(enc)
	return nil
}

// gopText is a stream's keyframe interval in its unit (see gopChange).
func gopText(b *baichuan.Node, stream string) string {
	cur := b.Value("Compression/" + stream + "/gop/cur")
	if hi, err := strconv.Atoi(b.Value("Compression/" + stream + "/gop/max")); err == nil && hi > 0 && hi <= gopSecondsMax {
		return cur + " s"
	}
	return cur + " frames"
}

package baichuan

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

// Configuration commands of the Reolink doorbell firmware (D340W v3.0.0.4662),
// from its netserver command table and libnetpublic XML builders, and
// reolink_aio. Every setting is changed with GET -> modify -> SET of the whole
// tree, as the firmware's SET handlers expect.
const (
	MsgImageGet    = 26  // VideoInput (brightness ...) + InputAdvanceCfg (ISP)
	MsgImageSet    = 25  //
	MsgMotionGet   = 46  // MD: enable, sensitivity, area
	MsgMotionSet   = 47  //
	MsgLedGet      = 208 // LedState: IR LEDs, status LED, button light
	MsgLedSet      = 209 //
	MsgAudioGet    = 264 // audioCfg: volumes, doorbell ringtone
	MsgAudioSet    = 265 //
	MsgAiCfgGet    = 299 // AiCfg: AI types on/off
	MsgAiCfgSet    = 300 //
	MsgAiDetectGet = 342 // AiDetectCfg per type: sensitivity, delay, area
	MsgAiDetectSet = 343 //
	MsgDayNightGet = 296 // DayNightThreshold
	MsgDayNightSet = 297 //
)

// channelExtension addresses a per-channel command (reolink_aio
// CHANNEL_EXTENSION_XML).
func channelExtension(channel uint8) []byte {
	return []byte(fmt.Sprintf("%s<Extension version=\"1.1\">\n<channelId>%d</channelId>\n</Extension>\n", xmlHeader, channel))
}

// GetConfig sends a GET command and returns the response's <body> element.
// body is the request body for commands that need one (e.g. AiDetectCfg's
// type), else nil.
func (c *Client) GetConfig(ctx context.Context, msgID uint32, channel uint8, body []byte) (*Node, error) {
	if err := c.Login(ctx); err != nil {
		return nil, err
	}
	resp, err := c.sendRequest(ctx, request{
		MsgID:     msgID,
		ChannelID: channel + 1, // per-channel commands carry channel+1 (reolink_aio)
		Class:     classModernWithOffset,
		Extension: channelExtension(channel),
		Body:      body,
	})
	if err != nil {
		return nil, err
	}
	return ParseNode(resp.XML)
}

// SetConfig sends a SET command with a (modified) <body> from GetConfig.
func (c *Client) SetConfig(ctx context.Context, msgID uint32, channel uint8, body *Node) error {
	if err := c.Login(ctx); err != nil {
		return err
	}
	_, err := c.sendRequest(ctx, request{
		MsgID:     msgID,
		ChannelID: channel + 1,
		Class:     classModernWithOffset,
		Extension: channelExtension(channel),
		Body:      body.Marshal(),
	})
	return err
}

// AiDetectBody is the GET body for one AI type's AiDetectCfg (reolink_aio
// GetAiAlarm, verbatim).
func AiDetectBody(channel uint8, aiType string) []byte {
	return []byte(fmt.Sprintf("%s<body>\n<AiDetectCfg version=\"1.1\">\n<chn>%d</chn>\n<type>%s</type>\n</AiDetectCfg>\n</body>\n", xmlHeader, channel, aiType))
}

// Detection areas (motion scope/valueTable and AI area) are bitmaps of cols x
// rows cells, row-major from the top, one bit per cell (1 = detect), MSB first,
// base64 without line breaks (firmware get_area_from_xmlnode /
// EncodeBinaryArrayToBase64).

// EncodeArea packs cells[row*cols+col].
func EncodeArea(cells []bool, cols, rows int) string {
	buf := make([]byte, (cols*rows+7)/8)
	for i, on := range cells[:cols*rows] {
		if on {
			buf[i/8] |= 0x80 >> (i % 8)
		}
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// DecodeArea unpacks an area bitmap.
func DecodeArea(s string, cols, rows int) ([]bool, error) {
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		return nil, err
	}
	if len(b) < (cols*rows+7)/8 {
		return nil, fmt.Errorf("baichuan: area has %d bytes, %dx%d needs %d", len(b), cols, rows, (cols*rows+7)/8)
	}
	out := make([]bool, cols*rows)
	for i := range out {
		out[i] = b[i/8]&(0x80>>(i%8)) != 0
	}
	return out, nil
}

// MsgIframeRequest makes the encoder of one stream start a new keyframe
// (IFRAME_REQUEST; D340W netserver handler 0x54470, param RequestIframe).
// The doorbell answers 200 at once, before the keyframe is made.
const MsgIframeRequest = 189

// RequestIframe asks for a keyframe on stream ("mainStream", "subStream").
func (c *Client) RequestIframe(ctx context.Context, channel uint8, stream string) error {
	if err := c.Login(ctx); err != nil {
		return err
	}
	_, err := c.sendRequest(ctx, request{
		MsgID:     MsgIframeRequest,
		ChannelID: channel + 1,
		Class:     classModernWithOffset,
		Extension: channelExtension(channel),
		Body:      IframeBody(channel, stream),
	})
	return err
}

// IframeBody is the RequestIframe body; the firmware takes the channel from
// here, not from the extension, and fails without a body.
func IframeBody(channel uint8, stream string) []byte {
	return []byte(fmt.Sprintf("%s<body>\n<RequestIframe version=\"1.1\">\n<channelId>%d</channelId>\n<streamType>%s</streamType>\n</RequestIframe>\n</body>\n", xmlHeader, channel, stream))
}

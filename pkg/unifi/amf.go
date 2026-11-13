package unifi

import (
	"encoding/binary"
	"math"
)

// Prop is one key/value pair of an AMF0 object. Objects are slices, not maps,
// because Protect is fed the exact key order a real camera (or ffmpeg) sends.
type Prop struct {
	Key   string
	Value any
}

// Object encodes as an AMF0 object (0x03). flvlib3, which the Python proxy uses
// for onClockSync/onMpma, writes its FLVObject this way.
type Object []Prop

// ECMAArray encodes as an AMF0 ECMA array (0x08), the type ffmpeg uses for onMetaData.
type ECMAArray []Prop

// ScriptData encodes a script tag body: the name as AMF0 string, then the value.
func ScriptData(name string, value any) []byte {
	b := appendAMFString(nil, name)
	return appendAMFValue(b, value)
}

func appendAMFString(b []byte, s string) []byte {
	b = append(b, 0x02, byte(len(s)>>8), byte(len(s)))
	return append(b, s...)
}

func appendAMFKey(b []byte, s string) []byte {
	b = append(b, byte(len(s)>>8), byte(len(s)))
	return append(b, s...)
}

func appendAMFNumber(b []byte, n float64) []byte {
	b = append(b, 0x00)
	return binary.BigEndian.AppendUint64(b, math.Float64bits(n))
}

func appendAMFValue(b []byte, value any) []byte {
	switch v := value.(type) {
	case float64:
		return appendAMFNumber(b, v)
	case int:
		return appendAMFNumber(b, float64(v))
	case uint32:
		return appendAMFNumber(b, float64(v))
	case int64:
		return appendAMFNumber(b, float64(v))
	case bool:
		if v {
			return append(b, 0x01, 1)
		}
		return append(b, 0x01, 0)
	case string:
		return appendAMFString(b, v)
	case Object:
		b = append(b, 0x03)
		return appendAMFProps(b, v)
	case ECMAArray:
		n := len(v)
		b = append(b, 0x08, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
		return appendAMFProps(b, v)
	default:
		panic("unifi: unsupported AMF value")
	}
}

func appendAMFProps(b []byte, props []Prop) []byte {
	for _, p := range props {
		b = appendAMFKey(b, p.Key)
		b = appendAMFValue(b, p.Value)
	}
	return append(b, 0, 0, 0x09) // empty key + object end marker
}

package windsurf

// Hand-rolled protobuf encoding for Windsurf LS gRPC protocol.
// Avoids heavy proto dependency — only encodes the specific messages we need.

import (
	"encoding/binary"
	"math"
	"math/rand"
	"time"
)

// Wire types
const (
	wireVarint  = 0
	wire64      = 1
	wireLen     = 2
	wire32      = 5
)

type pbuf struct {
	buf []byte
}

func (p *pbuf) bytes() []byte { return p.buf }

func (p *pbuf) varint(fieldNum int, v uint64) {
	p.tag(fieldNum, wireVarint)
	p.rawVarint(v)
}

func (p *pbuf) str(fieldNum int, s string) {
	if s == "" {
		return
	}
	p.tag(fieldNum, wireLen)
	p.rawVarint(uint64(len(s)))
	p.buf = append(p.buf, s...)
}

func (p *pbuf) bytesField(fieldNum int, b []byte) {
	if len(b) == 0 {
		return
	}
	p.tag(fieldNum, wireLen)
	p.rawVarint(uint64(len(b)))
	p.buf = append(p.buf, b...)
}

func (p *pbuf) msg(fieldNum int, sub *pbuf) {
	if sub == nil || len(sub.buf) == 0 {
		return
	}
	p.bytesField(fieldNum, sub.buf)
}

func (p *pbuf) tag(fieldNum int, wireType int) {
	p.rawVarint(uint64(fieldNum<<3 | wireType))
}

func (p *pbuf) rawVarint(v uint64) {
	for v >= 0x80 {
		p.buf = append(p.buf, byte(v)|0x80)
		v >>= 7
	}
	p.buf = append(p.buf, byte(v))
}

// buildMetadata encodes the Metadata message.
func buildMetadata(apiKey, sessionID string) []byte {
	var m pbuf
	m.str(1, "windsurf")                       // ide_name
	m.str(2, "2.0.67")                         // extension_version
	m.str(3, apiKey)                           // api_key
	m.str(4, "en")                             // locale
	m.str(5, "linux")                          // os
	m.str(7, "2.0.67")                         // ide_version
	m.str(8, "x86_64")                         // hardware
	m.varint(9, rand.Uint64()&0xFFFFFFFFFFFF)  // request_id (48-bit)
	m.str(10, sessionID)                       // session_id
	m.str(12, "windsurf")                      // extension_name
	return m.bytes()
}

// ChatMessageSource enum
const (
	sourceUser      = 1
	sourceSystem    = 2
	sourceAssistant = 3
	sourceTool      = 4
)

// buildChatMessage encodes a single ChatMessage.
func buildChatMessage(msgID string, source int, text string, conversationID string) []byte {
	var intent pbuf
	var generic pbuf
	generic.str(1, text)

	// Timestamp: {seconds = field 1}
	var ts pbuf
	ts.varint(1, uint64(time.Now().Unix()))

	var msg pbuf
	msg.str(1, msgID)
	msg.varint(2, uint64(source))
	msg.msg(3, &ts)              // timestamp (required)
	msg.str(4, conversationID)   // conversation_id (required)

	if source == sourceAssistant {
		// action.generic.text
		var action pbuf
		var actionGeneric pbuf
		actionGeneric.str(1, text)
		action.msg(1, &actionGeneric)
		msg.msg(6, &action)
	} else {
		// intent.generic.text
		intent.msg(1, &generic)
		msg.msg(5, &intent)
	}
	return msg.bytes()
}

// buildRawGetChatMessageRequest builds the full request proto.
func buildRawGetChatMessageRequest(apiKey, sessionID, model string, messages []chatMsg) []byte {
	metadata := buildMetadata(apiKey, sessionID)
	conversationID := sessionID // use session as conversation ID

	var req pbuf
	req.bytesField(1, metadata) // metadata

	for _, m := range messages {
		msgBytes := buildChatMessage(m.id, m.source, m.text, conversationID)
		req.bytesField(2, msgBytes) // repeated messages
	}

	// chat_model_name (field 5)
	req.str(5, model)

	return req.bytes()
}

type chatMsg struct {
	id     string
	source int
	text   string
}

// grpcFrame wraps a protobuf message in the standard 5-byte gRPC frame.
func grpcFrame(msg []byte) []byte {
	frame := make([]byte, 5+len(msg))
	frame[0] = 0 // no compression
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(msg)))
	copy(frame[5:], msg)
	return frame
}

// --- Protobuf decoding (minimal, for streaming response) ---

type protoField struct {
	num      int
	wireType int
	data     []byte   // for wireLen
	val      uint64   // for wireVarint/wire64/wire32
}

func decodeProto(buf []byte) []protoField {
	var fields []protoField
	for len(buf) > 0 {
		tag, n := decodeVarint(buf)
		if n == 0 {
			break
		}
		buf = buf[n:]
		fieldNum := int(tag >> 3)
		wt := int(tag & 7)
		switch wt {
		case wireVarint:
			v, vn := decodeVarint(buf)
			if vn == 0 {
				return fields
			}
			fields = append(fields, protoField{num: fieldNum, wireType: wt, val: v})
			buf = buf[vn:]
		case wire64:
			if len(buf) < 8 {
				return fields
			}
			v := binary.LittleEndian.Uint64(buf[:8])
			fields = append(fields, protoField{num: fieldNum, wireType: wt, val: v})
			buf = buf[8:]
		case wireLen:
			length, ln := decodeVarint(buf)
			if ln == 0 || int(length) > len(buf[ln:]) {
				return fields
			}
			data := buf[ln : ln+int(length)]
			fields = append(fields, protoField{num: fieldNum, wireType: wt, data: data})
			buf = buf[ln+int(length):]
		case wire32:
			if len(buf) < 4 {
				return fields
			}
			v := uint64(binary.LittleEndian.Uint32(buf[:4]))
			fields = append(fields, protoField{num: fieldNum, wireType: wt, val: v})
			buf = buf[4:]
		default:
			return fields
		}
	}
	return fields
}

func decodeVarint(buf []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, b := range buf {
		if i >= 10 {
			return 0, 0
		}
		if b < 0x80 {
			return x | uint64(b)<<s, i + 1
		}
		x |= uint64(b&0x7f) << s
		s += 7
	}
	return 0, 0
}

func getString(fields []protoField, num int) string {
	for _, f := range fields {
		if f.num == num && f.wireType == wireLen {
			return string(f.data)
		}
	}
	return ""
}

func getBool(fields []protoField, num int) bool {
	for _, f := range fields {
		if f.num == num && f.wireType == wireVarint {
			return f.val != 0
		}
	}
	return false
}

func getMsg(fields []protoField, num int) []byte {
	for _, f := range fields {
		if f.num == num && f.wireType == wireLen {
			return f.data
		}
	}
	return nil
}

var _ = math.MaxInt // suppress unused import

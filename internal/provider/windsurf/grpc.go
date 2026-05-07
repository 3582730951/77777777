package windsurf

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
)

const (
	grpcPath = "/exa.language_server_pb.LanguageServerService/RawGetChatMessage"
)

// rawGetChatMessage sends a server-streaming gRPC call to the local LS.
// Returns an io.ReadCloser that yields gRPC frames.
func (m *LSManager) rawGetChatMessage(ctx context.Context, reqProto []byte) (io.ReadCloser, error) {
	body := grpcFrame(reqProto)
	url := m.Addr() + grpcPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("Te", "trailers")
	req.Header.Set("x-codeium-csrf-token", m.csrfToken)

	resp, err := m.h2c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("grpc call: %w", err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("grpc status %d: %s", resp.StatusCode, string(b))
	}
	return resp.Body, nil
}

// readGRPCFrame reads one gRPC frame (5-byte header + payload) from r.
func readGRPCFrame(r io.Reader) ([]byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length > 4*1024*1024 {
		return nil, fmt.Errorf("grpc frame too large: %d bytes", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// parseRawChatResponse extracts text + in_progress + is_error from
// a RawGetChatMessageResponse protobuf.
//
// message RawGetChatMessageResponse { RawChatMessage delta_message = 1; }
// message RawChatMessage {
//   string text = 5;
//   bool in_progress = 6;
//   bool is_error = 7;
// }
func parseRawChatResponse(payload []byte) (text string, inProgress bool, isError bool) {
	outer := decodeProto(payload)
	msgData := getMsg(outer, 1) // delta_message
	if msgData == nil {
		return "", false, false
	}
	inner := decodeProto(msgData)
	text = getString(inner, 5)
	inProgress = getBool(inner, 6)
	isError = getBool(inner, 7)
	return
}

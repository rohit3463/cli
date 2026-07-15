package webbrowser

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Wire protocol between the browser client (frontend) and the gateway CDP handler.
// The backend relays these bytes verbatim (like the RDP path). Every message is
// length-prefixed: [1 byte type][4 byte big-endian length][payload].
type MsgType byte

const (
	MsgFrame   MsgType = 1 // gateway -> client: a JPEG screencast frame (payload = JPEG bytes)
	MsgInput   MsgType = 2 // client -> gateway: an input event (payload = JSON InputEvent)
	MsgControl MsgType = 3 // either direction: control/status (payload = JSON ControlMsg)
)

const maxMessageBytes = 32 * 1024 * 1024 // safety ceiling per message

// InputEvent is sent by the client for every user interaction. Coordinates are
// normalized (0..1) relative to the displayed canvas; the gateway maps them onto
// the locked remote viewport, so the client never needs the remote resolution.
type InputEvent struct {
	Kind      string  `json:"kind"`      // "mouse" | "wheel" | "key" | "resize"
	EventType string  `json:"eventType"` // mouse: down|up|move ; key: down|up|char
	NX        float64 `json:"nx"`        // normalized x (0..1)
	NY        float64 `json:"ny"`        // normalized y (0..1)
	Button    string  `json:"button"`    // left|right|middle|none
	DeltaX    float64 `json:"deltaX"`
	DeltaY    float64 `json:"deltaY"`
	Key       string  `json:"key"`
	Code      string  `json:"code"`
	Text      string  `json:"text"`
	Modifiers int64   `json:"modifiers"`
	Width     int64   `json:"width"`  // resize: new viewport width
	Height    int64   `json:"height"` // resize: new viewport height
}

// ControlMsg carries status/lifecycle signals.
type ControlMsg struct {
	Kind   string `json:"kind"`             // "ready" | "navigated" | "error"
	URL    string `json:"url,omitempty"`    // for "navigated"
	Detail string `json:"detail,omitempty"` // for "error"
}

func writeMessage(w io.Writer, t MsgType, payload []byte) error {
	var hdr [5]byte
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

func readMessage(r io.Reader) (MsgType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxMessageBytes {
		return 0, nil, fmt.Errorf("webbrowser: message too large (%d bytes)", n)
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return MsgType(hdr[0]), payload, nil
}

package webbrowser

import (
	"encoding/base64"
	"encoding/json"

	"github.com/Infisical/infisical-merge/packages/pam/session"
)

// webEventEnvelope is the JSON payload stored in each web SessionEvent's Data.
// Kind discriminates a video "frame" from an "activity" (click/key/navigate),
// so replay can render the video track and the synchronized activity timeline.
type webEventEnvelope struct {
	Kind      string `json:"kind"` // "frame" | "activity"
	ElapsedNs int64  `json:"elapsedNs"`

	// frame
	Image string `json:"image,omitempty"` // base64-encoded JPEG

	// activity
	Type string  `json:"type,omitempty"` // "click" | "key" | "navigate"
	NX   float64 `json:"nx,omitempty"`
	NY   float64 `json:"ny,omitempty"`
	Key  string  `json:"key,omitempty"`
	URL  string  `json:"url,omitempty"`
}

type sessionLoggerRecorder struct {
	logger session.SessionLogger
}

// NewSessionLoggerRecorder returns a Recorder that writes the session video
// (frames) and activity events into the PAM recording pipeline (chunked,
// encrypted, uploaded).
func NewSessionLoggerRecorder(logger session.SessionLogger) Recorder {
	return &sessionLoggerRecorder{logger: logger}
}

func (r *sessionLoggerRecorder) emit(env webEventEnvelope) {
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	_ = r.logger.LogSessionEvent(session.SessionEvent{
		EventType:   session.SessionEventWeb,
		ChannelType: session.SessionChannelWeb,
		Data:        data,
		ElapsedTime: float64(env.ElapsedNs) / 1e9,
	})
}

func (r *sessionLoggerRecorder) RecordFrame(elapsedNs int64, jpeg []byte) {
	r.emit(webEventEnvelope{
		Kind:      "frame",
		ElapsedNs: elapsedNs,
		Image:     base64.StdEncoding.EncodeToString(jpeg),
	})
}

func (r *sessionLoggerRecorder) RecordActivity(meta InteractionMeta) {
	r.emit(webEventEnvelope{
		Kind:      "activity",
		ElapsedNs: meta.ElapsedNs,
		Type:      meta.Type,
		NX:        meta.NX,
		NY:        meta.NY,
		Key:       meta.Key,
		URL:       meta.URL,
	})
}

package webbrowser

import (
	"encoding/base64"
	"encoding/json"

	"github.com/Infisical/infisical-merge/packages/pam/session"
)

// webInteractionEnvelope is the JSON payload stored in each web SessionEvent's Data.
// before/after are base64-encoded PNG screenshots captured around an interaction.
type webInteractionEnvelope struct {
	Type      string  `json:"type"` // "click" | "key" | "navigate"
	NX        float64 `json:"nx,omitempty"`
	NY        float64 `json:"ny,omitempty"`
	Key       string  `json:"key,omitempty"`
	URL       string  `json:"url,omitempty"`
	ElapsedNs int64   `json:"elapsedNs"`
	Before    string  `json:"before,omitempty"`
	After     string  `json:"after,omitempty"`
}

type sessionLoggerRecorder struct {
	logger session.SessionLogger
}

// NewSessionLoggerRecorder returns a Recorder that writes before/after screenshots
// as web SessionEvents into the PAM recording pipeline (chunked, encrypted, uploaded).
func NewSessionLoggerRecorder(logger session.SessionLogger) Recorder {
	return &sessionLoggerRecorder{logger: logger}
}

func (r *sessionLoggerRecorder) RecordInteraction(meta InteractionMeta, before, after []byte) {
	env := webInteractionEnvelope{
		Type:      meta.Type,
		NX:        meta.NX,
		NY:        meta.NY,
		Key:       meta.Key,
		URL:       meta.URL,
		ElapsedNs: meta.ElapsedNs,
	}
	if len(before) > 0 {
		env.Before = base64.StdEncoding.EncodeToString(before)
	}
	if len(after) > 0 {
		env.After = base64.StdEncoding.EncodeToString(after)
	}
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	_ = r.logger.LogSessionEvent(session.SessionEvent{
		EventType:   session.SessionEventWeb,
		ChannelType: session.SessionChannelWeb,
		Data:        data,
		ElapsedTime: float64(meta.ElapsedNs) / 1e9,
	})
}

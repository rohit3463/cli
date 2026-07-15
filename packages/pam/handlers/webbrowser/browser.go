// Package webbrowser is the gateway-side PAM handler for remote web-application
// access. It spawns a fresh headless Chromium per session (throwaway user-data-dir,
// killed on session end -> inherent per-user isolation), drives it over CDP, streams
// JPEG screencast frames to the client, injects the client's input, and records the
// session as before/after screenshots around each interaction.
package webbrowser

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/rs/zerolog/log"
)

// InteractionMeta describes a recorded interaction (the "report" granularity).
type InteractionMeta struct {
	Type      string  `json:"type"` // "click" | "key" | "navigate"
	NX        float64 `json:"nx,omitempty"`
	NY        float64 `json:"ny,omitempty"`
	Key       string  `json:"key,omitempty"`
	URL       string  `json:"url,omitempty"`
	ElapsedNs int64   `json:"elapsedNs"`
}

// Recorder receives before/after screenshots for each meaningful interaction.
// The gateway implements it against the PAM SessionLogger; the test harness
// implements it by writing PNGs to disk.
type Recorder interface {
	RecordInteraction(meta InteractionMeta, beforePNG, afterPNG []byte)
}

// Config parameterizes a web session.
type Config struct {
	TargetURL  string
	Username   string // optional: injected server-side for HTTP basic auth
	Password   string
	VerifyTLS  bool
	Width      int64
	Height     int64
	Quality    int64
	ChromePath string // optional; empty = chromedp auto-detect
}

const settleDelay = 400 * time.Millisecond

func (c *Config) applyDefaults() {
	if c.Width == 0 {
		c.Width = 1280
	}
	if c.Height == 0 {
		c.Height = 720
	}
	if c.Quality == 0 {
		c.Quality = 60
	}
}

// Run drives a full web session against conn until conn closes or ctx is done.
// It owns the Chromium lifecycle and cleans it (process + temp dir) on return.
func Run(ctx context.Context, conn io.ReadWriter, cfg Config, rec Recorder) error {
	cfg.applyDefaults()

	userDataDir, err := os.MkdirTemp("", "infisical-pam-web-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(userDataDir)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless,
		chromedp.NoSandbox,
		chromedp.DisableGPU,
		chromedp.UserDataDir(userDataDir),
		chromedp.WindowSize(int(cfg.Width), int(cfg.Height)),
		// The gateway sits inside the private network and reaches targets directly;
		// never route through a proxy inherited from the gateway's environment
		// (an inherited HTTP(S)_PROXY would otherwise make navigation hang).
		chromedp.Flag("no-proxy-server", true),
	)
	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}
	// Capture Chromium's own stdout/stderr to a file to diagnose launch/nav failures.
	if chromeLog, logErr := os.CreateTemp("", "infisical-pam-chrome-*.log"); logErr == nil {
		opts = append(opts, chromedp.CombinedOutput(chromeLog))
		log.Info().Str("chromeLog", chromeLog.Name()).Msg("webbrowser: chromium output log")
		defer chromeLog.Close()
	}

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	cdpCtx, cancelCtx := chromedp.NewContext(allocCtx)
	defer cancelCtx()

	startedAt := time.Now()

	// chromedp multiplexes CDP commands by message id, so concurrent Run calls (input
	// dispatch, screenshot capture, screencast acks, and fetch request/auth continues)
	// are safe. We deliberately do NOT serialize with a mutex: with request interception
	// (fetch, enabled for basic-auth injection) the paused requests must be continued
	// WHILE the navigation is in flight, or the page never finishes loading — a mutex
	// held across the navigation would deadlock exactly that.
	runCDP := func(actions ...chromedp.Action) error {
		return chromedp.Run(cdpCtx, actions...)
	}

	// The remote viewport's actual CSS dimensions, learned from each screencast frame's
	// metadata. Client input arrives normalized (0..1); we scale by these so clicks map
	// exactly even when the real viewport differs from the requested size.
	var (
		viewMu sync.RWMutex
		viewW  = float64(cfg.Width)
		viewH  = float64(cfg.Height)
	)
	viewport := func() (float64, float64) {
		viewMu.RLock()
		defer viewMu.RUnlock()
		return viewW, viewH
	}

	// Outbound writer: one goroutine owns all writes to the client connection so the
	// chromedp event goroutine (ListenTarget) never blocks on network I/O. A blocked
	// handler would stall CDP event processing and hang navigation/screencast.
	type outMsg struct {
		t       MsgType
		payload []byte
	}
	outbound := make(chan outMsg, 64)
	go func() {
		for m := range outbound {
			if err := writeMessage(conn, m.t, m.payload); err != nil {
				return
			}
		}
	}()
	sendFrame := func(data []byte) {
		select {
		case outbound <- outMsg{MsgFrame, data}:
		default: // client is behind; drop this frame rather than stall CDP
		}
	}
	sendControl := func(payload []byte) {
		select {
		case outbound <- outMsg{MsgControl, payload}:
		default:
		}
	}

	elapsed := func() int64 { return time.Since(startedAt).Nanoseconds() }

	captureAfter := func(meta InteractionMeta, before []byte) {
		time.Sleep(settleDelay)
		var after []byte
		_ = runCDP(chromedp.CaptureScreenshot(&after))
		rec.RecordInteraction(meta, before, after)
	}

	var frameCount int64
	chromedp.ListenTarget(cdpCtx, func(ev interface{}) {
		switch e := ev.(type) {
		case *page.EventScreencastFrame:
			// Track the true viewport dims from frame metadata for exact input mapping.
			if e.Metadata != nil && e.Metadata.DeviceWidth > 0 && e.Metadata.DeviceHeight > 0 {
				viewMu.Lock()
				viewW = e.Metadata.DeviceWidth
				viewH = e.Metadata.DeviceHeight
				viewMu.Unlock()
			}
			// Data is a base64-encoded JPEG; decode to raw bytes for the wire protocol.
			if data, derr := base64.StdEncoding.DecodeString(e.Data); derr == nil {
				frameCount++
				if frameCount == 1 || frameCount%30 == 0 {
					m := e.Metadata
					evt := log.Info().Int64("frames", frameCount).Int("bytes", len(data))
					if m != nil {
						evt = evt.Float64("devW", m.DeviceWidth).Float64("devH", m.DeviceHeight).
							Float64("pageScale", m.PageScaleFactor).Float64("offsetTop", m.OffsetTop)
					}
					evt.Msg("webbrowser: screencast frame")
				}
				sendFrame(data)
			}
			sid := e.SessionID
			go func() { _ = runCDP(page.ScreencastFrameAck(sid)) }()
		case *page.EventJavascriptDialogOpening:
			// Auto-dismiss alerts/confirms/prompts so the page can't hang the session.
			go func() { _ = runCDP(page.HandleJavaScriptDialog(false)) }()
		case *page.EventFrameNavigated:
			if e.Frame != nil && e.Frame.ParentID == "" {
				url := e.Frame.URL
				sendControl(mustJSON(ControlMsg{Kind: "navigated", URL: url}))
				if rec != nil {
					meta := InteractionMeta{Type: "navigate", URL: url, ElapsedNs: elapsed()}
					go captureAfter(meta, nil)
				}
			}
		case *fetch.EventAuthRequired:
			resp := &fetch.AuthChallengeResponse{Response: fetch.AuthChallengeResponseResponseCancelAuth}
			if cfg.Username != "" {
				resp.Response = fetch.AuthChallengeResponseResponseProvideCredentials
				resp.Username = cfg.Username
				resp.Password = cfg.Password
			}
			id := e.RequestID
			go func() { _ = runCDP(fetch.ContinueWithAuth(id, resp)) }()
		case *fetch.EventRequestPaused:
			id := e.RequestID
			go func() { _ = runCDP(fetch.ContinueRequest(id)) }()
		}
	})

	setup := []chromedp.Action{
		emulation.SetDeviceMetricsOverride(cfg.Width, cfg.Height, 1, false),
		browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorDeny),
	}
	// Only intercept requests when we need to inject basic-auth credentials —
	// otherwise avoid pausing every request.
	if cfg.Username != "" {
		setup = append(setup, fetch.Enable().WithHandleAuthRequests(true))
	}
	setup = append(setup,
		chromedp.Navigate(cfg.TargetURL),
		page.StartScreencast().
			WithFormat(page.ScreencastFormatJpeg).
			WithQuality(cfg.Quality).
			WithMaxWidth(cfg.Width).
			WithMaxHeight(cfg.Height),
	)

	// Launch Chromium + configure + navigate + start screencast in one batch on the
	// session context. (Using a short-lived timeout child here would tear down the
	// chromedp target when cancelled, so we rely on the session context's lifetime.)
	log.Info().Str("url", cfg.TargetURL).Msg("webbrowser: launching chromium and navigating")
	if err := runCDP(setup...); err != nil {
		log.Error().Err(err).Str("url", cfg.TargetURL).Msg("webbrowser: setup failed")
		sendControl(mustJSON(ControlMsg{Kind: "error", Detail: err.Error()}))
		return err
	}
	log.Info().Str("url", cfg.TargetURL).Msg("webbrowser: session ready, streaming frames")
	sendControl(mustJSON(ControlMsg{Kind: "ready"}))

	// Screencast only emits on visual change, so a static page can leave the client
	// black until the first interaction. Push an immediate screenshot as the first frame.
	go func() {
		var shot []byte
		if runCDP(chromedp.CaptureScreenshot(&shot)) == nil && len(shot) > 0 {
			sendFrame(shot)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		var inputCount int64
		for {
			t, payload, rErr := readMessage(conn)
			if rErr != nil {
				errCh <- rErr
				return
			}
			if t != MsgInput {
				continue
			}
			var ev InputEvent
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			inputCount++
			// Log discrete interactions (clicks/scroll/keys) with coords; skip noisy moves.
			if ev.Kind != "mouse" || ev.EventType != "move" {
				log.Info().
					Str("kind", ev.Kind).
					Str("eventType", ev.EventType).
					Float64("nx", ev.NX).
					Float64("ny", ev.NY).
					Str("button", ev.Button).
					Str("key", ev.Key).
					Msg("webbrowser: input")
			}
			dispatchInput(runCDP, viewport, ev, rec, elapsed, captureAfter)
		}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-cdpCtx.Done():
		return cdpCtx.Err()
	case err := <-errCh:
		return err
	}
}

func dispatchInput(
	runCDP func(...chromedp.Action) error,
	viewport func() (float64, float64),
	ev InputEvent,
	rec Recorder,
	elapsed func() int64,
	captureAfter func(InteractionMeta, []byte),
) {
	w, h := viewport()
	x := ev.NX * w
	y := ev.NY * h

	switch ev.Kind {
	case "mouse":
		mt, ok := mouseType(ev.EventType)
		if !ok {
			return
		}
		p := input.DispatchMouseEvent(mt, x, y).WithButton(mouseButton(ev.Button)).WithModifiers(input.Modifier(ev.Modifiers))
		if mt == input.MousePressed || mt == input.MouseReleased {
			p = p.WithClickCount(1)
		}
		if rec != nil && ev.EventType == "down" && ev.Button != "none" {
			var before []byte
			_ = runCDP(chromedp.CaptureScreenshot(&before))
			_ = runCDP(p)
			go captureAfter(InteractionMeta{Type: "click", NX: ev.NX, NY: ev.NY, ElapsedNs: elapsed()}, before)
			return
		}
		_ = runCDP(p)

	case "wheel":
		_ = runCDP(input.DispatchMouseEvent(input.MouseWheel, x, y).WithDeltaX(ev.DeltaX).WithDeltaY(ev.DeltaY))

	case "key":
		kt, ok := keyType(ev.EventType)
		if !ok {
			return
		}
		p := input.DispatchKeyEvent(kt).WithKey(ev.Key).WithCode(ev.Code).WithText(ev.Text).WithModifiers(input.Modifier(ev.Modifiers))
		if rec != nil && ev.EventType == "down" && ev.Key == "Enter" {
			var before []byte
			_ = runCDP(chromedp.CaptureScreenshot(&before))
			_ = runCDP(p)
			go captureAfter(InteractionMeta{Type: "key", Key: ev.Key, ElapsedNs: elapsed()}, before)
			return
		}
		_ = runCDP(p)
	}
	// "resize" is intentionally not handled in this phase; the viewport is locked.
}

func mouseType(t string) (input.MouseType, bool) {
	switch t {
	case "down":
		return input.MousePressed, true
	case "up":
		return input.MouseReleased, true
	case "move":
		return input.MouseMoved, true
	}
	return "", false
}

func mouseButton(b string) input.MouseButton {
	switch b {
	case "left":
		return input.Left
	case "right":
		return input.Right
	case "middle":
		return input.Middle
	}
	return input.None
}

func keyType(t string) (input.KeyType, bool) {
	switch t {
	case "down":
		return input.KeyDown, true
	case "up":
		return input.KeyUp, true
	case "char":
		return input.KeyChar, true
	}
	return "", false
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

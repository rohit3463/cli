package webbrowser

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

type recorded struct {
	meta            InteractionMeta
	before, after   int // byte lengths
}

type testRecorder struct {
	ch  chan recorded
	dir string
}

func (r *testRecorder) RecordInteraction(meta InteractionMeta, before, after []byte) {
	if len(before) > 0 {
		_ = os.WriteFile(filepath.Join(r.dir, fmt.Sprintf("%d-%s-before.png", meta.ElapsedNs, meta.Type)), before, 0o644)
	}
	if len(after) > 0 {
		_ = os.WriteFile(filepath.Join(r.dir, fmt.Sprintf("%d-%s-after.png", meta.ElapsedNs, meta.Type)), after, 0o644)
	}
	select {
	case r.ch <- recorded{meta: meta, before: len(before), after: len(after)}:
	default:
	}
}

// TestWebBrowserEngine drives the CDP engine end-to-end against a local page:
// it verifies screencast frames flow, a click is dispatched, and before/after
// screenshots are captured. Requires a local Chrome/Chromium.
//
//	RUN_WEBBROWSER_TEST=1 go test ./packages/pam/handlers/webbrowser/ -run TestWebBrowserEngine -v
func TestWebBrowserEngine(t *testing.T) {
	if os.Getenv("RUN_WEBBROWSER_TEST") == "" {
		t.Skip("set RUN_WEBBROWSER_TEST=1 (requires a local Chrome/Chromium) to run")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body style="margin:0">
<button id="b" style="width:100vw;height:100vh;font-size:48px"
 onclick="document.body.style.background='#0b8043';this.textContent='CLICKED'">CLICK ME</button>
</body></html>`)
	}))
	defer srv.Close()

	dir := filepath.Join(os.TempDir(), "infisical-pam-web-test")
	_ = os.MkdirAll(dir, 0o755)
	rec := &testRecorder{ch: make(chan recorded, 16), dir: dir}

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	go func() {
		_ = Run(ctx, serverConn, Config{TargetURL: srv.URL, ChromePath: os.Getenv("CHROME_PATH")}, rec)
	}()

	var frames int64
	ready := make(chan struct{}, 1)
	go func() {
		for {
			mt, payload, err := readMessage(clientConn)
			if err != nil {
				return
			}
			switch mt {
			case MsgFrame:
				atomic.AddInt64(&frames, 1)
			case MsgControl:
				var c ControlMsg
				if json.Unmarshal(payload, &c) == nil && c.Kind == "ready" {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("did not receive 'ready' control message")
	}

	// let a few frames stream, then click the center (the full-viewport button)
	time.Sleep(3 * time.Second)
	if err := writeMessage(clientConn, MsgInput, mustJSON(InputEvent{Kind: "mouse", EventType: "down", Button: "left", NX: 0.5, NY: 0.5})); err != nil {
		t.Fatalf("send mouse down: %v", err)
	}
	_ = writeMessage(clientConn, MsgInput, mustJSON(InputEvent{Kind: "mouse", EventType: "up", Button: "left", NX: 0.5, NY: 0.5}))

	// Drain interactions until we see the click, which must carry BOTH a before
	// and an after screenshot (the core "report" behavior).
	deadline := time.After(20 * time.Second)
	gotClick := false
	for !gotClick {
		select {
		case r := <-rec.ch:
			t.Logf("recorded %s interaction (before=%dB after=%dB)", r.meta.Type, r.before, r.after)
			if r.meta.Type == "click" {
				if r.before == 0 || r.after == 0 {
					t.Fatalf("click interaction missing before/after screenshot (before=%d after=%d)", r.before, r.after)
				}
				gotClick = true
			}
		case <-deadline:
			t.Fatal("no click interaction with before/after recorded")
		}
	}

	if n := atomic.LoadInt64(&frames); n == 0 {
		t.Fatal("no screencast frames received")
	}
	t.Logf("OK: screencast frames=%d; before/after PNGs written to %s", atomic.LoadInt64(&frames), dir)
}

// TestChromedpNav isolates Chromium launch + navigation from the protocol code.
//   RUN_WEBBROWSER_TEST=1 NAV_URL=http://localhost:8088 go test ./packages/pam/handlers/webbrowser/ -run TestChromedpNav -v
func TestChromedpNav(t *testing.T) {
	if os.Getenv("RUN_WEBBROWSER_TEST") == "" {
		t.Skip("set RUN_WEBBROWSER_TEST=1")
	}
	url := os.Getenv("NAV_URL")
	if url == "" {
		url = "http://localhost:8088"
	}
	dir, _ := os.MkdirTemp("", "cdp-nav-")
	defer os.RemoveAll(dir)
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU, chromedp.UserDataDir(dir))
	allocCtx, c1 := chromedp.NewExecAllocator(context.Background(), opts...)
	defer c1()
	ctx, c2 := chromedp.NewContext(allocCtx)
	defer c2()
	tctx, c3 := context.WithTimeout(ctx, 30*time.Second)
	defer c3()
	start := time.Now()
	var title string
	err := chromedp.Run(tctx, chromedp.Navigate(url), chromedp.Title(&title))
	t.Logf("nav to %s took %v err=%v title=%q", url, time.Since(start), err, title)
	if err != nil {
		t.Fatalf("navigation failed: %v", err)
	}
}

// TestWebBrowserReadyURL runs the full Run() path (screencast + listener) against a
// real URL and waits for the "ready" control, reproducing the gateway path minus the tunnel.
//   RUN_WEBBROWSER_TEST=1 NAV_URL=http://localhost:8088 go test ./packages/pam/handlers/webbrowser/ -run TestWebBrowserReadyURL -v
func TestWebBrowserReadyURL(t *testing.T) {
	if os.Getenv("RUN_WEBBROWSER_TEST") == "" {
		t.Skip("set RUN_WEBBROWSER_TEST=1")
	}
	url := os.Getenv("NAV_URL")
	if url == "" {
		url = "http://localhost:8088"
	}
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Username set -> enables fetch request interception (the deadlock trigger we're verifying is fixed).
	go func() { _ = Run(ctx, serverConn, Config{TargetURL: url, Username: "testuser", Password: "x"}, nil) }()

	result := make(chan string, 1)
	go func() {
		for {
			mt, payload, err := readMessage(clientConn)
			if err != nil {
				return
			}
			if mt == MsgControl {
				var c ControlMsg
				if json.Unmarshal(payload, &c) == nil && (c.Kind == "ready" || c.Kind == "error") {
					select {
					case result <- c.Kind + " " + c.Detail:
					default:
					}
				}
			}
		}
	}()
	select {
	case r := <-result:
		t.Logf("got control: %s", r)
	case <-time.After(50 * time.Second):
		t.Fatal("timed out waiting for ready/error control")
	}
}

// TestWebBrowserFetchClickFrames reproduces the gateway condition (username -> fetch
// interception) and verifies a click both registers AND produces new screencast frames.
//   RUN_WEBBROWSER_TEST=1 go test ./packages/pam/handlers/webbrowser/ -run TestWebBrowserFetchClickFrames -v
func TestWebBrowserFetchClickFrames(t *testing.T) {
	if os.Getenv("RUN_WEBBROWSER_TEST") == "" {
		t.Skip("set RUN_WEBBROWSER_TEST=1")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<html><body style="margin:0;background:#fff">
<button style="width:100vw;height:100vh;font-size:48px"
 onclick="document.body.style.background='#0b8043';this.textContent='CLICKED'">CLICK ME</button>
</body></html>`)
	}))
	defer srv.Close()

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	go func() { _ = Run(ctx, serverConn, Config{TargetURL: srv.URL, Username: "u", Password: "p"}, nil) }()

	var frames int64
	ready := make(chan struct{}, 1)
	go func() {
		for {
			mt, payload, err := readMessage(clientConn)
			if err != nil {
				return
			}
			if mt == MsgFrame {
				atomic.AddInt64(&frames, 1)
			} else if mt == MsgControl {
				var c ControlMsg
				if json.Unmarshal(payload, &c) == nil && c.Kind == "ready" {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("no ready")
	}
	time.Sleep(1500 * time.Millisecond)
	before := atomic.LoadInt64(&frames)
	_ = writeMessage(clientConn, MsgInput, mustJSON(InputEvent{Kind: "mouse", EventType: "down", Button: "left", NX: 0.5, NY: 0.5}))
	_ = writeMessage(clientConn, MsgInput, mustJSON(InputEvent{Kind: "mouse", EventType: "up", Button: "left", NX: 0.5, NY: 0.5}))
	time.Sleep(2500 * time.Millisecond)
	after := atomic.LoadInt64(&frames)
	t.Logf("frames before click=%d after=%d", before, after)
	if after <= before {
		t.Fatalf("no new frames after click — screencast not emitting change frames under fetch")
	}
}

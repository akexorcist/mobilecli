package devices

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// The JVMTI agent path (android_webview.go) needs `run-as`, which the platform
// only grants for debuggable packages. A release build therefore has no agent
// route at all.
//
// Chrome DevTools Protocol is the second way in, and the one Appium and Maestro
// use: WebView publishes an abstract socket, @webview_devtools_remote_<pid>,
// that speaks CDP. It carries no `run-as` requirement, so it works against a
// non-debuggable build without touching the app, the APK or the device.
//
// It is not unconditional — WebView only opens the socket when the app opted in via
// setWebContentsDebuggingEnabled, when the app is debuggable (automatic as of WebView
// 113), or when the device itself is a userdebug/eng build. That covers emulators and
// test farms while leaving production `user` builds closed, which is the property that
// makes this safe to ship.

// cdpTarget is one entry of the devtools /json/list response.
type cdpTarget struct {
	ID                   string `json:"id"`
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	Title                string `json:"title"`
	Description          string `json:"description"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// cdpDescription is the JSON blob WebView packs into a target's description
// field; it is where the native geometry of the webview lives.
type cdpDescription struct {
	Attached bool `json:"attached"`
	Empty    bool `json:"empty"`
	Visible  bool `json:"visible"`
	ScreenX  int  `json:"screenX"`
	ScreenY  int  `json:"screenY"`
	Width    int  `json:"width"`
	Height   int  `json:"height"`
}

// Protocol key names shared across several CDP calls.
const (
	cdpKeyURL        = "url"
	cdpKeyMethod     = "method"
	cdpKeyExpression = "expression"
)

var devtoolsSocketRe = regexp.MustCompile(`^@webview_devtools_remote_(\d+)$`)

// devtoolsHTTPTimeout bounds the /json calls: http.DefaultClient has no timeout, so a
// half-open adb forward would otherwise hang a webview operation indefinitely.
const devtoolsHTTPTimeout = 10 * time.Second

var devtoolsHTTPClient = &http.Client{Timeout: devtoolsHTTPTimeout}

// parseDevtoolsSocketName picks the listening devtools socket belonging to one of pids.
// Columns of /proc/net/unix are: Num RefCount Protocol Flags Type St Inode Path. Only a
// listening socket (flags 00010000, state 01) is connectable — a webview that has gone
// away can leave a non-listening entry behind under the same name. Returns "" if none.
func parseDevtoolsSocketName(procNetUnix string, pids map[string]bool) string {
	for _, line := range strings.Split(procNetUnix, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 8 || fields[3] != "00010000" || fields[5] != "01" {
			continue
		}
		m := devtoolsSocketRe.FindStringSubmatch(fields[7])
		if m == nil || !pids[m[1]] {
			continue
		}
		return strings.TrimPrefix(m[0], "@")
	}
	return ""
}

// findDevtoolsSocket returns the abstract socket name WebView opened for pkg.
// /proc/net/unix is readable by shell, so this needs no elevated access.
func (d *AndroidDevice) findDevtoolsSocket(pkg string) (string, error) {
	out, err := d.runAdbCommand("shell", "pidof", pkg)
	if err != nil {
		return "", fmt.Errorf("pidof %s: %w", pkg, err)
	}
	pids := map[string]bool{}
	for _, p := range strings.Fields(strings.TrimSpace(string(out))) {
		pids[p] = true
	}
	if len(pids) == 0 {
		return "", fmt.Errorf("no running process for %s — is the app open?", pkg)
	}

	sockets, err := d.runAdbCommand("shell", "cat", "/proc/net/unix")
	if err != nil {
		return "", fmt.Errorf("read /proc/net/unix: %w", err)
	}
	if name := parseDevtoolsSocketName(string(sockets), pids); name != "" {
		return name, nil
	}
	return "", fmt.Errorf("no webview devtools socket found for any process of %s — it may host no "+
		"WebView, host it in a separate android:process, or have WebView debugging off (it is on "+
		"when the app called setWebContentsDebuggingEnabled, when the app is debuggable on "+
		"WebView 113+, or when the device is a userdebug/eng build)", pkg)
}

// findForwardPort returns the host port this device already forwards to target, or 0.
//
// `adb forward --list` prints "<serial> tcp:<port> <target>" and lists every device — it
// ignores the -s argument — so both the serial and the target must match as whole fields:
//
//   - matching the target loosely is wrong because the socket name ends in a pid, and
//     webview_devtools_remote_1234 is a prefix of webview_devtools_remote_12345;
//   - ignoring the serial is wrong because two devices running the same app can land on
//     the same pid, and we would drive the other device's webview.
func findForwardPort(listOutput, serial, target string) int {
	for _, line := range strings.Split(strings.TrimSpace(listOutput), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != serial || fields[2] != target {
			continue
		}
		if port, err := strconv.Atoi(strings.TrimPrefix(fields[1], "tcp:")); err == nil && port > 0 {
			return port
		}
	}
	return 0
}

// ensureCDPForward forwards a local TCP port onto the app's devtools socket,
// reusing an existing forward when one is already in place.
func (d *AndroidDevice) ensureCDPForward(pkg string) (int, error) {
	socket, err := d.findDevtoolsSocket(pkg)
	if err != nil {
		return 0, err
	}
	target := "localabstract:" + socket

	if out, err := d.runAdbCommand("forward", "--list"); err == nil {
		if port := findForwardPort(string(out), d.getAdbIdentifier(), target); port != 0 {
			return port, nil
		}
	}

	out, err := d.runAdbCommand("forward", "tcp:0", target)
	if err != nil {
		return 0, fmt.Errorf("adb forward %s: %s: %w", target, strings.TrimSpace(string(out)), err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("unexpected adb forward output %q: %w", strings.TrimSpace(string(out)), err)
	}
	return port, nil
}

// cdpBackend serves webview operations over the DevTools protocol.
type cdpBackend struct {
	port     int
	bundleID string

	mu       sync.Mutex
	sessions map[string]*cdpSession
}

func (c *cdpBackend) targets() ([]cdpTarget, error) {
	ctx, cancel := context.WithTimeout(context.Background(), devtoolsHTTPTimeout)
	defer cancel()

	url := fmt.Sprintf("http://127.0.0.1:%d/json/list", c.port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("devtools /json/list: %w", err)
	}
	resp, err := devtoolsHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("devtools /json/list: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to recover from on a read-only response

	var all []cdpTarget
	if err := json.NewDecoder(resp.Body).Decode(&all); err != nil {
		return nil, fmt.Errorf("parse devtools target list: %w", err)
	}
	pages := make([]cdpTarget, 0, len(all))
	for _, t := range all {
		if t.Type == "page" {
			pages = append(pages, t)
		}
	}
	return pages, nil
}

func (c *cdpBackend) list() ([]WebViewInfo, error) {
	targets, err := c.targets()
	if err != nil {
		return nil, err
	}
	infos := make([]WebViewInfo, 0, len(targets))
	for _, t := range targets {
		info := WebViewInfo{
			ID:        t.ID,
			URL:       t.URL,
			Title:     t.Title,
			BundleID:  c.bundleID,
			IsVisible: true,
		}
		// WebView reports the native geometry in `description`; reuse it so the
		// client can position-match these against the native view hierarchy the
		// same way it does for agent-reported webviews.
		var desc cdpDescription
		if t.Description != "" && json.Unmarshal([]byte(t.Description), &desc) == nil {
			info.IsVisible = desc.Visible
			info.Bounds = map[string]any{
				"x": desc.ScreenX, "y": desc.ScreenY,
				"width": desc.Width, "height": desc.Height,
			}
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// session returns a live CDP session for a webview id, dialing on first use.
func (c *cdpBackend) session(webviewID string) (*cdpSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.sessions[webviewID]; ok {
		if s.alive() {
			return s, nil
		}
		// gorilla leaves the socket open when the read side fails, so a replaced
		// session leaks its fd unless it is closed explicitly.
		if s.conn != nil {
			s.conn.Close() //nolint:errcheck,gosec // dropping a dead session; the error is moot
		}
		delete(c.sessions, webviewID)
	}

	targets, err := c.targets()
	if err != nil {
		return nil, err
	}
	var target *cdpTarget
	for i := range targets {
		if webviewID == "" || targets[i].ID == webviewID {
			target = &targets[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("webview %q not found among %d devtools target(s)", webviewID, len(targets))
	}

	s, err := dialCDP(target.WebSocketDebuggerURL)
	if err != nil {
		return nil, err
	}
	if c.sessions == nil {
		c.sessions = map[string]*cdpSession{}
	}
	c.sessions[webviewID] = s
	return s, nil
}

// buildEvaluateExpression turns the agent protocol's function *body* into an expression
// CDP can evaluate. Args, when present, are passed through apply() so the body reaches
// them via `arguments`, matching the agent; they are additionally bound as a0..aN, which
// is a CDP-side convenience the agent does not offer.
//
// The body is fenced with newlines because it may end in a line comment, which would
// otherwise swallow the closing brace.
func buildEvaluateExpression(expression string, args []any) (string, error) {
	body := ensureReturnExpression(expression)
	if len(args) == 0 {
		return "(function(){\n" + body + "\n})()", nil
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("encode evaluate args: %w", err)
	}
	names := make([]string, len(args))
	for i := range args {
		names[i] = fmt.Sprintf("a%d", i)
	}
	return "(function(" + strings.Join(names, ",") + "){\n" + body + "\n}).apply(null," + string(encoded) + ")", nil
}

func (c *cdpBackend) evaluate(webviewID, expression string, args []any) (any, error) {
	call, err := buildEvaluateExpression(expression, args)
	if err != nil {
		return nil, err
	}
	s, err := c.session(webviewID)
	if err != nil {
		return nil, err
	}
	return s.evaluate(call)
}

func (c *cdpBackend) goTo(webviewID, url string) error {
	s, err := c.session(webviewID)
	if err != nil {
		return err
	}
	_, err = s.call("Page.navigate", map[string]any{cdpKeyURL: url})
	return err
}

func (c *cdpBackend) reload(webviewID string) error {
	s, err := c.session(webviewID)
	if err != nil {
		return err
	}
	_, err = s.call("Page.reload", map[string]any{})
	return err
}

func (c *cdpBackend) goBack(webviewID string) error {
	_, err := c.evaluate(webviewID, "return history.back()", nil)
	return err
}

func (c *cdpBackend) goForward(webviewID string) error {
	_, err := c.evaluate(webviewID, "return history.forward()", nil)
	return err
}

func (c *cdpBackend) waitForLoadState(webviewID, state string, timeoutMs int) error {
	waitMs := webViewAgentDefaultTimeoutMs
	if timeoutMs > 0 {
		waitMs = timeoutMs
	}
	want := "return document.readyState === 'complete'"
	if state == "domcontentloaded" {
		want = "return document.readyState === 'interactive' || document.readyState === 'complete'"
	}

	deadline := time.Now().Add(time.Duration(waitMs) * time.Millisecond)
	var lastErr error
	for {
		done, err := c.evaluate(webviewID, want, nil)
		lastErr = err
		if err == nil {
			if ready, ok := done.(bool); ok && ready {
				return nil
			}
		}
		if time.Now().After(deadline) {
			// Without this a bad webview id, a dial failure or a dead session all
			// masquerade as "the page never loaded".
			if lastErr != nil {
				return fmt.Errorf("webview did not reach load state %q within %dms: %w",
					state, waitMs, lastErr)
			}
			return fmt.Errorf("webview did not reach load state %q within %dms", state, waitMs)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ── minimal CDP session ─────────────────────────────────────────────────────

type cdpSession struct {
	conn *websocket.Conn

	// gorilla/websocket allows exactly one concurrent writer and panics otherwise, so
	// writes take their own mutex. It cannot be mu: readLoop needs that one while a
	// write is in flight.
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan cdpResponse
	closed  bool
}

type cdpResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
	ID int `json:"id"`
}

func dialCDP(wsURL string) (*cdpSession, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial devtools websocket: %w", err)
	}
	s := &cdpSession{conn: conn, pending: map[int]chan cdpResponse{}}
	go s.readLoop()
	return s, nil
}

func (s *cdpSession) readLoop() {
	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			s.closed = true
			for id, ch := range s.pending {
				close(ch)
				delete(s.pending, id)
			}
			s.mu.Unlock()
			return
		}
		var resp cdpResponse
		if json.Unmarshal(data, &resp) != nil || resp.ID == 0 {
			continue // an event, not a command reply
		}
		s.mu.Lock()
		if ch, ok := s.pending[resp.ID]; ok {
			ch <- resp
			delete(s.pending, resp.ID)
		}
		s.mu.Unlock()
	}
}

func (s *cdpSession) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed
}

func (s *cdpSession) call(method string, params map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("devtools session closed")
	}
	s.nextID++
	id := s.nextID
	ch := make(chan cdpResponse, 1)
	s.pending[id] = ch
	s.mu.Unlock()

	s.writeMu.Lock()
	err := s.conn.WriteJSON(map[string]any{"id": id, cdpKeyMethod: method, "params": params})
	s.writeMu.Unlock()
	if err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", method, err)
	}

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: devtools session closed", method)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(30 * time.Second):
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("%s: timed out", method)
	}
}

// evaluate runs expression in the page and returns its value by value.
func (s *cdpSession) evaluate(expression string) (any, error) {
	raw, err := s.call("Runtime.evaluate", map[string]any{
		cdpKeyExpression: expression,
		"returnByValue":  true,
		"awaitPromise":   true,
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse evaluate result: %w", err)
	}
	if out.ExceptionDetails != nil {
		msg := out.ExceptionDetails.Text
		if out.ExceptionDetails.Exception != nil && out.ExceptionDetails.Exception.Description != "" {
			msg = out.ExceptionDetails.Exception.Description
		}
		return nil, fmt.Errorf("evaluate: %s", msg)
	}
	return out.Result.Value, nil
}

// close drops every open devtools session. Called when the cached backend is replaced,
// so a superseded backend does not leave its websockets open.
func (c *cdpBackend) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.sessions {
		if s.conn != nil {
			s.conn.Close() //nolint:errcheck,gosec // dropping a dead session; the error is moot
		}
		delete(c.sessions, id)
	}
}

// healthy reports whether this backend still points at a live devtools socket.
// The socket name carries the app's pid, so a restart invalidates it and the
// caller must re-resolve rather than reuse a stale forward.
func (c *cdpBackend) healthy() bool {
	if _, err := c.targets(); err != nil {
		c.mu.Lock()
		for id, s := range c.sessions {
			if s.conn != nil {
				s.conn.Close() //nolint:errcheck,gosec // dropping a dead session; the error is moot
			}
			delete(c.sessions, id)
		}
		c.mu.Unlock()
		return false
	}
	return true
}

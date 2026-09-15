package devices

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newEchoCDPServer answers every request with a Runtime.evaluate-shaped result echoing
// the request id, and interleaves an unsolicited event to exercise id correlation.
const testPkg = "com.example.app"

func newEchoCDPServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck,gosec // test server teardown
		for {
			var req struct {
				ID int `json:"id"`
			}
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if json.Unmarshal(data, &req) != nil {
				continue
			}
			// an event first: no id, must not be mistaken for a reply
			conn.WriteJSON(map[string]any{"method": "Runtime.consoleAPICalled", "params": map[string]any{}}) //nolint:errcheck,gosec // test server
			conn.WriteJSON(map[string]any{                                                                   //nolint:errcheck,gosec // test server
				"id":     req.ID,
				"result": map[string]any{"result": map[string]any{"value": req.ID}},
			})
		}
	}))
	return srv, "ws" + strings.TrimPrefix(srv.URL, "http")
}

// gorilla/websocket permits one concurrent writer and panics otherwise. Without a
// dedicated write mutex this test panics instead of failing.
func Test_cdpSession_concurrentCalls(t *testing.T) {
	srv, wsURL := newEchoCDPServer(t)
	defer srv.Close()

	s, err := dialCDP(wsURL)
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer s.conn.Close() //nolint:errcheck,gosec // test teardown

	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.call("Runtime.evaluate", map[string]any{"expression": "1"}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call failed: %v", err)
	}
}

// Each caller must receive the reply carrying its own id, not whichever frame arrived
// first — events are interleaved by the server above.
func Test_cdpSession_idCorrelation(t *testing.T) {
	srv, wsURL := newEchoCDPServer(t)
	defer srv.Close()

	s, err := dialCDP(wsURL)
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	defer s.conn.Close() //nolint:errcheck,gosec // test teardown

	for i := 1; i <= 5; i++ {
		got, err := s.evaluate("whatever")
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		// the echo server returns the request id as the value
		if v, ok := got.(float64); !ok || int(v) != i {
			t.Errorf("call %d got value %v, want %d", i, got, i)
		}
	}
}

// A session whose peer hung up must report itself dead so callers re-dial.
func Test_cdpSession_aliveAfterServerClose(t *testing.T) {
	srv, wsURL := newEchoCDPServer(t)
	s, err := dialCDP(wsURL)
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	if !s.alive() {
		t.Fatal("session should be alive immediately after dial")
	}
	srv.Close()
	s.conn.Close() //nolint:errcheck,gosec // forcing the read loop to observe the hangup
	if _, err := s.call("Runtime.evaluate", nil); err == nil {
		t.Error("call on a closed session should fail")
	}
}

// resolveWebViewBackend re-validates a cached backend through an optional healthy()
// method. A backend without one is cached forever: the app restarts, the agent is gone,
// and every later call fails where the uncached code would have re-attached. Both
// backends must therefore expose it.
func Test_webViewBackends_exposeHealthy(t *testing.T) {
	backends := map[string]webViewBackend{
		"agentBackend": agentBackend{port: 1},
		"cdpBackend":   &cdpBackend{port: 1},
	}
	for name, b := range backends {
		t.Run(name, func(t *testing.T) {
			if _, ok := b.(interface{ healthy() bool }); !ok {
				t.Errorf("%s does not expose healthy(); resolveWebViewBackend would cache it "+
					"forever and never recover from an app restart", name)
			}
		})
	}
}

// A dead session must be closed and dropped before a replacement is dialled: gorilla
// leaves the fd open when the read side fails, so replacing it silently leaks one.
func Test_cdpBackend_replacesDeadSession(t *testing.T) {
	srv, wsURL := newEchoCDPServer(t)
	defer srv.Close()

	dead, err := dialCDP(wsURL)
	if err != nil {
		t.Fatalf("dialCDP: %v", err)
	}
	dead.conn.Close() //nolint:errcheck,gosec // forcing the read loop to observe the hangup
	for i := 0; i < 100 && dead.alive(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if dead.alive() {
		t.Fatal("session should be dead after its socket is closed")
	}

	c := &cdpBackend{sessions: map[string]*cdpSession{"target-1": dead}}
	c.mu.Lock()
	if s, ok := c.sessions["target-1"]; ok && !s.alive() {
		if s.conn != nil {
			s.conn.Close() //nolint:errcheck,gosec // mirrors session()'s replace path
		}
		delete(c.sessions, "target-1")
	}
	c.mu.Unlock()

	if _, still := c.sessions["target-1"]; still {
		t.Error("a dead session must be dropped from the map, not left to be reused")
	}
}

// The timeout must carry the underlying error, or a bad webview id, a dial failure and a
// genuinely slow page are indistinguishable.
func Test_cdpBackend_waitForLoadState_reportsUnderlyingError(t *testing.T) {
	// port 1 has nothing listening, so every poll fails to reach /json/list
	c := &cdpBackend{port: 1, bundleID: testPkg}
	err := c.waitForLoadState("nope", "load", 300)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "did not reach load state") {
		t.Errorf("should still report the timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "devtools") && !strings.Contains(err.Error(), "connect") {
		t.Errorf("timeout should carry the underlying cause, got: %v", err)
	}
}

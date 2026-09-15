package devices

import (
	"strings"
	"testing"
)

func Test_parseDevtoolsSocketName(t *testing.T) { //nolint:funlen
	tests := []struct {
		name  string
		input string
		pids  map[string]bool
		want  string
	}{
		{
			name:  "listening socket for a known pid",
			input: "0000000000000000: 00000002 00000000 00010000 0001 01 278848 @webview_devtools_remote_31691",
			pids:  map[string]bool{"31691": true},
			want:  "webview_devtools_remote_31691",
		},
		{
			name: "picks the socket whose pid we asked for",
			input: "0000000000000000: 00000002 00000000 00010000 0001 01 214101 @webview_devtools_remote_25791\n" +
				"0000000000000000: 00000002 00000000 00010000 0001 01 278848 @webview_devtools_remote_31691",
			pids: map[string]bool{"31691": true},
			want: "webview_devtools_remote_31691",
		},
		{
			name:  "ignores a socket belonging to another app",
			input: "0000000000000000: 00000002 00000000 00010000 0001 01 214101 @webview_devtools_remote_25791",
			pids:  map[string]bool{"31691": true},
			want:  "",
		},
		{
			name:  "ignores a non-listening entry left behind by a dead webview",
			input: "0000000000000000: 00000003 00000000 00000000 0001 03 36470 @webview_devtools_remote_31691",
			pids:  map[string]bool{"31691": true},
			want:  "",
		},
		{
			name:  "ignores a connected-but-not-listening state",
			input: "0000000000000000: 00000002 00000000 00010000 0001 03 278848 @webview_devtools_remote_31691",
			pids:  map[string]bool{"31691": true},
			want:  "",
		},
		{
			name:  "does not match a socket name that merely contains the pattern",
			input: "0000000000000000: 00000002 00000000 00010000 0001 01 278848 @webview_devtools_remote_31691_stale",
			pids:  map[string]bool{"31691": true},
			want:  "",
		},
		{
			name:  "ignores chrome's own devtools socket",
			input: "0000000000000000: 00000002 00000000 00010000 0001 01 249262 @chrome_devtools_remote",
			pids:  map[string]bool{"31691": true},
			want:  "",
		},
		{
			name:  "tolerates a truncated line",
			input: "0000000000000000: 00000002 00000000",
			pids:  map[string]bool{"31691": true},
			want:  "",
		},
		{
			name:  "no sockets at all",
			input: "",
			pids:  map[string]bool{"31691": true},
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseDevtoolsSocketName(tt.input, tt.pids); got != tt.want {
				t.Errorf("parseDevtoolsSocketName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func Test_buildEvaluateExpression(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		args       []any
		want       string
	}{
		{
			name:       "bare expression is wrapped in a returning function",
			expression: "document.title",
			want:       "(function(){\nreturn (document.title)\n})()",
		},
		{
			name:       "an explicit return is left alone",
			expression: "return document.title",
			want:       "(function(){\nreturn document.title\n})()",
		},
		{
			name:       "args are bound positionally",
			expression: "return a0 + a1",
			args:       []any{1, 2},
			want:       "(function(a0,a1){\nreturn a0 + a1\n}).apply(null,[1,2])",
		},
		{
			name:       "string args are json encoded",
			expression: "return a0",
			args:       []any{"hi"},
			want:       "(function(a0){\nreturn a0\n}).apply(null,[\"hi\"])",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildEvaluateExpression(tt.expression, tt.args)
			if err != nil {
				t.Fatalf("buildEvaluateExpression() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("buildEvaluateExpression() = %q, want %q", got, tt.want)
			}
		})
	}
}

const (
	testSerialA = "emulator-5554"
	testSerialB = "emulator-5556"
)

func Test_findForwardPort(t *testing.T) { //nolint:funlen
	// `adb forward --list` is global: it lists every device regardless of -s.
	const list = testSerialA + " tcp:41234 localabstract:webview_devtools_remote_12345\n" +
		testSerialA + " tcp:41999 localabstract:mobilecli.com.example.app\n" +
		testSerialB + " tcp:51284 localabstract:webview_devtools_remote_3572\n"

	tests := []struct {
		name   string
		list   string
		serial string
		target string
		want   int
	}{
		{
			name: "exact serial and target", list: list, serial: testSerialA,
			target: "localabstract:webview_devtools_remote_12345", want: 41234,
		},
		{
			// the socket name ends in a pid, so a substring match would hand back the
			// forward for pid 12345 when asked for pid 1234
			name: "a shorter pid must not match a longer one", list: list, serial: testSerialA,
			target: "localabstract:webview_devtools_remote_1234", want: 0,
		},
		{
			// the decisive one: same pid, another device. Reusing this port would drive
			// the other device's webview with no error anywhere.
			name: "same target on a different device must not match", list: list, serial: testSerialA,
			target: "localabstract:webview_devtools_remote_3572", want: 0,
		},
		{
			name: "that forward is found for its own device", list: list, serial: testSerialB,
			target: "localabstract:webview_devtools_remote_3572", want: 51284,
		},
		{
			name: "agent socket matches exactly", list: list, serial: testSerialA,
			target: "localabstract:mobilecli.com.example.app", want: 41999,
		},
		{
			name: "package prefix must not match", list: list, serial: testSerialA,
			target: "localabstract:mobilecli.com.example", want: 0,
		},
		{name: "empty list", list: "", serial: testSerialA, target: "localabstract:x", want: 0},
		{
			name: "CRLF line endings", list: testSerialA + " tcp:41234 localabstract:sock\r\n",
			serial: testSerialA, target: "localabstract:sock", want: 41234,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := findForwardPort(tt.list, tt.serial, tt.target); got != tt.want {
				t.Errorf("findForwardPort() = %d, want %d", got, tt.want)
			}
		})
	}
}

// A body ending in a line comment must not swallow the wrapper's closing brace. The
// agent path is immune because it builds the body with new Function(<json string>).
func Test_buildEvaluateExpression_trailingComment(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		args       []any
	}{
		{name: "no args", expression: "return document.title // the page title"},
		{name: "with args", expression: "return a0 // first arg", args: []any{1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := buildEvaluateExpression(tt.expression, tt.args)
			if err != nil {
				t.Fatalf("buildEvaluateExpression() error = %v", err)
			}
			// the comment must be terminated before the closing brace
			if !strings.Contains(got, "\n})") && !strings.Contains(got, "\n}).apply") {
				t.Errorf("closing brace is not fenced off from the trailing comment: %q", got)
			}
		})
	}
}

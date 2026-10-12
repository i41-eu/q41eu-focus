package app

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/i41-eu/q41eu-focus/internal/timer"
	"github.com/i41-eu/q41eu-raw/statestore"
	"github.com/i41-eu/q41eu-raw/wire"
)

const token = "test-robot-token"

// clock is a test clock.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func newApp(t *testing.T, st statestore.Store) (*App, *httptest.Server, *clock) {
	t.Helper()
	clk := &clock{now: time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)}
	a := New(Config{RobotToken: token, PublicURL: "http://focus.test", State: st, Location: time.UTC, Now: clk.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ts := httptest.NewServer(a.Handler())
	t.Cleanup(ts.Close)
	return a, ts, clk
}

// fakeRobot connects as a robot and records what it gets.
type fakeRobot struct {
	t      *testing.T
	ws     *websocket.Conn
	mu     sync.Mutex
	frames []wire.Frame
	bins   map[byte]int
}

func connect(t *testing.T, ts *httptest.Server, id string) *fakeRobot {
	t.Helper()
	h := http.Header{"Authorization": {"Bearer " + token}, wire.DeviceIDHeader: {id}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+wire.ConnectPath, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	f, _ := wire.Marshal(wire.KindRegister, wire.Meta{WorkerID: id}, wire.RegisterBody{Class: wire.ClassRobot,
		Capabilities: wire.RobotCapabilities{Model: "test", Commands: []string{"image", "leds", "speaker", "nod", "look"}}})
	ws.WriteMessage(websocket.TextMessage, f)
	r := &fakeRobot{t: t, ws: ws, bins: map[byte]int{}}
	go func() {
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			r.mu.Lock()
			if kind == websocket.BinaryMessage {
				r.bins[data[0]]++
			} else if fs, err := wire.Parse(data); err == nil {
				r.frames = append(r.frames, fs...)
			}
			r.mu.Unlock()
		}
	}()
	return r
}

func (r *fakeRobot) event(name string) {
	f, _ := wire.Marshal(wire.KindRobotEvent, wire.Meta{}, wire.RobotEventBody{Name: name})
	r.ws.WriteMessage(websocket.TextMessage, f)
}

// commands lists the robot commands it got, in order.
func (r *fakeRobot) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, f := range r.frames {
		if f.Kind == wire.KindRobotCommand {
			var c wire.RobotCommandBody
			f.Decode(&c)
			out = append(out, c.Command)
		}
	}
	return out
}

func (r *fakeRobot) count(kind byte) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bins[kind]
}

func (r *fakeRobot) pairCode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	code := ""
	for _, f := range r.frames {
		if f.Kind == wire.KindPairCode {
			var b wire.PairCodeBody
			f.Decode(&b)
			code = b.Code
		}
	}
	return code
}

func (r *fakeRobot) lastPaired() (wire.PairedBody, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var p wire.PairedBody
	ok := false
	for _, f := range r.frames {
		if f.Kind == wire.KindPaired {
			f.Decode(&p)
			ok = true
		}
	}
	return p, ok
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

// browser is a paired browser.
type browser struct {
	t  *testing.T
	ts *httptest.Server
	c  *http.Client
}

func newBrowser(t *testing.T, ts *httptest.Server) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, ts: ts, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) get(path string) (int, map[string]any) {
	b.t.Helper()
	resp, err := b.c.Get(b.ts.URL + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func (b *browser) post(path, body string) (int, map[string]any) {
	b.t.Helper()
	req, _ := http.NewRequest("POST", b.ts.URL+path, strings.NewReader(body))
	req.Header.Set("Origin", b.ts.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var v map[string]any
	json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v
}

func TestRobotAloneShowsTheTimer(t *testing.T) {
	a, ts, clk := newApp(t, nil)
	r := connect(t, ts, "stackchan-0a1b2c3d4e50")
	// No browser paired: the robot still hears one, so it shows the timer, not its QR screen.
	eventually(t, "paired and a picture", func() bool { p, ok := r.lastPaired(); return ok && p.Viewers >= 1 && r.count(wire.BinShowJPEG) >= 1 })
	eventually(t, "the LEDs", func() bool { return strings.Contains(strings.Join(r.commands(), ","), "leds") })

	// A tap starts, a second tap within the touch gap is the same touch, a later one pauses.
	r.event("screen_tap")
	eventually(t, "running", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.robots["stackchan-0a1b2c3d4e50"].timer.Status == "running"
	})
	r.event("head_press")
	time.Sleep(50 * time.Millisecond)
	a.mu.Lock()
	if s := a.robots["stackchan-0a1b2c3d4e50"].timer.Status; s != "running" {
		t.Fatalf("one touch reported twice paused it: %s", s)
	}
	a.mu.Unlock()
	clk.add(2 * time.Second)
	r.event("head_press")
	eventually(t, "paused", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.robots["stackchan-0a1b2c3d4e50"].timer.Status == "paused"
	})

	// Resume, then the focus period runs out: a chime and a nod; a break waits (no auto start).
	clk.add(2 * time.Second)
	r.event("screen_tap")
	eventually(t, "running again", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.robots["stackchan-0a1b2c3d4e50"].timer.Status == "running"
	})
	before := r.count(wire.BinSpeakerPCM)
	clk.add(26 * time.Minute)
	a.step(clk.Now())
	eventually(t, "the chime and the nod", func() bool {
		return r.count(wire.BinSpeakerPCM) > before && strings.Contains(strings.Join(r.commands(), ","), "nod")
	})
	a.mu.Lock()
	tm := a.robots["stackchan-0a1b2c3d4e50"].timer
	if tm.Phase != "short" || tm.Status != "idle" || len(tm.History) != 1 {
		t.Fatalf("after the focus period: %+v", tm)
	}
	a.mu.Unlock()

	// A long press skips the break.
	r.event("screen_long_press")
	eventually(t, "skipped", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.robots["stackchan-0a1b2c3d4e50"].timer.Phase == "focus"
	})
}

func TestBrowserPairsAndRunsIt(t *testing.T) {
	_, ts, _ := newApp(t, nil)
	r := connect(t, ts, "stackchan-0a1b2c3d4e51")
	eventually(t, "a pairing code", func() bool { return r.pairCode() != "" })
	b := newBrowser(t, ts)
	if code, _ := b.get("/api/state"); code != http.StatusNotFound {
		t.Fatalf("not paired yet: %d", code)
	}
	if code, _ := b.post("/api/action", `{"action":"start"}`); code != http.StatusNotFound {
		t.Fatalf("an action unpaired: %d", code)
	}
	resp, _ := b.c.Get(ts.URL + "/pair?code=" + r.pairCode())
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("pair: %d", resp.StatusCode)
	}
	code, v := b.get("/api/state")
	if code != 200 || v["robot"] != "stackchan-0a1b2c3d4e51" || v["status"] != "idle" || v["online"] != true {
		t.Fatalf("state: %d %v", code, v)
	}
	if _, v = b.post("/api/action", `{"action":"task","arg":"write the report"}`); v["task"] != "write the report" {
		t.Fatalf("task: %v", v)
	}
	if _, v = b.post("/api/action", `{"action":"start"}`); v["status"] != "running" || v["last_event"] != "started" {
		t.Fatalf("start: %v", v)
	}
	if _, v = b.post("/api/action", `{"action":"interrupt","arg":"external"}`); v["status"] != "running" {
		t.Fatalf("an interruption stops nothing: %v", v)
	}
	stats := v["stats"].(map[string]any)["today"].(map[string]any)
	if stats["external"] != 1.0 {
		t.Fatalf("interruption counted: %v", stats)
	}
	if code, _ = b.post("/api/action", `{"action":"explode"}`); code != http.StatusBadRequest {
		t.Fatalf("an unknown action: %d", code)
	}
	code, v = b.post("/api/settings", `{"settings":{"focus_min":50,"short_min":10,"long_min":20,"long_every":3,"sound":false}}`)
	if code != 200 || v["settings"].(map[string]any)["focus_min"] != 50.0 {
		t.Fatalf("settings: %d %v", code, v)
	}
	// Another site's page cannot act.
	req, _ := http.NewRequest("POST", ts.URL+"/api/action", strings.NewReader(`{"action":"reset"}`))
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := b.c.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", resp.StatusCode)
	}
	// An expired or wrong code pairs nothing.
	b2 := newBrowser(t, ts)
	if resp, _ := b2.c.Get(ts.URL + "/pair?code=AAAAAAAA"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a wrong code: %d", resp.StatusCode)
	}
}

func TestStateIsKept(t *testing.T) {
	st := &statestore.File{Path: t.TempDir() + "/state.json"}
	a, ts, clk := newApp(t, st)
	r := connect(t, ts, "stackchan-0a1b2c3d4e52")
	eventually(t, "a pairing code", func() bool { return r.pairCode() != "" })
	b := newBrowser(t, ts)
	b.c.Get(ts.URL + "/pair?code=" + r.pairCode())
	b.post("/api/action", `{"action":"start"}`)
	clk.add(25 * time.Minute)
	a.step(clk.Now())
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	a2 := New(Config{RobotToken: token, PublicURL: "http://focus.test", State: st, Location: time.UTC, Now: clk.Now,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	rb := a2.robots["stackchan-0a1b2c3d4e52"]
	if rb == nil || len(rb.timer.History) != 1 || rb.timer.Phase != "short" {
		t.Fatalf("after a restart: %+v", rb)
	}
	if n := len(a2.sessions); n != 1 {
		t.Fatalf("sessions after a restart: %d", n)
	}
}

func TestUnknownRobotRefused(t *testing.T) {
	_, ts, _ := newApp(t, nil)
	h := http.Header{"Authorization": {"Bearer wrong"}, wire.DeviceIDHeader: {"stackchan-0a1b2c3d4e53"}}
	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+wire.ConnectPath, h)
	if err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a wrong token: %v %v", err, resp)
	}
}

func TestDisplayOnTheRobot(t *testing.T) {
	const id = "stackchan-0a1b2c3d4e54"
	a, ts, clk := newApp(t, nil)
	r := connect(t, ts, id)
	eventually(t, "a pairing code", func() bool { return r.pairCode() != "" })
	b := newBrowser(t, ts)
	b.c.Get(ts.URL + "/pair?code=" + r.pairCode())
	if _, v := b.get("/api/state"); v["settings"].(map[string]any)["display"] != "calm" {
		t.Fatalf("a new robot is calm: %v", v["settings"])
	}
	b.post("/api/action", `{"action":"start"}`)

	// sync waits until the robot got all the app sent it so far (a frame queued after them came
	// back) and returns how many pictures it got.
	marker := 1000
	sync := func() int {
		marker++
		a.mu.Lock()
		a.robots[id].conn.Frame(wire.KindPaired, wire.PairedBody{Viewers: marker})
		a.mu.Unlock()
		eventually(t, "the robot got all", func() bool { p, _ := r.lastPaired(); return p.Viewers == marker })
		return r.count(wire.BinShowJPEG)
	}
	// pictures is how many pictures the robot gets in the next seconds.
	pictures := func(seconds int) int {
		before := sync()
		for range seconds {
			clk.add(time.Second)
			a.step(clk.Now())
		}
		return sync() - before
	}
	choose := func(display string) any {
		code, v := b.post("/api/settings", fmt.Sprintf(`{"settings":{"focus_min":25,"short_min":5,"long_min":15,"long_every":4,`+
			`"daily_goal":8,"sound":true,"lights":true,"moves":true,"display":%q}}`, display))
		if code != 200 {
			t.Fatalf("display %s: %d", display, code)
		}
		return v["settings"].(map[string]any)["display"]
	}
	show := func(display string) { // the robot gets a new picture right away
		before := sync()
		if got := choose(display); got != display {
			t.Fatalf("display %s: %v", display, got)
		}
		if n := sync() - before; n != 1 {
			t.Fatalf("display %s: %d pictures right away, want 1", display, n)
		}
	}

	if n := pictures(60); n != 0 { // 25:00 to 24:00 left: the same sixth
		t.Errorf("calm: %d pictures in a minute, want none", n)
	}
	show("seconds")
	if n := pictures(30); n != 30 {
		t.Errorf("seconds: %d pictures in 30 seconds", n)
	}
	show("minutes")
	if n := pictures(120); n != 2 {
		t.Errorf("minutes: %d pictures in 2 minutes", n)
	}
	show("calm") // 21:30 left: the next sixth starts at 20:50
	if n := pictures(30); n != 0 {
		t.Errorf("calm again: %d pictures in 30 seconds", n)
	}
	if n := pictures(20); n != 1 {
		t.Errorf("calm, a sixth over: %d pictures", n)
	}
	if got := choose("fancy"); got != "calm" {
		t.Errorf("an unknown display: %v, want calm", got)
	}
}

func TestDisplayCalmForExistingUsers(t *testing.T) {
	// A state saved before the display setting existed: its timers have none, and are calm.
	const id = "stackchan-0a1b2c3d4e55"
	path := t.TempDir() + "/state.json"
	if err := os.WriteFile(path, []byte(`{"robots":{"`+id+`":{"timer":{"settings":{"focus_min":50,"short_min":10,"long_min":20,`+
		`"long_every":3,"daily_goal":8,"sound":true,"lights":true,"moves":true},"phase":"focus","status":"idle","left":3000000000000}}},"sessions":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a, _, _ := newApp(t, &statestore.File{Path: path})
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.robots[id].timer.Settings; s.Display != timer.Calm || s.FocusMin != 50 {
		t.Errorf("an existing timer: %+v", s)
	}
	if v := a.view(a.robots[id], ""); v.Settings.Display != timer.Calm {
		t.Errorf("the page gets %q", v.Settings.Display)
	}
	if d := a.robotFor("stackchan-0a1b2c3d4e56").timer.Settings.Display; d != timer.Calm {
		t.Errorf("a new robot: %q", d)
	}
}

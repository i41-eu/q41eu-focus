package app

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/i41-eu/q41eu-focus/internal/timer"
	"github.com/i41-eu/q41eu-raw/wire"
)

//go:embed ui/index.html
var uiFS embed.FS

// handleIndex serves the page (from disk with Config.UIDir, for development).
func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	a.sessionID(w, r) // the cookie before the page's first request
	var b []byte
	var err error
	if a.cfg.UIDir != "" {
		b, err = os.ReadFile(filepath.Join(a.cfg.UIDir, "index.html"))
	} else {
		b, err = fs.ReadFile(uiFS, "ui/index.html")
	}
	if err != nil {
		http.Error(w, "page missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(b)
}

/* ----------------------------------- view --------------------------------- */

// stateView is what the page shows.
type stateView struct {
	Robot     string         `json:"robot"`
	Online    bool           `json:"online"`
	Phase     timer.Phase    `json:"phase"`
	Status    timer.Status   `json:"status"`
	LeftMs    int64          `json:"left_ms"` // at ServerMs
	FullMs    int64          `json:"full_ms"`
	ServerMs  int64          `json:"server_ms"`
	Round     int            `json:"round"`
	Task      string         `json:"task"`
	Settings  timer.Settings `json:"settings"`
	Stats     timer.Stats    `json:"stats"`
	Robots    []string       `json:"robots"` // the session's robots
	SignIn    bool           `json:"sign_in"`
	SignedIn  string         `json:"signed_in,omitempty"` // the account's name
	Paired    int            `json:"paired"`              // browsers paired with this robot
	Watching  int            `json:"watching"`
	Speaker   bool           `json:"speaker"` // the robot can play the chimes
	LastEvent string         `json:"last_event,omitempty"`
}

// view is the robot's timer for the page. a.mu held.
func (a *App) view(r *robot, sid string) stateView {
	now := a.cfg.Now()
	t := r.timer
	v := stateView{Robot: r.id, Online: r.conn != nil, Phase: t.Phase, Status: t.Status, LeftMs: t.Remaining(now).Milliseconds(),
		FullMs: t.Settings.Length(t.Phase).Milliseconds(), ServerMs: now.UnixMilli(), Round: t.Round, Task: t.Task,
		Settings: t.Settings, Stats: t.Stats(now, a.cfg.Location), SignIn: a.cfg.SignIn != nil,
		Speaker: slices.Contains(r.commands, "speaker")}
	p := a.paired(r.id)
	v.Paired, v.Watching = 0, p.Watching
	for _, s := range a.sessions {
		if slices.Contains(s.Robots, r.id) {
			v.Paired++
		}
	}
	if s := a.sessions[sid]; s != nil {
		v.Robots = s.Robots
		if s.account != nil {
			v.SignedIn = s.account.Name
		}
	}
	return v
}

/* ----------------------------------- SSE ---------------------------------- */

type subscriber struct {
	sid    string
	robot  string // "" = the session's most recent
	events chan []byte
}

// publishState sends the robot's state to the browsers paired with it. a.mu held.
func (a *App) publishState(r *robot) {
	for sub := range a.subs {
		s := a.sessions[sub.sid]
		if s == nil || !slices.Contains(s.Robots, r.id) {
			continue
		}
		if want := sub.robot; want != "" && want != r.id || want == "" && s.Robots[0] != r.id {
			continue
		}
		select {
		case sub.events <- mustJSON(a.view(r, sub.sid)):
		default: // a slow browser gets the next state
		}
	}
}

// handleEvents streams "state" events of the session's robot (?robot= one of its robots).
func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	sid := a.sessionID(w, r)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	sub := &subscriber{sid: sid, robot: r.URL.Query().Get("robot"), events: make(chan []byte, 16)}
	a.mu.Lock()
	a.subs[sub] = struct{}{}
	a.tellWatching(sid)
	var first []byte
	if rb := a.pairedRobot(sid, sub.robot); rb != nil {
		first = mustJSON(a.view(rb, sid))
	}
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.subs, sub)
		a.tellWatching(sid)
		a.mu.Unlock()
	}()
	send := func(data []byte) bool {
		if _, err := fmt.Fprintf(w, "event: state\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if first != nil && !send(first) {
		return
	}
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case data := <-sub.events:
			if !send(data) {
				return
			}
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

/* ---------------------------------- pairing ------------------------------- */

// handlePair is the QR target: the one-time code proves the browser can see the robot.
func (a *App) handlePair(w http.ResponseWriter, r *http.Request) {
	sid := a.sessionID(w, r)
	code := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(r.URL.Query().Get("code")))
	a.mu.Lock()
	rb, ok := a.redeem(sid, code)
	if ok {
		if c := rb.conn; c != nil {
			c.Frame(wire.KindPaired, a.paired(rb.id))
		}
		a.publishState(rb)
	}
	a.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1">`+
			`<title>Focus</title><body style="font-family:system-ui;padding:24px;font-size:18px">`+
			`<p>This code has expired. Scan the QR code on the robot again (swipe up on the robot, then QR).</p>`)
		return
	}
	a.log.Info("browser paired", "robot", rb.id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

/* ---------------------------------- the api ------------------------------- */

// sameOrigin: a browser's POST from this site's own page (no cross-site requests).
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	return strings.TrimPrefix(strings.TrimPrefix(o, "https://"), "http://") == r.Host
}

func (a *App) handleState(w http.ResponseWriter, r *http.Request) {
	sid := a.sessionID(w, r)
	a.mu.Lock()
	defer a.mu.Unlock()
	rb := a.pairedRobot(sid, r.URL.Query().Get("robot"))
	if rb == nil {
		v := map[string]any{"error": "not_paired", "sign_in": a.cfg.SignIn != nil}
		if s := a.sessions[sid]; s != nil && s.account != nil {
			v["signed_in"] = s.account.Name
		}
		writeJSON(w, http.StatusNotFound, v)
		return
	}
	writeJSON(w, http.StatusOK, a.view(rb, sid))
}

// handleAction: {"robot", "action": start|pause|toggle|skip|reset|interrupt|task, "arg"}.
func (a *App) handleAction(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	sid := a.sessionID(w, r)
	var req struct{ Robot, Action, Arg string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil {
		http.Error(w, "body must be JSON", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rb := a.pairedRobot(sid, req.Robot)
	if rb == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_paired"})
		return
	}
	ev, ok := a.actLocked(rb, req.Action, req.Arg, a.cfg.Now())
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown action"})
		return
	}
	v := a.view(rb, sid)
	v.LastEvent = string(ev)
	writeJSON(w, http.StatusOK, v)
}

// handleSettings: {"robot", "settings": timer.Settings}.
func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	sid := a.sessionID(w, r)
	var req struct {
		Robot    string         `json:"robot"`
		Settings timer.Settings `json:"settings"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil {
		http.Error(w, "body must be JSON", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	rb := a.pairedRobot(sid, req.Robot)
	if rb == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_paired"})
		return
	}
	rb.timer.SetSettings(req.Settings)
	a.dirty = true
	a.showLocked(rb, a.cfg.Now())
	a.publishState(rb)
	writeJSON(w, http.StatusOK, a.view(rb, sid))
}

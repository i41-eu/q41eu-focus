// Package app is the focus timer server: robots connect over the Embody Mode protocol
// (github.com/i41-eu/q41eu-raw/wire); each robot has one timer, run on the robot itself (its
// screen, LEDs, a touch) and from a page on a phone or computer paired by the robot's QR code.
// The timer works without any page: the robot shows it from the start.
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/i41-eu/q41eu-dev/robothub"
	"github.com/i41-eu/q41eu-focus/internal/timer"
	"github.com/i41-eu/q41eu-raw/robotauth"
	"github.com/i41-eu/q41eu-raw/sso"
	"github.com/i41-eu/q41eu-raw/statestore"
	"github.com/i41-eu/q41eu-raw/wire"
)

// Config configures an App.
type Config struct {
	RobotToken   string            // the shared bearer token of the owner's robots
	Manager      *robotauth.Client // robots set up by a Stackchan manager, with a token of their own; nil = none
	SignIn       *sso.Client       // sign-in through that manager: the owner's browsers get their robots without the QR code
	SignInAlways bool              // try a silent sign-in without the manager's hint cookie (sso.SilentTry): this app on another domain than the manager's -sign-in-hint-domain
	PublicURL    string            // base URL browsers use
	PairTTL      time.Duration     // lifetime of a pairing code
	State        statestore.Store  // where timers and pairings are kept; nil = in memory only
	UIDir        string            // development: serve the page from this directory
	Location     *time.Location    // the time zone of the statistics' days; default time.Local
	Log          *slog.Logger
	Now          func() time.Time // tests; default time.Now
}

// App holds all state. One mutex guards everything; robot sockets only queue messages.
type App struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	robots   map[string]*robot
	hub      *robothub.Hub
	sessions map[string]*session
	subs     map[*subscriber]struct{}
	dirty    bool
	saveMu   sync.Mutex
}

// session is one browser: its paired robots, the most recent first.
type session struct {
	Robots  []string
	account *sso.Account // signed in through the manager (signin.go)
	handle  string
	meta    sessionMeta // device, last seen, paired since (pairings.go)
}

// robot is one robot and its timer; it stays after the robot disconnects.
type robot struct {
	id    string
	timer *timer.Timer
	owner string // the owner's account key, from the manager; "" = the shared token

	conn     *robothub.Robot // nil while offline
	lastSeen time.Time
	commands []string

	// What the robot shows now (robot.go), so only changes are sent.
	shownFrame string // screen.Key of the picture
	shownLEDs  string
	lastTouch  time.Time // the last start/pause from the robot: one per touch
}

func New(cfg Config) *App {
	if cfg.PairTTL <= 0 {
		cfg.PairTTL = 5 * time.Minute
	}
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	a := &App{cfg: cfg, log: cfg.Log, robots: map[string]*robot{},
		sessions: map[string]*session{}, subs: map[*subscriber]struct{}{}}
	a.hub = a.newHub()
	if err := a.load(); err != nil {
		a.log.Error("state not loaded: starting empty", "err", err)
	}
	return a
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+wire.ConnectPath, a.hub)
	mux.HandleFunc("GET /{$}", a.trySignIn(a.handleIndex))
	mux.HandleFunc("GET /pair", a.trySignIn(a.handlePair))
	mux.HandleFunc("GET /auth/login", a.handleLogin)
	mux.HandleFunc("GET /auth/sso", a.handleSSOReturn)
	mux.HandleFunc("POST /auth/logout", a.handleLogout)
	mux.HandleFunc("GET /api/state", a.handleState)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("POST /api/action", a.handleAction)
	mux.HandleFunc("POST /api/settings", a.handleSettings)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	return mux
}

// robotFor returns the robot, creating it with a new timer on first sight. a.mu held.
func (a *App) robotFor(id string) *robot {
	r := a.robots[id]
	if r == nil {
		r = &robot{id: id, timer: timer.New(timer.DefaultSettings())}
		a.robots[id] = r
		a.dirty = true
	}
	return r
}

/* --------------------------------- pairing -------------------------------- */

// redeem pairs the session with the code's robot (first in its list); the hub gives the robot a
// fresh code for the next browser. a.mu held.
func (a *App) redeem(sid, code string) (*robot, bool) {
	id, ok := a.hub.Redeem(code)
	if !ok {
		return nil, false
	}
	s := a.sessionFor(sid)
	s.Robots = append([]string{id}, slices.DeleteFunc(s.Robots, func(x string) bool { return x == id })...)
	a.pairedNow(s, id)
	a.dirty = true
	return a.robotFor(id), true
}

// paired is what the robot hears about browsers. The timer needs none, so the robot always
// hears at least one: it shows the timer, not its QR screen (the QR stays a swipe away).
// a.mu held.
func (a *App) paired(robotID string) wire.PairedBody {
	viewers, seen := 0, map[string]bool{}
	for _, s := range a.sessions {
		if slices.Contains(s.Robots, robotID) {
			viewers++
		}
	}
	for sub := range a.subs {
		if s := a.sessions[sub.sid]; s != nil && slices.Contains(s.Robots, robotID) {
			seen[sub.sid] = true
		}
	}
	return wire.PairedBody{Viewers: max(1, viewers), Watching: len(seen), Reconnect: true}
}

// tellWatching sends the session's robots how many watch now. a.mu held.
func (a *App) tellWatching(sid string) {
	if s := a.sessions[sid]; s != nil {
		for _, id := range s.Robots {
			if rb := a.robots[id]; rb != nil && rb.conn != nil {
				rb.conn.Frame(wire.KindPaired, a.paired(id))
			}
		}
	}
}

/* -------------------------------- sessions -------------------------------- */

const sessionCookie = "focus_session"

// sessionID returns the browser's session id, setting a new cookie if needed.
func (a *App) sessionID(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && validSessionID(c.Value) {
		a.seen(c.Value, r)
		return c.Value
	}
	b := make([]byte, 32)
	rand.Read(b)
	id := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", MaxAge: int((365 * 24 * time.Hour) / time.Second),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil || strings.HasPrefix(a.cfg.PublicURL, "https://")})
	return id
}

func validSessionID(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// sessionFor returns the session, creating it. a.mu held.
func (a *App) sessionFor(sid string) *session {
	s := a.sessions[sid]
	if s == nil {
		s = &session{}
		a.sessions[sid] = s
	}
	return s
}

// pairedRobot is the session's robot: want if paired, else the most recent. a.mu held.
func (a *App) pairedRobot(sid, want string) *robot {
	s := a.sessions[sid]
	if s == nil {
		return nil
	}
	for _, id := range s.Robots {
		if want == "" || id == want {
			return a.robots[id]
		}
	}
	return nil
}

/* ---------------------------------- state --------------------------------- */

// stateDoc is what is saved: timers, owners and pairings, not live connections.
type stateDoc struct {
	Robots   map[string]savedRobot   `json:"robots"`
	Sessions map[string]savedSession `json:"sessions"`
}

type savedRobot struct {
	Timer *timer.Timer `json:"timer"`
	Owner string       `json:"owner,omitempty"`
}

type savedSession struct {
	Robots  []string     `json:"robots"`
	Meta    sessionMeta  `json:"meta"`
	Account *sso.Account `json:"account,omitempty"`
	Handle  string       `json:"handle,omitempty"`
}

func (a *App) load() error {
	if a.cfg.State == nil {
		return nil
	}
	b, err := a.cfg.State.Load(context.Background())
	if err != nil || b == nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var doc stateDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	for id, sr := range doc.Robots {
		r := &robot{id: id, timer: sr.Timer, owner: sr.Owner}
		if r.timer == nil {
			r.timer = timer.New(timer.DefaultSettings())
		}
		r.timer.Settings = r.timer.Settings.Clean()
		a.robots[id] = r
	}
	for sid, ss := range doc.Sessions {
		a.sessions[sid] = &session{Robots: ss.Robots, meta: ss.Meta, account: ss.Account, handle: ss.Handle}
	}
	return nil
}

// Save writes the state when it changed.
func (a *App) Save() error {
	if a.cfg.State == nil {
		return nil
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	a.mu.Lock()
	if !a.dirty {
		a.mu.Unlock()
		return nil
	}
	doc := stateDoc{Robots: map[string]savedRobot{}, Sessions: map[string]savedSession{}}
	for id, r := range a.robots {
		t := *r.timer
		doc.Robots[id] = savedRobot{Timer: &t, Owner: r.owner}
	}
	for sid, s := range a.sessions {
		if len(s.Robots) > 0 || s.account != nil {
			doc.Sessions[sid] = savedSession{Robots: s.Robots, Meta: s.meta, Account: s.account, Handle: s.handle}
		}
	}
	a.dirty = false
	a.mu.Unlock()
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	if err := a.cfg.State.Save(context.Background(), b); err != nil {
		a.mu.Lock()
		a.dirty = true
		a.mu.Unlock()
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

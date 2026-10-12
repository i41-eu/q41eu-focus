package app

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/i41-eu/q41eu-dev/robothub"
	"github.com/i41-eu/q41eu-dev/sound"
	"github.com/i41-eu/q41eu-focus/internal/screen"
	"github.com/i41-eu/q41eu-focus/internal/timer"
	"github.com/i41-eu/q41eu-raw/wire"
)

const touchGap = 1500 * time.Millisecond // one start/pause per touch, however it is reported

// newHub: robots connect through the dev kit's hub (github.com/i41-eu/q41eu-dev/robothub), which
// checks their tokens, keeps their connections and pairing codes; the timer is the app's.
func (a *App) newHub() *robothub.Hub {
	return robothub.New(robothub.Config{
		Token: a.cfg.RobotToken, Manager: a.cfg.Manager, PublicURL: a.cfg.PublicURL, PairTTL: a.cfg.PairTTL, Log: a.log,
		Connected:    a.attach,
		Disconnected: a.detach,
		Event:        func(c *robothub.Robot, ev wire.RobotEventBody) { a.robotEvent(c.ID(), ev) },
		// The timer needs no browser: the robot always hears of one, so it shows the timer, not its QR screen.
		Paired: func(id string) wire.PairedBody {
			a.mu.Lock()
			defer a.mu.Unlock()
			return a.paired(id)
		},
		Owner: func(id, owner string) {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.setOwnerLocked(id, owner)
		},
	})
}

// attach makes c the robot's connection and puts the timer on its screen.
func (a *App) attach(c *robothub.Robot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.robotFor(c.ID())
	r.conn, r.commands, r.lastSeen = c, c.Capabilities().Commands, a.cfg.Now()
	r.shownFrame, r.shownLEDs = "", ""
	a.showLocked(r, a.cfg.Now())
	a.publishState(r)
	go a.reportSeen(context.Background(), c.ID())
}

func (a *App) detach(c *robothub.Robot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.robots[c.ID()]; r != nil && r.conn == c {
		r.conn = nil
		a.publishState(r)
	}
}

// robotEvent: a tap on the screen or a touch on the head starts or pauses; a long press on the
// screen skips to the next phase.
func (a *App) robotEvent(id string, ev wire.RobotEventBody) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.robots[id]
	if r == nil {
		return
	}
	now := a.cfg.Now()
	r.lastSeen = now
	switch ev.Name {
	case "screen_tap", "head_press":
		if now.Sub(r.lastTouch) < touchGap {
			return
		}
		r.lastTouch = now
		a.actLocked(r, "toggle", "", now)
	case "screen_long_press":
		r.lastTouch = now
		a.actLocked(r, "skip", "", now)
	}
}

// actLocked runs an action from the robot or a page. a.mu held.
func (a *App) actLocked(r *robot, action, arg string, now time.Time) (timer.Event, bool) {
	t := r.timer
	var ev timer.Event
	switch action {
	case "start":
		ev = t.Start(now)
	case "pause":
		ev = t.Pause(now)
	case "toggle":
		ev = t.Toggle(now)
	case "skip":
		ev = t.Skip(now)
	case "reset":
		ev = t.ResetAll(now)
	case "interrupt":
		if !t.Interrupt(now, arg) {
			return "", false
		}
		ev = "interrupted"
	case "task":
		task := strings.TrimSpace(arg)
		if len([]rune(task)) > 80 {
			task = string([]rune(task)[:80])
		}
		t.Task = task
		ev = "task"
	default:
		return "", false
	}
	if ev != "" {
		a.dirty = true
		a.reactLocked(r, ev)
		a.showLocked(r, now)
		a.publishState(r)
	}
	return ev, true
}

// reactLocked is the robot's answer to what happened: a sound, a nod, a look up. a.mu held.
func (a *App) reactLocked(r *robot, ev timer.Event) {
	c, s := r.conn, r.timer.Settings
	if c == nil {
		return
	}
	play := func(name string) {
		if s.Sound && slices.Contains(r.commands, "speaker") {
			c.Binary(wire.BinSpeakerPCM, sound.Message(sound.Chime(name)))
		}
	}
	switch ev {
	case timer.Started, timer.Resumed:
		play("start")
	case timer.Completed: // a break now: a bell and a nod
		play("done")
		if s.Moves {
			c.Command("nod", nil)
		}
	case timer.BreakOver: // back to work: two notes, a look up at the person
		play("break_over")
		if s.Moves {
			c.Command("look", map[string]any{"yaw": 0, "pitch": 30})
		}
	}
}

// screenView is the timer as the robot shows it. a.mu held.
func (a *App) screenView(r *robot, now time.Time) screen.View {
	t := r.timer
	st := t.Stats(now, a.cfg.Location)
	return screen.View{Phase: string(t.Phase), Status: string(t.Status), Display: string(t.Settings.Display), Remaining: t.Remaining(now),
		Full: t.Settings.Length(t.Phase), Round: t.Round, LongEvery: t.Settings.LongEvery, Today: st.Today.Done, Goal: t.Settings.DailyGoal}
}

// showLocked sends the robot its picture and LEDs when they changed: a picture every second
// only with the seconds shown, once a minute with minutes, six times a phase when calm. a.mu held.
func (a *App) showLocked(r *robot, now time.Time) {
	c := r.conn
	if c == nil {
		return
	}
	v := a.screenView(r, now)
	if key := screen.Key(v); key != r.shownFrame {
		r.shownFrame = key
		c.Binary(wire.BinShowJPEG, screen.Frame(v))
	}
	if !r.timer.Settings.Lights {
		v.Status = "idle" // all off
	}
	leds := strings.Join(screen.LEDs(v), ",")
	if leds != r.shownLEDs && slices.Contains(r.commands, "leds") {
		r.shownLEDs = leds
		px := []any{}
		for _, c := range screen.LEDs(v) {
			px = append(px, c)
		}
		c.Command("leds", map[string]any{"pixels": px})
	}
}

// Run moves every timer on, keeps the robots' screens up to date, saves, and reports pairings
// to the manager.
func (a *App) Run(ctx context.Context) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	save := time.NewTicker(5 * time.Second)
	defer save.Stop()
	report := time.NewTicker(15 * time.Second)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			a.step(a.cfg.Now())
		case <-save.C:
			if err := a.Save(); err != nil {
				a.log.Warn("state not saved", "err", err)
			}
			a.mu.Lock()
			a.expirePairings()
			a.mu.Unlock()
		case <-report.C:
			a.reportAllSeen(ctx)
		}
	}
}

// step: a phase that is over moves on; the robots' screens follow the clock.
func (a *App) step(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.robots {
		for i := 0; i < 3; i++ { // a long gap (the server was away) may end more than one phase
			ev := r.timer.Tick(now)
			if ev == "" {
				break
			}
			a.dirty = true
			a.reactLocked(r, ev)
			a.publishState(r)
		}
		a.showLocked(r, now)
	}
}

/* ------------------------------- the manager ------------------------------ */

// reportSeen tells the manager which browsers are paired with a robot it set up; the answer
// names those its owner removed there (the hub knows which robots have a manager's token).
func (a *App) reportSeen(ctx context.Context, robotID string) {
	a.mu.Lock()
	pairings := a.pairings(robotID)
	a.mu.Unlock()
	unpair, err := a.hub.Report(ctx, robotID, pairings)
	if err != nil {
		a.log.Warn("manager", "robot", robotID, "err", err)
		return
	}
	a.unpair(robotID, unpair)
}

func (a *App) reportAllSeen(ctx context.Context) {
	for _, c := range a.hub.Robots() {
		a.reportSeen(ctx, c.ID())
	}
}

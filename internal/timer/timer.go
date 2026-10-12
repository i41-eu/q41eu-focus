// Package timer is the focus timer of one robot (Pomodoro style): focus periods with short
// breaks between them and a long break after every few; start, pause, skip, reset; the
// periods it completed, for the statistics. It is pure: the caller gives the time.
package timer

import (
	"cmp"
	"slices"
	"time"
)

// Phase is what the time is for.
type Phase string

const (
	Focus Phase = "focus"
	Short Phase = "short" // a short break
	Long  Phase = "long"  // a long break, after LongEvery focus periods
)

// Status is whether the time runs.
type Status string

const (
	Idle    Status = "idle"    // ready to start this phase, its whole length
	Running Status = "running" // counting down to EndsAt
	Paused  Status = "paused"  // stopped with Left to go
)

// Display is how the robot's screen and the page show the time left.
type Display string

const (
	Calm    Display = "calm"    // no number: the phase and a ring that empties in a few steps (the default)
	Minutes Display = "minutes" // the whole minutes left, once a minute
	Seconds Display = "seconds" // minutes and seconds, every second
)

// Settings are the person's choices.
type Settings struct {
	FocusMin       int  `json:"focus_min"`         // 25
	ShortMin       int  `json:"short_min"`         // 5
	LongMin        int  `json:"long_min"`          // 15
	LongEvery      int  `json:"long_every"`        // a long break after this many focus periods (4)
	AutoBreak      bool `json:"auto_break"`        // a break starts by itself when a focus period ends
	AutoFocus      bool `json:"auto_focus"`        // the next focus period starts by itself after a break
	DailyGoal      int  `json:"daily_goal"`        // focus periods a day (0 = none)
	Sound          bool `json:"sound"`             // a chime at the end of each phase
	Lights         bool `json:"lights"`            // the robot's LEDs show the time left
	Moves          bool `json:"moves"`             // the robot moves its head at the end of a phase
	DayStartsAtMin int  `json:"day_starts_at_min"` // a "day" of statistics starts at this local minute (0 = midnight)
	// Display: how the time left shows; calm unless minutes or seconds were chosen (also for a
	// timer saved before the setting existed).
	Display Display `json:"display"`
}

// DefaultSettings are the classic 25/5/15, four before a long break, and no number counting down.
func DefaultSettings() Settings {
	return Settings{FocusMin: 25, ShortMin: 5, LongMin: 15, LongEvery: 4, DailyGoal: 8,
		Sound: true, Lights: true, Moves: true, Display: Calm}
}

// Clean keeps the settings within sane bounds.
func (s Settings) Clean() Settings {
	clamp := func(v, lo, hi, def int) int {
		if v == 0 {
			return def
		}
		return max(lo, min(hi, v))
	}
	s.FocusMin = clamp(s.FocusMin, 1, 180, 25)
	s.ShortMin = clamp(s.ShortMin, 1, 60, 5)
	s.LongMin = clamp(s.LongMin, 1, 120, 15)
	s.LongEvery = clamp(s.LongEvery, 1, 12, 4)
	s.DailyGoal = max(0, min(48, s.DailyGoal))
	s.DayStartsAtMin = max(0, min(23*60+59, s.DayStartsAtMin))
	if s.Display != Minutes && s.Display != Seconds {
		s.Display = Calm
	}
	return s
}

// Length is the phase's full length.
func (s Settings) Length(p Phase) time.Duration {
	switch p {
	case Short:
		return time.Duration(s.ShortMin) * time.Minute
	case Long:
		return time.Duration(s.LongMin) * time.Minute
	}
	return time.Duration(s.FocusMin) * time.Minute
}

// Done is one completed focus period.
type Done struct {
	At      time.Time `json:"at"`      // when it ended
	Minutes int       `json:"minutes"` // how long it was
	Task    string    `json:"task,omitempty"`
}

// Timer is one robot's timer. Exported fields are saved.
type Timer struct {
	Settings Settings      `json:"settings"`
	Phase    Phase         `json:"phase"`
	Status   Status        `json:"status"`
	EndsAt   time.Time     `json:"ends_at,omitzero"` // Running
	Left     time.Duration `json:"left"`             // Idle and Paused: what is left
	Round    int           `json:"round"`            // focus periods done since the last long break
	Task     string        `json:"task,omitempty"`   // what the focus is on (optional)
	History  []Done        `json:"history,omitempty"`
	Voided   []time.Time   `json:"voided,omitempty"` // focus periods ended early (skipped or reset while running)
	// Interruptions noted during focus without stopping it (Cirillo's marks): "internal" (an
	// urge, a thought: ') or "external" (someone or something: -).
	Interruptions []Mark `json:"interruptions,omitempty"`
}

// Mark is one interruption.
type Mark struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // internal, external
}

// Interrupt notes an interruption; the timer goes on (it is a note, not a stop).
func (t *Timer) Interrupt(now time.Time, kind string) bool {
	if kind != "internal" && kind != "external" {
		return false
	}
	t.Interruptions = append(t.Interruptions, Mark{At: now.UTC(), Kind: kind})
	t.prune(now)
	return true
}

// keep is how long the history is kept.
const keep = 120 * 24 * time.Hour

// New is a timer ready for its first focus period.
func New(s Settings) *Timer {
	s = s.Clean()
	return &Timer{Settings: s, Phase: Focus, Status: Idle, Left: s.Length(Focus)}
}

// Remaining is the time left now.
func (t *Timer) Remaining(now time.Time) time.Duration {
	if t.Status == Running {
		return max(0, t.EndsAt.Sub(now))
	}
	return t.Left
}

// Progress is how much of the phase is behind (0..1).
func (t *Timer) Progress(now time.Time) float64 {
	full := t.Settings.Length(t.Phase)
	if full <= 0 {
		return 0
	}
	return min(1, max(0, 1-float64(t.Remaining(now))/float64(full)))
}

// Event is what happened, for the robot and the pages.
type Event string

const (
	Started   Event = "started"
	Paused_   Event = "paused"
	Resumed   Event = "resumed"
	Completed Event = "completed" // a focus period ended in full: one more done
	BreakOver Event = "break_over"
	Skipped   Event = "skipped"
	Reset     Event = "reset"
)

// Start runs the current phase (from idle or paused).
func (t *Timer) Start(now time.Time) Event {
	switch t.Status {
	case Running:
		return ""
	case Paused:
		t.Status, t.EndsAt = Running, now.Add(t.Left)
		return Resumed
	}
	t.Status, t.EndsAt = Running, now.Add(t.Left)
	return Started
}

// Pause stops the clock.
func (t *Timer) Pause(now time.Time) Event {
	if t.Status != Running {
		return ""
	}
	t.Left, t.Status = t.Remaining(now), Paused
	return Paused_
}

// Toggle starts or pauses: the one button on the robot.
func (t *Timer) Toggle(now time.Time) Event {
	if t.Status == Running {
		return t.Pause(now)
	}
	return t.Start(now)
}

// Skip ends the phase now without counting it, and goes to the next one (idle). A focus period
// cut short is void, as in the Pomodoro Technique, and goes to the next break; a break skipped
// goes to the next focus period.
func (t *Timer) Skip(now time.Time) Event {
	if t.Phase == Focus && t.Status != Idle {
		t.void(now)
	}
	t.next(now, false)
	return Skipped
}

// ResetAll goes back to the first focus period of a set (the task stays).
func (t *Timer) ResetAll(now time.Time) Event {
	if t.Phase == Focus && t.Status != Idle {
		t.void(now)
	}
	t.Phase, t.Status, t.Round, t.EndsAt = Focus, Idle, 0, time.Time{}
	t.Left = t.Settings.Length(Focus)
	return Reset
}

func (t *Timer) void(now time.Time) {
	t.Voided = append(t.Voided, now.UTC())
}

// Tick moves the timer on when a running phase is over. It returns what happened (one event at
// most; call it again for the next).
func (t *Timer) Tick(now time.Time) Event {
	if t.Status != Running || now.Before(t.EndsAt) {
		return ""
	}
	end := t.EndsAt
	if t.Phase == Focus {
		t.History = append(t.History, Done{At: end.UTC(), Minutes: t.Settings.FocusMin, Task: t.Task})
		t.prune(now)
		t.Round++
		t.next(end, true)
		return Completed
	}
	t.next(end, true)
	return BreakOver
}

// next prepares the phase after this one, started at from when it starts by itself.
func (t *Timer) next(from time.Time, natural bool) {
	switch {
	case t.Phase != Focus:
		if t.Phase == Long {
			t.Round = 0
		}
		t.Phase = Focus
	case t.Round >= t.Settings.LongEvery:
		t.Phase = Long
	default:
		t.Phase = Short
	}
	t.Left, t.EndsAt, t.Status = t.Settings.Length(t.Phase), time.Time{}, Idle
	auto := t.Settings.AutoBreak && t.Phase != Focus || t.Settings.AutoFocus && t.Phase == Focus
	if natural && auto {
		t.Status, t.EndsAt = Running, from.Add(t.Left)
	}
}

// SetSettings changes the settings; an idle phase takes its new length.
func (t *Timer) SetSettings(s Settings) {
	t.Settings = s.Clean()
	if t.Status == Idle {
		t.Left = t.Settings.Length(t.Phase)
	}
}

func (t *Timer) prune(now time.Time) {
	cut := now.Add(-keep)
	i := 0
	for i < len(t.History) && t.History[i].At.Before(cut) {
		i++
	}
	t.History = t.History[i:]
	j := 0
	for j < len(t.Voided) && t.Voided[j].Before(cut) {
		j++
	}
	t.Voided = t.Voided[j:]
	k := 0
	for k < len(t.Interruptions) && t.Interruptions[k].At.Before(cut) {
		k++
	}
	t.Interruptions = t.Interruptions[k:]
}

// Day is one day's statistics.
type Day struct {
	Date    string `json:"date"` // YYYY-MM-DD, local
	Done    int    `json:"done"`
	Minutes int    `json:"minutes"`
	Voided  int    `json:"voided"`
	// Interruptions noted that day.
	Internal int `json:"internal"`
	External int `json:"external"`
}

// Stats are what the pages show.
type Stats struct {
	Today  Day         `json:"today"`
	Week   []Day       `json:"week"`   // the last 7 days, oldest first, today last
	Streak int         `json:"streak"` // days in a row (until today, or yesterday) with at least one done
	Total  int         `json:"total"`  // in the kept history
	Goal   int         `json:"goal"`
	Tasks  []TaskCount `json:"tasks,omitempty"` // this week, the most first
}

// TaskCount is focus periods per task.
type TaskCount struct {
	Task string `json:"task"`
	Done int    `json:"done"`
}

// dayOf is the statistics day a moment belongs to (days start at DayStartsAtMin).
func (t *Timer) dayOf(at time.Time, loc *time.Location) string {
	return at.In(loc).Add(-time.Duration(t.Settings.DayStartsAtMin) * time.Minute).Format(time.DateOnly)
}

// Stats counts the history in the time zone loc.
func (t *Timer) Stats(now time.Time, loc *time.Location) Stats {
	today := t.dayOf(now, loc)
	byDay := map[string]*Day{}
	get := func(d string) *Day {
		if byDay[d] == nil {
			byDay[d] = &Day{Date: d}
		}
		return byDay[d]
	}
	st := Stats{Goal: t.Settings.DailyGoal, Total: len(t.History)}
	base := now.In(loc).Add(-time.Duration(t.Settings.DayStartsAtMin) * time.Minute)
	weekStart := time.Date(base.Year(), base.Month(), base.Day()-6, 0, 0, 0, 0, loc).Format(time.DateOnly)
	tasks := map[string]int{}
	for _, d := range t.History {
		day := get(t.dayOf(d.At, loc))
		day.Done++
		day.Minutes += d.Minutes
		if day.Date >= weekStart && d.Task != "" {
			tasks[d.Task]++
		}
	}
	for _, v := range t.Voided {
		get(t.dayOf(v, loc)).Voided++
	}
	for _, m := range t.Interruptions {
		if d := get(t.dayOf(m.At, loc)); m.Kind == "internal" {
			d.Internal++
		} else {
			d.External++
		}
	}
	for i := 6; i >= 0; i-- {
		d := time.Date(base.Year(), base.Month(), base.Day()-i, 12, 0, 0, 0, loc).Format(time.DateOnly)
		st.Week = append(st.Week, *get(d))
	}
	st.Today = *get(today)
	// The streak: today counts once it has one; otherwise it runs until yesterday.
	for i := 0; ; i++ {
		d := time.Date(base.Year(), base.Month(), base.Day()-i, 12, 0, 0, 0, loc).Format(time.DateOnly)
		if get(d).Done > 0 {
			st.Streak++
		} else if i > 0 {
			break
		}
		if i > 400 {
			break
		}
	}
	for task, n := range tasks {
		st.Tasks = append(st.Tasks, TaskCount{Task: task, Done: n})
	}
	sortTasks(st.Tasks)
	return st
}

func sortTasks(ts []TaskCount) {
	slices.SortFunc(ts, func(a, b TaskCount) int {
		return cmp.Or(cmp.Compare(b.Done, a.Done), cmp.Compare(a.Task, b.Task))
	})
}

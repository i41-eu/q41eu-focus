package timer

import (
	"encoding/json"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC)

func TestCycle(t *testing.T) {
	s := DefaultSettings()
	s.LongEvery, s.AutoBreak = 2, true
	tm := New(s)
	now := t0
	if ev := tm.Start(now); ev != Started || tm.Remaining(now) != 25*time.Minute {
		t.Fatalf("start: %v %v", ev, tm.Remaining(now))
	}
	now = now.Add(10 * time.Minute)
	if p := tm.Progress(now); p < 0.39 || p > 0.41 {
		t.Errorf("progress %v", p)
	}
	tm.Pause(now)
	now = now.Add(time.Hour) // paused: the clock stands
	if tm.Remaining(now) != 15*time.Minute || tm.Tick(now) != "" {
		t.Fatalf("paused: %v", tm.Remaining(now))
	}
	if tm.Toggle(now) != Resumed {
		t.Fatal("resume")
	}
	now = now.Add(15 * time.Minute)
	if ev := tm.Tick(now); ev != Completed || tm.Phase != Short || tm.Status != Running || len(tm.History) != 1 {
		t.Fatalf("focus over: %v %+v", ev, tm)
	}
	// The break starts by itself (AutoBreak), the next focus does not (AutoFocus off).
	now = now.Add(5 * time.Minute)
	if ev := tm.Tick(now); ev != BreakOver || tm.Phase != Focus || tm.Status != Idle {
		t.Fatalf("break over: %v %+v", ev, tm)
	}
	tm.Start(now)
	now = now.Add(25 * time.Minute)
	if ev := tm.Tick(now); ev != Completed || tm.Phase != Long || tm.Remaining(now) != 15*time.Minute {
		t.Fatalf("the second: a long break: %v %+v", ev, tm)
	}
	now = now.Add(15 * time.Minute)
	if ev := tm.Tick(now); ev != BreakOver || tm.Round != 0 || tm.Phase != Focus {
		t.Fatalf("after the long break: %v %+v", ev, tm)
	}
}

func TestTickLateKeepsSchedule(t *testing.T) {
	// The server was busy: the break that started by itself started when the focus ended.
	s := DefaultSettings()
	s.AutoBreak = true
	tm := New(s)
	tm.Start(t0)
	late := t0.Add(25*time.Minute + 40*time.Second)
	tm.Tick(late)
	if got := tm.Remaining(late); got != 5*time.Minute-40*time.Second {
		t.Errorf("break left %v", got)
	}
}

func TestSkipAndResetVoid(t *testing.T) {
	tm := New(DefaultSettings())
	tm.Start(t0)
	if tm.Skip(t0.Add(time.Minute)) != Skipped || tm.Phase != Short || tm.Status != Idle || len(tm.Voided) != 1 || len(tm.History) != 0 {
		t.Fatalf("skip a focus: %+v", tm)
	}
	tm.Skip(t0.Add(2 * time.Minute)) // skip the break: no void
	if tm.Phase != Focus || len(tm.Voided) != 1 {
		t.Fatalf("skip a break: %+v", tm)
	}
	tm.Start(t0.Add(3 * time.Minute))
	tm.ResetAll(t0.Add(4 * time.Minute))
	if tm.Phase != Focus || tm.Status != Idle || tm.Round != 0 || len(tm.Voided) != 2 || tm.Remaining(t0) != 25*time.Minute {
		t.Fatalf("reset: %+v", tm)
	}
}

func TestSettingsClean(t *testing.T) {
	s := Settings{FocusMin: 9999, ShortMin: -3, LongEvery: 0}.Clean()
	if s.FocusMin != 180 || s.ShortMin != 1 || s.LongMin != 15 || s.LongEvery != 4 {
		t.Errorf("%+v", s)
	}
	tm := New(DefaultSettings())
	s = DefaultSettings()
	s.FocusMin = 50
	tm.SetSettings(s)
	if tm.Remaining(t0) != 50*time.Minute {
		t.Errorf("idle takes the new length: %v", tm.Remaining(t0))
	}
}

func TestStats(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Prague")
	tm := New(DefaultSettings())
	now := time.Date(2026, 10, 9, 15, 0, 0, 0, loc)
	add := func(daysAgo, n int, task string) {
		for i := 0; i < n; i++ {
			tm.History = append(tm.History, Done{At: now.AddDate(0, 0, -daysAgo).Add(-time.Duration(i) * time.Hour).UTC(), Minutes: 25, Task: task})
		}
	}
	add(3, 1, "")
	add(2, 2, "report")
	add(1, 1, "report")
	add(0, 3, "code")
	tm.Voided = []time.Time{now.UTC()}
	tm.Interrupt(now, "internal")
	tm.Interrupt(now, "external")
	tm.Interrupt(now, "external")
	if tm.Interrupt(now, "phone") {
		t.Error("an unknown kind")
	}
	st := tm.Stats(now, loc)
	if st.Today.Internal != 1 || st.Today.External != 2 {
		t.Errorf("interruptions %+v", st.Today)
	}
	if st.Today.Done != 3 || st.Today.Minutes != 75 || st.Today.Voided != 1 || st.Streak != 4 || len(st.Week) != 7 || st.Week[6].Date != "2026-10-09" {
		t.Fatalf("%+v", st)
	}
	if len(st.Tasks) != 2 || st.Tasks[0].Task != "code" || st.Tasks[1].Done != 3 {
		t.Errorf("tasks %+v", st.Tasks)
	}
	// Nothing today yet: the streak runs until yesterday.
	st = tm.Stats(now.AddDate(0, 0, 1).Add(-14*time.Hour), loc)
	if st.Today.Done != 0 || st.Streak != 4 {
		t.Errorf("next morning: %+v", st)
	}
	// A day that starts at 4:00: a period done at 2:00 belongs to the day before.
	tm2 := New(DefaultSettings())
	tm2.Settings.DayStartsAtMin = 4 * 60
	tm2.History = []Done{{At: time.Date(2026, 10, 10, 2, 0, 0, 0, loc).UTC(), Minutes: 25}}
	if st := tm2.Stats(time.Date(2026, 10, 10, 3, 0, 0, 0, loc), loc); st.Today.Date != "2026-10-09" || st.Today.Done != 1 {
		t.Errorf("night owl: %+v", st.Today)
	}
}

func TestDisplaySetting(t *testing.T) {
	// A new timer is calm: no number counts down.
	if d := New(DefaultSettings()).Settings.Display; d != Calm {
		t.Errorf("a new timer: %q", d)
	}
	// A timer saved before the setting existed has none; cleaned (as the app does on load), calm.
	var old Timer
	if err := json.Unmarshal([]byte(`{"settings":{"focus_min":50,"short_min":10,"long_min":20,"long_every":3,"daily_goal":8,`+
		`"sound":true,"lights":true,"moves":true},"phase":"focus","status":"idle","left":3000000000000}`), &old); err != nil {
		t.Fatal(err)
	}
	if s := old.Settings.Clean(); s.Display != Calm || s.FocusMin != 50 {
		t.Errorf("a timer saved before: %+v", s)
	}
	for in, want := range map[Display]Display{"": Calm, "calm": Calm, "minutes": Minutes, "seconds": Seconds, "fancy": Calm, "Minutes": Calm} {
		if got := (Settings{Display: in}).Clean().Display; got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
	// A choice is kept, and saved under "display".
	tm := New(DefaultSettings())
	s := tm.Settings
	s.Display = Minutes
	tm.SetSettings(s)
	b, _ := json.Marshal(tm)
	var back Timer
	json.Unmarshal(b, &back)
	if tm.Settings.Display != Minutes || back.Settings.Display != Minutes {
		t.Errorf("minutes: %q, saved %s", tm.Settings.Display, b)
	}
}

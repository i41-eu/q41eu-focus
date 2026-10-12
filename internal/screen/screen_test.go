package screen

import (
	"bytes"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClockAndStatus(t *testing.T) {
	for d, want := range map[time.Duration]string{25 * time.Minute: "25:00", 25*time.Minute - 300*time.Millisecond: "25:00",
		time.Second: "0:01", 0: "0:00", 59*time.Second + time.Millisecond: "1:00"} {
		if got := Clock(d); got != want {
			t.Errorf("Clock(%v) = %s, want %s", d, got, want)
		}
	}
	for d, want := range map[time.Duration]int{25 * time.Minute: 25, 24*time.Minute + 59*time.Second: 25, 24 * time.Minute: 24,
		time.Millisecond: 1, 0: 0, -time.Second: 0} {
		if got := Minutes(d); got != want {
			t.Errorf("Minutes(%v) = %d, want %d", d, got, want)
		}
	}
	v := View{Phase: "focus", Status: "running", Display: "seconds", Remaining: 11*time.Minute + time.Second, Full: 25 * time.Minute}
	if s := Status(v); s != "FREE IN 12 MIN" {
		t.Errorf("status %q", s)
	}
}

// running is a focus period with left to go, shown in display.
func running(display string, left time.Duration) View {
	return View{Phase: "focus", Status: "running", Display: display, Remaining: left, Full: 25 * time.Minute, Round: 1, LongEvery: 4, Today: 3, Goal: 8}
}

func TestDisplay(t *testing.T) {
	left := 17*time.Minute + 42*time.Second
	for _, c := range []struct{ display, number, line string }{
		{"seconds", "17:42", "FREE IN 18 MIN"},
		{"minutes", "18", "FREE IN 18 MIN"},
		{"calm", "", "FOCUSING"},
		{"", "", "FOCUSING"}, // a view without a display: calm
		{"fancy", "", "FOCUSING"},
	} {
		v := running(c.display, left)
		if n, l := Number(v), Status(v); n != c.number || l != c.line {
			t.Errorf("%q: %q %q, want %q %q", c.display, n, l, c.number, c.line)
		}
	}
	// Breaks, pauses and idle read the same in every display.
	for _, d := range []string{"calm", "minutes", "seconds"} {
		v := running(d, left)
		v.Phase = "long"
		if s := Status(v); s != "TAKE A WALK" {
			t.Errorf("%s, a long break: %q", d, s)
		}
		v.Status = "paused"
		if s := Status(v); s != "PAUSED" {
			t.Errorf("%s, paused: %q", d, s)
		}
	}

	// How often the picture changes over a whole 25 minute focus period, second by second: calm
	// six times (and once more at the very end), minutes once a minute, seconds every second.
	for display, want := range map[string]int{"calm": Steps + 1, "minutes": 25 + 1, "seconds": 25*60 + 1} {
		keys := map[string]bool{}
		for s := 25 * 60; s >= 0; s-- {
			keys[Key(running(display, time.Duration(s)*time.Second))] = true
		}
		if len(keys) != want {
			t.Errorf("%s: %d different pictures in a focus period, want %d", display, len(keys), want)
		}
	}

	// The same key is the same picture; calm shows no number: the picture at 17:42 is the one at
	// 16:41 (both in the fifth sixth), and only the sixth's end changes it.
	same := func(a, b View) bool { return bytes.Equal(Frame(a), Frame(b)) }
	if !same(running("calm", left), running("calm", 16*time.Minute+41*time.Second)) {
		t.Error("calm: the picture changed within a sixth of the phase")
	}
	if same(running("calm", 16*time.Minute+40*time.Second+time.Millisecond), running("calm", 16*time.Minute+40*time.Second)) {
		t.Error("calm: no change at the end of a sixth")
	}
	if !same(running("minutes", left), running("minutes", 17*time.Minute+time.Second)) || same(running("minutes", left), running("minutes", 17*time.Minute)) {
		t.Error("minutes: the picture must change once a minute, at the whole minute")
	}
	if same(running("seconds", left), running("seconds", left-time.Second)) {
		t.Error("seconds: the picture must change every second")
	}
	if same(running("calm", left), running("minutes", left)) || same(running("minutes", left), running("seconds", left)) {
		t.Error("the displays draw the same picture")
	}
	// The LEDs are the same in every display, in the calm ring's steps.
	for _, d := range []string{"minutes", "seconds"} {
		if a, b := LEDs(running("calm", left)), LEDs(running(d, left)); strings.Join(a, ",") != strings.Join(b, ",") {
			t.Errorf("leds calm %v, %s %v", a, d, b)
		}
	}
	if s := Step(running("calm", left)); s != 5 {
		t.Errorf("step at 17:42 of 25:00: %d", s)
	}
}

func TestFrameAndLEDs(t *testing.T) {
	views := map[string]View{
		"idle":    {Phase: "focus", Status: "idle", Remaining: 25 * time.Minute, Full: 25 * time.Minute, LongEvery: 4, Today: 0, Goal: 8},
		"running": {Phase: "focus", Status: "running", Remaining: 11 * time.Minute, Full: 25 * time.Minute, Round: 2, LongEvery: 4, Today: 5, Goal: 8},
		"paused":  {Phase: "focus", Status: "paused", Remaining: 3 * time.Minute, Full: 25 * time.Minute, Round: 1, LongEvery: 4, Today: 1},
		"break":   {Phase: "long", Status: "running", Remaining: 14 * time.Minute, Full: 15 * time.Minute, Round: 4, LongEvery: 4, Today: 4, Goal: 8},
	}
	for name, v := range views {
		for _, d := range []string{"calm", "minutes", "seconds"} {
			v.Display = d
			b := Frame(v)
			img, err := jpeg.Decode(bytes.NewReader(b))
			if err != nil || img.Bounds().Dx() != W || img.Bounds().Dy() != H || len(b) > 40<<10 {
				t.Fatalf("%s, %s: %v, %d bytes", name, d, err, len(b))
			}
			if dir := os.Getenv("SCREEN_OUT"); dir != "" {
				os.WriteFile(filepath.Join(dir, name+"-"+d+".jpg"), b, 0o644)
			}
		}
	}
	leds := LEDs(views["running"]) // 11 of 25 min left: 3 of 6 on each side
	lit := 0
	for _, c := range leds {
		if c != "#000000" {
			lit++
		}
	}
	if lit != 6 || leds[0] == "#000000" || leds[11] == "#000000" || leds[5] != "#000000" {
		t.Errorf("leds %v", leds)
	}
	for _, c := range LEDs(views["idle"]) {
		if c != "#000000" {
			t.Fatalf("idle leds %v", LEDs(views["idle"]))
		}
	}
}

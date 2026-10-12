package screen

import (
	"bytes"
	"image/jpeg"
	"os"
	"path/filepath"
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
	v := View{Phase: "focus", Status: "running", Remaining: 11*time.Minute + time.Second, Full: 25 * time.Minute}
	if s := Status(v); s != "FREE IN 12 MIN" {
		t.Errorf("status %q", s)
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
		b := Frame(v)
		img, err := jpeg.Decode(bytes.NewReader(b))
		if err != nil || img.Bounds().Dx() != W || img.Bounds().Dy() != H || len(b) > 40<<10 {
			t.Fatalf("%s: %v, %d bytes", name, err, len(b))
		}
		if dir := os.Getenv("SCREEN_OUT"); dir != "" {
			os.WriteFile(filepath.Join(dir, name+".jpg"), b, 0o644)
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

// Package screen draws the timer for the robot: a 320x240 picture (JPEG, sent as
// wire.BinShowJPEG) and the colors of its 12 LEDs. The picture is a "Time Timer" ring of the
// time left in the phase's color, the phase at the top, today's count, the set's dots, and a line
// for the room. How the time left shows is the View's Display: calm (the default) has no number,
// the ring in six segments and the line for the room in the middle ("FOCUSING"); minutes puts the
// whole minutes left in the middle ("18 MIN"), seconds a countdown ("17:42"), both with the line
// under them ("FREE IN 18 MIN").
package screen

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"strings"
	"time"
)

// W, H: the robot's screen.
const W, H = 320, 240

// View is what the robot shows.
type View struct {
	Phase     string        // focus, short, long
	Status    string        // idle, running, paused
	Display   string        // calm (no number; also any other value), minutes, seconds
	Remaining time.Duration // in the phase
	Full      time.Duration // the phase's length
	Round     int           // focus periods done in this set (before the long break)
	LongEvery int
	Today     int // focus periods done today
	Goal      int // a day's goal (0 = none)
}

// Colors.
var (
	bg      = color.RGBA{0x10, 0x14, 0x2a, 0xff}
	track   = color.RGBA{0x2a, 0x30, 0x50, 0xff}
	white   = color.RGBA{0xf2, 0xf4, 0xff, 0xff}
	muted   = color.RGBA{0x8a, 0x93, 0xb8, 0xff}
	amber   = color.RGBA{0xf5, 0xb0, 0x3c, 0xff}
	colorOf = map[string]color.RGBA{
		"focus": {0xe5, 0x53, 0x3d, 0xff}, // tomato
		"short": {0x2e, 0xad, 0x6b, 0xff}, // green
		"long":  {0x3b, 0x82, 0xf6, 0xff}, // blue
	}
	labelOf = map[string]string{"focus": "FOCUS", "short": "SHORT BREAK", "long": "LONG BREAK"}
)

// Phase color (focus if unknown).
func phaseColor(p string) color.RGBA {
	if c, ok := colorOf[p]; ok {
		return c
	}
	return colorOf["focus"]
}

// Steps: the calm ring's segments, as many as the LEDs on each side.
const Steps = 6

// display is v.Display: calm unless minutes or seconds.
func display(v View) string {
	if v.Display == "minutes" || v.Display == "seconds" {
		return v.Display
	}
	return "calm"
}

// Clock is the remaining time as M:SS or MM:SS (minutes rounded down, seconds up: 24:59 right
// after the start, 0:01 at the end, as a kitchen timer).
func Clock(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// Minutes is the remaining time in whole minutes, rounded up: 25 right after the start of 25
// minutes, 1 in the last minute, 0 only at the end.
func Minutes(d time.Duration) int {
	return int((max(0, d) + time.Minute - 1) / time.Minute)
}

// Step is the sixths of the phase still to go, rounded up (0..Steps): Steps until the first
// sixth is over, 0 only at the end. The LEDs and the calm ring show it.
func Step(v View) int {
	if v.Full <= 0 {
		return Steps
	}
	left := min(max(0, v.Remaining), v.Full)
	return int((Steps*left + v.Full - 1) / v.Full)
}

// Number is the time left as the middle of the screen shows it: "17:42" (seconds), "18" (minutes,
// with MIN beside it), "" (calm: no number).
func Number(v View) string {
	switch display(v) {
	case "seconds":
		return Clock(v.Remaining)
	case "minutes":
		return fmt.Sprint(Minutes(v.Remaining))
	}
	return ""
}

// Status is the line for the room: under the number, or in the middle when calm. Calm names no
// minutes: "FOCUSING", not "FREE IN 12 MIN".
func Status(v View) string {
	switch {
	case v.Status == "paused":
		return "PAUSED"
	case v.Status == "idle" && v.Phase == "focus":
		return "TAP TO FOCUS"
	case v.Status == "idle":
		return "TAP TO REST"
	case v.Phase == "focus" && display(v) == "calm":
		return "FOCUSING"
	case v.Phase == "focus":
		return fmt.Sprintf("FREE IN %d MIN", Minutes(v.Remaining))
	case v.Phase == "long":
		return "TAKE A WALK"
	}
	return "STRETCH"
}

// ringLeft is the share of the ring lit (0..1), changing no more often than the number: calm in
// sixths, minutes by the minute, seconds by the second.
func ringLeft(v View) float64 {
	if v.Full <= 0 {
		return 1
	}
	var d time.Duration
	switch display(v) {
	case "calm":
		return float64(Step(v)) / Steps
	case "minutes":
		d = time.Duration(Minutes(v.Remaining)) * time.Minute
	default:
		d = time.Duration(math.Ceil(v.Remaining.Seconds())) * time.Second
	}
	return math.Max(0, math.Min(1, d.Seconds()/v.Full.Seconds()))
}

// Key names the picture: Frame draws the same picture for views with the same key, so the robot
// needs a new one only when the key changes (calm: six times a phase; minutes: once a minute;
// seconds: every second; and on a start, a pause, a skip, a new setting).
func Key(v View) string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%.6f|%d/%d|%d/%d", v.Phase, v.Status, display(v), Number(v), Status(v),
		ringLeft(v), v.Round, v.LongEvery, v.Today, v.Goal)
}

// Frame draws the picture.
func Frame(v View) []byte {
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	pc := phaseColor(v.Phase)

	// The ring: the time left from 12 o'clock clockwise, the rest dark; calm: in Steps segments,
	// with gaps between them.
	const cx, cy, ro, ri = 160, 131, 108, 92
	const gap = 1.5 * math.Pi / 180 // half a gap between calm segments
	left, calm, seg := ringLeft(v), display(v) == "calm", 2*math.Pi/Steps
	arc := pc
	if v.Status == "paused" {
		arc = amber
	}
	for y := cy - ro; y <= cy+ro; y++ {
		for x := cx - ro; x <= cx+ro; x++ {
			dx, dy := float64(x-cx)+0.5, float64(y-cy)+0.5
			r := math.Hypot(dx, dy)
			if r > ro || r < ri {
				continue
			}
			a := math.Atan2(dx, -dy) // 0 at the top, clockwise
			if a < 0 {
				a += 2 * math.Pi
			}
			if calm && math.Mod(a+gap, seg) < 2*gap {
				continue
			}
			c := track
			if a <= left*2*math.Pi {
				c = arc
			}
			img.SetRGBA(x, y, c)
		}
	}

	// The phase at the top left, today's count at the top right.
	text(img, labelOf[v.Phase], 8, 6, 2, pc)
	count := fmt.Sprint(v.Today)
	if v.Goal > 0 {
		count += "/" + fmt.Sprint(v.Goal)
	}
	text(img, count, W-8-textWidth(count, 2), 6, 2, white)

	// Above the middle: play (idle) or pause (paused).
	switch v.Status {
	case "idle":
		for i := 0; i < 18; i++ { // a triangle pointing right
			draw.Draw(img, image.Rect(152+i, 64+i/2, 153+i, 82-i/2), &image.Uniform{white}, image.Point{}, draw.Src)
		}
	case "paused":
		draw.Draw(img, image.Rect(150, 64, 156, 82), &image.Uniform{amber}, image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(164, 64, 170, 82), &image.Uniform{amber}, image.Point{}, draw.Src)
	}

	// The middle: the number (minutes and seconds, or minutes with MIN beside them) and the line
	// for the room under it; calm: no number, the line alone, larger (in two lines if need be).
	switch n := Number(v); display(v) {
	case "seconds":
		centered(img, n, cx, 97, 6, white)
		centered(img, Status(v), cx, 152, 2, muted)
	case "minutes":
		x := cx - (textWidth(n, 6)+8+textWidth("MIN", 2))/2
		text(img, n, x, 97, 6, white)
		text(img, "MIN", x+textWidth(n, 6)+8, 97+7*6-7*2, 2, muted)
		centered(img, Status(v), cx, 152, 2, muted)
	default:
		const scale, lineH = 3, 7*3 + 8
		lines := wrap(Status(v), scale, 2*ri-24)
		y := 118 - (len(lines)*lineH-8)/2
		for i, l := range lines {
			centered(img, l, cx, y+i*lineH, scale, white)
		}
	}

	// The set's dots.
	if v.LongEvery > 0 {
		const step, rad = 18, 5
		x0 := cx - (v.LongEvery-1)*step/2
		for i := 0; i < v.LongEvery; i++ {
			c := track
			if i < v.Round {
				c = colorOf["focus"]
			}
			circle(img, x0+i*step, 184, rad, c)
		}
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes()
}

// wrap splits s into lines no wider than maxW at scale, at spaces.
func wrap(s string, scale, maxW int) []string {
	var lines []string
	for _, w := range strings.Fields(s) {
		if n := len(lines); n > 0 && textWidth(lines[n-1]+" "+w, scale) <= maxW {
			lines[n-1] += " " + w
		} else {
			lines = append(lines, w)
		}
	}
	return lines
}

func circle(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

// LEDs are the robot's 12 LEDs ("leds" command "pixels": left 0-5, right 6-11; 0 and 11 at the
// screen end): both strips show the time left in sixths (Step, as the calm ring), from the screen
// end back, in the phase's color, dim (a room light, not a lamp). Idle: off. The same in every
// Display.
func LEDs(v View) []string {
	out := make([]string, 12)
	for i := range out {
		out[i] = "#000000"
	}
	if v.Status == "idle" || v.Full <= 0 {
		return out
	}
	n := Step(v)
	c := phaseColor(v.Phase)
	if v.Status == "paused" {
		c = amber
	}
	dim := fmt.Sprintf("#%02x%02x%02x", c.R/4, c.G/4, c.B/4)
	for i := 0; i < n; i++ {
		out[i] = dim    // left, from the screen end
		out[11-i] = dim // right, from the screen end
	}
	return out
}

// Package screen draws the timer for the robot: a 320x240 picture (JPEG, sent as
// wire.BinShowJPEG) and the colors of its 12 LEDs. The picture is a "Time Timer" ring of the
// time left in the phase's color, the countdown in the middle, the phase at the top, today's
// count, the set's dots, and a line for the room ("FREE IN 12 MIN" while focusing).
package screen

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math"
	"time"
)

// W, H: the robot's screen.
const W, H = 320, 240

// View is what the robot shows.
type View struct {
	Phase     string        // focus, short, long
	Status    string        // idle, running, paused
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

// Clock is the remaining time as M:SS or MM:SS (minutes rounded down, seconds up: 24:59 right
// after the start, 0:01 at the end, as a kitchen timer).
func Clock(d time.Duration) string {
	s := int(math.Ceil(d.Seconds()))
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// Status is the line for the room, under the countdown.
func Status(v View) string {
	switch {
	case v.Status == "paused":
		return "PAUSED"
	case v.Status == "idle" && v.Phase == "focus":
		return "TAP TO FOCUS"
	case v.Status == "idle":
		return "TAP TO REST"
	case v.Phase == "focus":
		return fmt.Sprintf("FREE IN %d MIN", int(math.Ceil(v.Remaining.Minutes())))
	case v.Phase == "long":
		return "TAKE A WALK"
	}
	return "STRETCH"
}

// Frame draws the picture.
func Frame(v View) []byte {
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	pc := phaseColor(v.Phase)

	// The ring: the time left from 12 o'clock clockwise, the rest dark.
	const cx, cy, ro, ri = 160, 131, 108, 92
	left := 1.0
	if v.Full > 0 {
		left = math.Max(0, math.Min(1, v.Remaining.Seconds()/v.Full.Seconds()))
	}
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

	// Above the countdown: play (idle) or pause (paused).
	switch v.Status {
	case "idle":
		for i := 0; i < 18; i++ { // a triangle pointing right
			draw.Draw(img, image.Rect(152+i, 64+i/2, 153+i, 82-i/2), &image.Uniform{white}, image.Point{}, draw.Src)
		}
	case "paused":
		draw.Draw(img, image.Rect(150, 64, 156, 82), &image.Uniform{amber}, image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(164, 64, 170, 82), &image.Uniform{amber}, image.Point{}, draw.Src)
	}

	// The countdown, the line for the room, the set's dots.
	centered(img, Clock(v.Remaining), cx, 97, 6, white)
	centered(img, Status(v), cx, 152, 2, muted)
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
// screen end): both strips show the time left, from the screen end back, in the phase's color,
// dim (a room light, not a lamp). Idle: off.
func LEDs(v View) []string {
	out := make([]string, 12)
	for i := range out {
		out[i] = "#000000"
	}
	if v.Status == "idle" || v.Full <= 0 {
		return out
	}
	n := int(math.Ceil(6 * math.Max(0, math.Min(1, v.Remaining.Seconds()/v.Full.Seconds()))))
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

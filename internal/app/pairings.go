package app

// The browsers paired with a robot, as its owner sees them on the manager (robotauth.Seen
// Pairings): which device, since when, last seen, watching now. The owner may remove them there;
// the manager's answer then lists them (robotauth.Auth Unpair). A pairing unused for pairingTTL
// goes by itself.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/i41-eu/q41eu-raw/robotauth"
	"github.com/i41-eu/q41eu-raw/wire"
)

const pairingTTL = 30 * 24 * time.Hour

// sessionMeta is what the manager's page shows of a browser. Saved with the state.
type sessionMeta struct {
	Device   string               `json:"device,omitempty"` // e.g. "Chrome on Android"
	LastSeen time.Time            `json:"last_seen,omitzero"`
	Since    map[string]time.Time `json:"since,omitempty"` // robot id -> paired at
}

// pairedNow notes when the session paired with the robot. a.mu held.
func (a *App) pairedNow(s *session, robotID string) {
	if s.meta.Since == nil {
		s.meta.Since = map[string]time.Time{}
	}
	s.meta.Since[robotID] = a.cfg.Now().UTC().Truncate(time.Minute)
}

// seen notes a request of the session: its device and when (hours are enough).
func (a *App) seen(sid string, r *http.Request) {
	now := a.cfg.Now().UTC().Truncate(time.Hour)
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.sessions[sid]
	if s == nil {
		return
	}
	if !s.meta.LastSeen.Equal(now) {
		s.meta.LastSeen = now
		a.dirty = true
	}
	if s.meta.Device == "" {
		s.meta.Device = agentSummary(r.UserAgent())
		a.dirty = true
	}
}

// expirePairings drops the pairings of browsers not seen for pairingTTL. a.mu held.
func (a *App) expirePairings() {
	for _, s := range a.sessions {
		if len(s.Robots) > 0 && !s.meta.LastSeen.IsZero() && a.cfg.Now().Sub(s.meta.LastSeen) > pairingTTL {
			s.Robots, s.meta.Since = nil, nil
		}
	}
}

// pairingID is the id the manager gets for a session's pairings: not the session id (a
// credential), but stable for it.
func pairingID(sid string) string {
	sum := sha256.Sum256([]byte("i41 pairing|" + sid))
	return hex.EncodeToString(sum[:6])
}

// pairings lists the browsers paired with the robot, oldest first. a.mu held.
func (a *App) pairings(robotID string) []robotauth.Pairing {
	watching := map[string]bool{}
	for sub := range a.subs {
		watching[sub.sid] = true
	}
	out := []robotauth.Pairing{}
	for sid, s := range a.sessions {
		if slices.Contains(s.Robots, robotID) {
			out = append(out, robotauth.Pairing{ID: pairingID(sid), Device: s.meta.Device, Paired: s.meta.Since[robotID],
				LastSeen: s.meta.LastSeen, Watching: watching[sid]})
		}
	}
	slices.SortFunc(out, func(x, y robotauth.Pairing) int {
		if c := x.Paired.Compare(y.Paired); c != 0 {
			return c
		}
		return strings.Compare(x.ID, y.ID)
	})
	return out
}

// unpair drops the robot's pairings the owner removed on the manager (by pairing id) and tells
// the robot how many are left.
func (a *App) unpair(robotID string, ids []string) {
	if len(ids) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	dropped := 0
	for sid, s := range a.sessions {
		if slices.Contains(s.Robots, robotID) && slices.Contains(ids, pairingID(sid)) {
			s.Robots = slices.DeleteFunc(s.Robots, func(id string) bool { return id == robotID })
			delete(s.meta.Since, robotID)
			dropped++
		}
	}
	if dropped == 0 {
		return
	}
	a.dirty = true
	a.log.Info("pairings removed on the manager", "robot", robotID, "browsers", dropped)
	if rb := a.robots[robotID]; rb != nil && rb.conn != nil {
		rb.conn.Frame(wire.KindPaired, a.paired(robotID))
		a.publishState(rb)
	}
}

// agentSummary turns a User-Agent into "Chrome on Android" and the like.
func agentSummary(ua string) string {
	browser := "A browser"
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"Chrome/", "Chrome"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	for _, o := range []struct{ token, name string }{
		{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iOS"}, {"CrOS", "ChromeOS"},
		{"Windows", "Windows"}, {"Mac OS X", "macOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			return browser + " on " + o.name
		}
	}
	return browser
}

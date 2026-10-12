package app

// Sign-in through the Stackchan manager (Config.SignIn, package sso from q41eu-raw): one
// sign-in for every app. With it, the owner's signed-in browsers get their robots' timers without
// the QR code. Anyone else pairs by the QR code, without an account.

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/i41-eu/q41eu-raw/sso"
)

const (
	ssoTriedCookie   = "focus_sso_tried" // the manager's hint a silent sign-in was tried with
	signInCheckEvery = time.Minute
)

// owns: the session's account owns the robot (the manager names its owner). a.mu held.
func (a *App) owns(s *session, rb *robot) bool {
	return a.cfg.SignIn != nil && s != nil && s.account != nil && rb.owner != "" && s.account.Key == rb.owner
}

// pairOwnedLocked gives a signed-in session the robots its account owns. a.mu held.
func (a *App) pairOwnedLocked(sid string) {
	s := a.sessions[sid]
	if s == nil || s.account == nil {
		return
	}
	for id, rb := range a.robots {
		if a.owns(s, rb) && !slices.Contains(s.Robots, id) {
			s.Robots = append(s.Robots, id)
			a.pairedNow(s, id)
			a.dirty = true
		}
	}
}

// setOwner records whose robot it is (from the manager; "" for the shared token) and gives it to
// the owner's signed-in browsers. a.mu held.
func (a *App) setOwnerLocked(id, owner string) {
	rb := a.robotFor(id)
	if rb.owner != owner {
		rb.owner = owner
		a.dirty = true
	}
	if a.cfg.SignIn == nil {
		return
	}
	for sid, s := range a.sessions {
		if s.account != nil {
			a.pairOwnedLocked(sid)
		}
	}
}

// GET /auth/login?next=/path: off to the manager.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.cfg.SignIn == nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, a.cfg.SignIn.LoginURL(a.ssoReturn(localPath(r.URL.Query().Get("next"))), false), http.StatusFound)
}

func (a *App) ssoReturn(next string) string {
	return a.cfg.PublicURL + "/auth/sso?next=" + url.QueryEscape(next)
}

// localPath is next if it is a path of this site, else "/".
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	return next
}

// trySignIn wraps a page: a browser not signed in here, but signed in at the manager lately (its
// hint cookie, sso.HintCookie; or any browser with SignInAlways), goes there silently once and
// comes back signed in.
func (a *App) trySignIn(page http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.SignIn == nil || r.URL.Query().Has("signin") {
			page(w, r)
			return
		}
		hint := sso.SilentTry(r, ssoTriedCookie, a.cfg.SignInAlways)
		if hint == "" {
			page(w, r)
			return
		}
		sid := a.sessionID(w, r)
		a.mu.Lock()
		s := a.sessions[sid]
		signedIn := s != nil && s.account != nil
		a.mu.Unlock()
		if signedIn {
			page(w, r)
			return
		}
		sso.MarkTried(w, ssoTriedCookie, hint, strings.HasPrefix(a.cfg.PublicURL, "https://"))
		http.Redirect(w, r, a.cfg.SignIn.LoginURL(a.ssoReturn(r.URL.RequestURI()), true), http.StatusFound)
	}
}

// GET /auth/sso?next=…&code=… (or &error=…): back from the manager.
func (a *App) handleSSOReturn(w http.ResponseWriter, r *http.Request) {
	if a.cfg.SignIn == nil {
		http.NotFound(w, r)
		return
	}
	sid := a.sessionID(w, r)
	q := r.URL.Query()
	next := localPath(q.Get("next"))
	if q.Get("code") == "" { // not signed in at the manager (silent), or cancelled
		u, _ := url.Parse(next)
		v := u.Query()
		v.Set("signin", "no")
		u.RawQuery = v.Encode()
		http.Redirect(w, r, u.String(), http.StatusSeeOther)
		return
	}
	ans, err := a.cfg.SignIn.Exchange(r.Context(), q.Get("code"))
	if err != nil || !ans.OK {
		if err != nil {
			a.log.Warn("sign-in failed", "err", err)
		}
		http.Error(w, "Sign-in failed, please try again.", http.StatusBadGateway)
		return
	}
	a.mu.Lock()
	s := a.sessionFor(sid)
	s.account, s.handle = ans.Account, ans.Handle
	a.pairOwnedLocked(sid)
	a.dirty = true
	a.mu.Unlock()
	a.log.Info("signed in")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// POST /auth/logout: here and, through the manager, in every app.
func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	sid := a.sessionID(w, r)
	a.mu.Lock()
	h := ""
	if s := a.sessions[sid]; s != nil {
		h = s.handle
		s.account, s.handle = nil, ""
		a.dirty = true
	}
	a.mu.Unlock()
	if a.cfg.SignIn != nil && h != "" {
		if err := a.cfg.SignIn.Logout(r.Context(), h); err != nil {
			a.log.Warn("sign-out at the manager", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"signed_in": false})
}

// RunSignInCheck asks the manager about every sign-in every minute, and ends those it ended.
func (a *App) RunSignInCheck(ctx context.Context) {
	if a.cfg.SignIn == nil {
		return
	}
	t := time.NewTicker(signInCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.checkSignIns(ctx)
		}
	}
}

func (a *App) checkSignIns(ctx context.Context) {
	a.mu.Lock()
	handles := map[string]string{}
	for sid, s := range a.sessions {
		if s.handle != "" {
			handles[sid] = s.handle
		}
	}
	a.mu.Unlock()
	for sid, h := range handles {
		acct, on, err := a.cfg.SignIn.Check(ctx, h)
		if err != nil {
			a.log.Warn("sign-in check", "err", err)
		}
		if on && acct != nil && acct.Key != "" {
			a.mu.Lock()
			if s := a.sessions[sid]; s != nil && s.handle == h && s.account != nil && *s.account != *acct {
				changed := s.account.Key != acct.Key
				fresh := *acct
				s.account = &fresh // e.g. the manager joined two sign-ins into one user
				if changed {
					a.pairOwnedLocked(sid)
				}
				a.dirty = true
			}
			a.mu.Unlock()
		}
		if !on {
			a.mu.Lock()
			if s := a.sessions[sid]; s != nil && s.handle == h {
				s.account, s.handle = nil, ""
				a.dirty = true
			}
			a.mu.Unlock()
		}
	}
}

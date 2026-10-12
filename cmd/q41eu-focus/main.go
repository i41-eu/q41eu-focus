// Command q41eu-focus is a focus timer (in the style of the Pomodoro Technique) for Stackchan
// robots in Embody Mode: the robot shows it, counts down, chimes; a phone paired by the robot's QR
// code runs it too, with statistics.
//
// Robots connect to ws://<host>/api/devices/connect with a bearer token (by default the same token
// file as q41eu-raw), or with a token of their own from a Stackchan manager (-manager-url,
// i41eu-manager), which this app checks with it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the statistics' days in -tz also in a container without zone files

	"github.com/i41-eu/q41eu-focus/internal/app"
	"github.com/i41-eu/q41eu-raw/robotauth"
	"github.com/i41-eu/q41eu-raw/sso"
	"github.com/i41-eu/q41eu-raw/statestore"
)

func main() {
	var (
		listen       = flag.String("listen", ":8775", "HTTP listen address for robots and browsers")
		publicURL    = flag.String("public-url", "", "base URL browsers use to reach this server (default: http://<LAN IP>:<port>)")
		tokenFile    = flag.String("token-file", defaultConfigFile("stackchan-server", "robot-token"), "file with the robot bearer token; generated if missing")
		stateFile    = flag.String("state-file", defaultStateFile(), "JSON file with the timers and pairings (\"\" keeps them in memory only)")
		stateDB      = flag.String("state-database", os.Getenv("STATE_DATABASE_URL"), "keep the state in Postgres instead (postgres://user@host/db; the password from PGPASSWORD); a -state-file that exists is imported once, into an empty database")
		tz           = flag.String("tz", "", "the time zone of the statistics' days, e.g. Europe/Prague (default: this machine's)")
		uiDir        = flag.String("ui-dir", "", "development: serve the page from this directory (internal/app/ui), so edits need only a reload")
		debug        = flag.Bool("debug", false, "debug logging")
		mgrURL       = flag.String("manager-url", "", "the Stackchan manager that set robots up with tokens for this app, e.g. https://m.i41.eu (\"\" = none)")
		mgrSecret    = flag.String("manager-secret-file", "", "file with this app's secret at the manager")
		mgrSignIn    = flag.Bool("manager-sign-in", false, "sign in through the manager (-manager-url): the owner's browsers get their robots without the QR code")
		signInAlways = flag.Bool("sign-in-always", false, "with -manager-sign-in: try a silent sign-in at the manager also without its hint cookie (this app on another domain than the manager's -sign-in-hint-domain), at most once per 10 minutes per browser")
	)
	flag.Parse()
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	fail := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
	token, err := loadOrCreateToken(*tokenFile, log)
	if err != nil {
		fail("robot token", err)
	}
	if *publicURL == "" {
		if *publicURL, err = defaultPublicURL(*listen); err != nil {
			fail("public URL", err)
		}
	}
	loc := time.Local
	if *tz != "" {
		if loc, err = time.LoadLocation(*tz); err != nil {
			fail("time zone", err)
		}
	}
	var manager *robotauth.Client
	var signIn *sso.Client
	if *mgrURL != "" {
		b, err := os.ReadFile(*mgrSecret)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			fail("manager secret file", fmt.Errorf("%s: %v", *mgrSecret, err))
		}
		manager = robotauth.New(*mgrURL, strings.TrimSpace(string(b)))
		if *mgrSignIn {
			signIn = sso.New(*mgrURL, strings.TrimSpace(string(b)))
		}
	} else if *mgrSignIn {
		fail("-manager-sign-in", errors.New("needs -manager-url and -manager-secret-file"))
	}
	var state statestore.Store
	if *stateDB != "" || *stateFile != "" {
		openCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute) // a database down for a moment (retried), or another copy's lock
		var from *statestore.File
		if *stateFile != "" {
			from = &statestore.File{Path: *stateFile}
		}
		st, err := statestore.Open(openCtx, *stateDB, "focus", from)
		cancel()
		if err != nil {
			fail("state", err)
		}
		state = st
		log.Info("state", "where", st.Where())
	}
	a := app.New(app.Config{RobotToken: token, Manager: manager, SignIn: signIn, SignInAlways: *signInAlways, PublicURL: strings.TrimRight(*publicURL, "/"),
		State: state, UIDir: *uiDir, Location: loc, Log: log})
	srv := &http.Server{Addr: *listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Run(ctx)
	if state != nil {
		go func() { // the database's lock is gone: another copy may write now; stop here
			select {
			case <-state.Lost():
				log.Error("state database: connection (and lock) lost; exiting")
				os.Exit(1)
			case <-ctx.Done():
			}
		}()
	}
	go a.RunSignInCheck(ctx)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Info("q41eu-focus listening", "listen", *listen, "public_url", *publicURL, "tz", loc.String(), "manager", *mgrURL)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail("server", err)
	}
	if err := a.Save(); err != nil {
		log.Warn("state not saved", "err", err)
	}
	if state != nil {
		state.Close()
	}
}

// defaultStateFile follows the XDG base directory spec: $XDG_STATE_HOME or ~/.local/state.
func defaultStateFile() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "q41eu-focus", "state.json")
}

// lookPath is the program's path, or "" when it is not installed.
// defaultCacheDir is name in $XDG_CACHE_HOME/q41eu-focus (or ~/.cache).
func defaultConfigFile(app, name string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return name
	}
	return filepath.Join(dir, app, name)
}

// loadOrCreateToken reads the robot token, generating a random one on first run.
func loadOrCreateToken(path string, log *slog.Logger) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		token := strings.TrimSpace(string(b))
		if token == "" {
			return "", fmt.Errorf("%s is empty", path)
		}
		return token, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	rand.Read(raw)
	token := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Info("generated new robot token", "path", path)
	return token, nil
}

// defaultPublicURL builds http://<LAN IP>:<port> so a phone on the same network can open the QR link.
func defaultPublicURL(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("parse -listen %q: %w", listen, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = lanIP()
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// lanIP returns the address of the interface used for outbound traffic (no packets are sent).
func lanIP() string {
	if c, err := net.Dial("udp4", "192.0.2.1:80"); err == nil {
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).IP.String()
	}
	return "127.0.0.1"
}

// photoDir keeps the leaderboard photos next to the state file ("" without one).

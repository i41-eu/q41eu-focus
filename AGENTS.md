# q41eu-focus

A focus timer (Pomodoro style) for Stackchan Embody Mode. See [readme.md](readme.md).

- Go only: the standard library plus gorilla/websocket (and pgx, only through q41eu-raw's `statestore`). The protocol comes from `github.com/i41-eu/q41eu-raw/wire`.
- Never call it "Pomodoro" in a name, a host or a title: Pomodoro® is Francesco Cirillo's registered trademark. Mention the technique only in prose, with the attribution (see the page's footer).
- `internal/timer` is pure (the caller passes the time): keep its rules there, with tests.
- The robot's screen is drawn in `internal/screen` (a 5x7 pixel font, no font files); sounds in `internal/sound` stay under 3 s (the robot's speaker buffer).
- Before committing, run `gofmt -l .`, `go vet ./...` and `go test -race ./...`.

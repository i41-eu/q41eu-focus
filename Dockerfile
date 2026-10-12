# syntax=docker/dockerfile:1

FROM docker.io/library/golang:1.26.8 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/q41eu-focus ./cmd/q41eu-focus

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/q41eu-focus /q41eu-focus
EXPOSE 8775
USER 65532:65532
# Mount the robot token at /secrets/robot-token and pass -public-url https://<host>; keep the state
# in Postgres (-state-database or STATE_DATABASE_URL) or on a volume (-state-file).
ENTRYPOINT ["/q41eu-focus", "-listen", ":8775", "-token-file", "/secrets/robot-token", "-state-file", ""]

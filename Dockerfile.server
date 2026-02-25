# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS builder
WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/multiscan-server ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /out/multiscan-server /usr/local/bin/multiscan-server

VOLUME ["/data"]
EXPOSE 8080

ENV SERVER_ADDR=:8080 \
    DB_PATH=/data/state.db \
    LEGACY_STATE_FILE=/data/state.json \
    LEASE_DURATION=2m \
    SYNC_WAIT=25s

ENTRYPOINT ["/usr/local/bin/multiscan-server"]

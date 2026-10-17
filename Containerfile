# Build: podman build -t slk .
# Run:   ./scripts/podman-run.sh   (see that script for volumes/env)
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o /out/slk ./cmd/slk

FROM docker.io/library/alpine:3.22
RUN apk add --no-cache ca-certificates tzdata ncurses-terminfo-base
COPY --from=build /out/slk /usr/local/bin/slk
ENV HOME=/home/slk \
    XDG_CONFIG_HOME=/home/slk/.config \
    XDG_DATA_HOME=/home/slk/.local/share \
    XDG_CACHE_HOME=/home/slk/.cache \
    LANG=C.UTF-8
WORKDIR /home/slk
ENTRYPOINT ["/usr/local/bin/slk"]

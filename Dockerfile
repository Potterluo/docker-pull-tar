# --- Stage 1: Build web UI (static export) ---------------------------------
FROM node:22-alpine AS web-builder
WORKDIR /src/web
# Pin pnpm so lockfile + build behavior stay reproducible.
RUN corepack enable && corepack prepare pnpm@10.15.0 --activate
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ .
RUN pnpm build

# --- Stage 2: Build the Go binary with the UI embedded ----------------------
FROM golang:1.25-alpine AS go-builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web-builder /src/web/out internal/server/dist
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w \
      -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE} \
      -X github.com/Potterluo/docker-pull-tar/internal/buildinfo.Version=${VERSION} \
      -X github.com/Potterluo/docker-pull-tar/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/Potterluo/docker-pull-tar/internal/buildinfo.Date=${DATE}" \
    -o /dockerpull ./cmd/server

# --- Stage 3: Minimal runtime -----------------------------------------------
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
COPY --from=go-builder /dockerpull /usr/local/bin/dockerpull

# Data directory for the SQLite database (override with APP_DATA_DIR).
ENV APP_DATA_DIR=/data
# Bind every interface: the process default is loopback, which inside a
# container means the published port (-p 8080:8080) answers nothing. A
# container exists to be reachable, so this is set here rather than left to
# the operator to discover.
ENV APP_BIND=all
RUN mkdir -p /data
VOLUME /data

EXPOSE 8080
ENTRYPOINT ["dockerpull"]

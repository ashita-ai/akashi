# syntax=docker/dockerfile:1

# Stage 1: Build frontend
FROM node:22-alpine AS ui-builder

WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --ignore-scripts
COPY ui/ .
RUN npm run build

# Stage 2: Build Go binary with embedded UI
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=ui-builder /ui/dist /src/ui/dist

RUN CGO_ENABLED=0 GOOS=linux go build -tags ui -trimpath -ldflags="-s -w" -o /akashi ./cmd/akashi

# Stage 3: Runtime
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget
RUN adduser -D -u 10001 akashi

WORKDIR /

COPY --from=builder /akashi /usr/local/bin/akashi
COPY migrations /migrations

USER akashi

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://localhost:${AKASHI_PORT:-8080}/health || exit 1

ENTRYPOINT ["akashi"]

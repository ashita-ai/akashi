# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /akashi ./cmd/akashi

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -u 10001 akashi

COPY --from=builder /akashi /usr/local/bin/akashi
COPY migrations /migrations

USER akashi

EXPOSE 8080

ENTRYPOINT ["akashi"]

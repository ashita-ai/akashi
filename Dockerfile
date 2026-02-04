# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /kyoyu ./cmd/kyoyu

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -u 10001 kyoyu

COPY --from=builder /kyoyu /usr/local/bin/kyoyu
COPY migrations /migrations

USER kyoyu

EXPOSE 8080

ENTRYPOINT ["kyoyu"]

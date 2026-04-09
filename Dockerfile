# syntax=docker/dockerfile:1.6

FROM golang:1.24-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./

RUN GOTOOLCHAIN=auto go mod download
COPY . .
# Build server binary
RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/marketflow-api ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /bin/marketflow-api /usr/local/bin/marketflow-api
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
USER app
EXPOSE 4000
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

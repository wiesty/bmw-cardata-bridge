FROM golang:1.24-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o bmw-bridge ./cmd/main.go

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /build/bmw-bridge .
VOLUME /data
EXPOSE 8080
ENV REST_PORT=8080
ENV POLL_INTERVAL_MINUTES=30
ENV DATA_DIR=/data
ENTRYPOINT ["./bmw-bridge"]

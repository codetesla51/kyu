
FROM golang:1.25 AS builder

WORKDIR /app


COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .


RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app/server ./cmd/example

FROM alpine:3.19

WORKDIR /app

COPY --from=builder /app/server .
RUN chmod +x /app/server

EXPOSE 8080

# Use exec form for proper signal handling
ENTRYPOINT ["./server"]


FROM golang:1.25 AS builder

WORKDIR /app


COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .


RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/server .

FROM alpine:3.19


WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/server .

EXPOSE 8080

# Use exec form for proper signal handling
ENTRYPOINT ["./server"]

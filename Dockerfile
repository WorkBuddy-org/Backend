# syntax=docker/dockerfile:1

FROM golang:1.25.14-alpine AS builder

WORKDIR /app

RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /server ./cmd/server \
    && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /migrate ./cmd/migrate

FROM alpine:3.21

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY --from=builder /server /app/server
# cmd/migrate reads ./migrations relative to the working directory.
COPY --from=builder /migrate /app/migrate
COPY migrations /app/migrations

EXPOSE 8080

USER nobody:nobody

ENTRYPOINT ["/app/server"]

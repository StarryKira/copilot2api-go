# Stage 1: Build frontend
FROM oven/bun:1-alpine AS frontend
WORKDIR /app/web
COPY web/package.json web/bun.lock* ./
RUN bun install --frozen-lockfile
COPY web/ ./
RUN bun run build

# Stage 2: Build Go binary (with embedded Copilot CLI)
FROM golang:1.25-alpine AS backend
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /app/web/dist ./web/dist
RUN go tool bundler
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o copilot-go .

# Stage 3: Runtime
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
  && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=backend /app/copilot-go .

EXPOSE 3000 4141
VOLUME /root/.local/share/copilot-api

ENTRYPOINT ["./copilot-go"]

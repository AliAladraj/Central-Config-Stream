# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src

# cache deps
COPY go.mod go.sum ./
RUN go mod download

# build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/central-config ./cmd/central-config
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/testconsole ./cmd/testconsole
# writable data dir for the SQLite test backend, owned by the runtime user
RUN mkdir -p /out/data && chown 65532:65532 /out/data

# ---- React UI for the test console ----
FROM node:20-alpine AS ui
WORKDIR /ui
COPY webui/package.json webui/package-lock.json ./
RUN npm ci
COPY webui/ ./
# vite.config.js writes the bundle to ../web
RUN npm run build

# ---- testconsole (local test harness only, never deployed) ----
FROM gcr.io/distroless/static-debian12:nonroot AS testconsole
WORKDIR /app
COPY --from=build /out/testconsole /app/testconsole
COPY --from=ui /web /app/web

USER nonroot:nonroot
EXPOSE 8090

ENTRYPOINT ["/app/testconsole"]

# ---- central-config (the service) — last, so a plain `docker build .` yields it ----
FROM gcr.io/distroless/static-debian12:nonroot AS central-config
WORKDIR /app
COPY --from=build /out/central-config /app/central-config
COPY --from=build --chown=nonroot:nonroot /out/data /data

# non-root (provided by the distroless nonroot image)
USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app/central-config"]

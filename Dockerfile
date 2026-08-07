# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src

# cache deps
COPY go.mod go.sum ./
RUN go mod download

# Build identity. The defaults match what an unstamped `go build` leaves in
# internal/buildinfo, so an image built without --build-arg still reports
# something honest instead of an empty version. .dockerignore keeps .git out of
# the build context, which is why these are arguments rather than git commands:
#   docker build --build-arg VERSION=$(git describe --tags --always --dirty) .
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# source
COPY . .
# -s -w strips the symbol and DWARF tables; -X writes the identity above into
# the package that both binaries print from. Set here rather than in the two
# stages below so the flags cannot drift between the service and the console.
ENV LDFLAGS="-s -w \
  -X github.com/ErasedKyte/Central-Config-Stream/internal/buildinfo.Version=${VERSION} \
  -X github.com/ErasedKyte/Central-Config-Stream/internal/buildinfo.Commit=${COMMIT} \
  -X github.com/ErasedKyte/Central-Config-Stream/internal/buildinfo.Date=${DATE}"

# ---- one compile per stage, so each target builds only what it ships ----
# These were both RUN lines in `build` above, which meant the release image
# compiled the console and then threw it away — ~14s of every release spent on a
# binary that is never deployed. BuildKit builds only the stages the requested
# target depends on, so `--target central-config` now reaches build-service and
# stops.

FROM build AS build-service
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="$LDFLAGS" -o /out/central-config ./cmd/central-config
# writable data dir for the SQLite test backend, owned by the runtime user
RUN mkdir -p /out/data && chown 65532:65532 /out/data

FROM build AS build-console
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="$LDFLAGS" -o /out/testconsole ./cmd/testconsole

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
COPY --from=build-console /out/testconsole /app/testconsole
COPY --from=ui /web /app/web

# Re-declared: an ARG belongs to the stage that declares it, and these are the
# labels a registry, an SBOM tool or `docker inspect` reads.
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="testconsole" \
      org.opencontainers.image.source="https://github.com/ErasedKyte/Central-Config-Stream" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"

USER nonroot:nonroot
EXPOSE 8090

ENTRYPOINT ["/app/testconsole"]

# ---- central-config (the service) — last, so a plain `docker build .` yields it ----
FROM gcr.io/distroless/static-debian12:nonroot AS central-config
WORKDIR /app
COPY --from=build-service /out/central-config /app/central-config
COPY --from=build-service --chown=nonroot:nonroot /out/data /data

ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="central-config" \
      org.opencontainers.image.source="https://github.com/ErasedKyte/Central-Config-Stream" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.licenses="MIT"

# non-root (provided by the distroless nonroot image)
USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/app/central-config"]

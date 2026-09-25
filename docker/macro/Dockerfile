# All-in-one self-hosted macro backend.
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY pkg/ pkg/
RUN CGO_ENABLED=0 go build -o /out/macro ./cmd/macro

FROM debian:bookworm-slim
# ffmpeg for call_recording_preview; libreoffice-nogui for the convert
# consumer (soffice --headless); ca-certificates/curl for TLS + healthcheck.
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg ca-certificates curl libreoffice-nogui \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 -m macro
COPY --from=build /out/macro /usr/local/bin/macro
COPY crates/macro_db_client/migrations /app/migrations
ENV MIGRATIONS_DIR=/app/migrations
WORKDIR /app
USER macro
HEALTHCHECK --interval=30s --timeout=3s \
  CMD curl -fsS http://localhost:8080/health || exit 1
ENTRYPOINT ["macro"]

# syntax=docker/dockerfile:1.7
# One statically configured image; KB_ROLE selects the services a container runs.

FROM rust:1.94-bookworm AS loro
WORKDIR /src/rust/loro-cabi
COPY rust/loro-cabi/Cargo.toml rust/loro-cabi/Cargo.lock ./
COPY rust/loro-cabi/src ./src
RUN cargo build --release

FROM node:22-bookworm AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json* ./
RUN if [ -f package.json ]; then npm ci; fi
COPY web ./
RUN if [ -f package.json ]; then npm run build; fi

FROM golang:1.26-bookworm AS build
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=loro /src/rust/loro-cabi/target/release/libloro_cabi.a rust/loro-cabi/target/release/libloro_cabi.a
COPY --from=web /src/web/dist internal/web/dist
RUN CGO_ENABLED=1 go build -trimpath \
      -ldflags "-X github.com/acx1729/ocean/internal/version.Version=${VERSION} -X github.com/acx1729/ocean/internal/version.Commit=${COMMIT}" \
      -o /out/kb ./cmd/kb

FROM gcr.io/distroless/base-debian12:nonroot
COPY --from=build /out/kb /kb
VOLUME ["/var/lib/kb"]
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/kb"]

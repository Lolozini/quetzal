# syntax=docker/dockerfile:1

# Base images are pinned by digest, so a rebuild uses the same bytes; Dependabot
# moves tag and digest together.

# 1) Build the React UI.
FROM node:22-alpine@sha256:0a7108bf6c7bf5de370ffb1a3ed6be93d405b43ff159f681a8d18c0e2bc2e402 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# 2) Build the Go binaries (apiserver embeds the UI built above).
FROM golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ENV GOTOOLCHAIN=local CGO_ENABLED=0
# Build metadata stamped into the binaries (see internal/version).
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /web/dist ./web/dist
RUN LDFLAGS="-s -w \
 -X github.com/lolozini/quetzal/internal/version.Version=${VERSION} \
 -X github.com/lolozini/quetzal/internal/version.Commit=${COMMIT} \
 -X github.com/lolozini/quetzal/internal/version.Date=${DATE}" \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/quetzal-apiserver ./cmd/apiserver \
 && go build -trimpath -ldflags "$LDFLAGS" -o /out/quetzal-controller ./cmd/controller \
 && go build -trimpath -o /out/quetzal-activator ./cmd/activator \
 && go build -trimpath -o /out/quetzal-configrender ./cmd/configrender \
 && go build -trimpath -o /out/quetzal-sftp ./cmd/sftp

# 3) Minimal runtime image.
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/quetzal-apiserver /usr/local/bin/quetzal-apiserver
COPY --from=build /out/quetzal-controller /usr/local/bin/quetzal-controller
COPY --from=build /out/quetzal-activator /usr/local/bin/quetzal-activator
COPY --from=build /out/quetzal-configrender /usr/local/bin/quetzal-configrender
COPY --from=build /out/quetzal-sftp /usr/local/bin/quetzal-sftp
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/quetzal-apiserver"]

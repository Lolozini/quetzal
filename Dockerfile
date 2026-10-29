# syntax=docker/dockerfile:1

# Base images are pinned by digest, so a rebuild uses the same bytes; Dependabot
# moves tag and digest together.

# The image is built for amd64 and arm64. The two build stages run on the
# builder's own platform ($BUILDPLATFORM) and Go cross-compiles for the target,
# so nothing is emulated; the last stage only copies files.

# 1) Build the React UI (the same static files on every platform).
FROM --platform=$BUILDPLATFORM node:24-alpine@sha256:ebfe2f90462722a7a4de65e91990e97fe0d401c70e0e762c5b53302f905ec1c1 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# 2) Build the Go binaries (apiserver embeds the UI built above).
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
ARG TARGETOS TARGETARCH
ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
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
FROM gcr.io/distroless/static:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99
COPY --from=build /out/quetzal-apiserver /usr/local/bin/quetzal-apiserver
COPY --from=build /out/quetzal-controller /usr/local/bin/quetzal-controller
COPY --from=build /out/quetzal-activator /usr/local/bin/quetzal-activator
COPY --from=build /out/quetzal-configrender /usr/local/bin/quetzal-configrender
COPY --from=build /out/quetzal-sftp /usr/local/bin/quetzal-sftp
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/quetzal-apiserver"]

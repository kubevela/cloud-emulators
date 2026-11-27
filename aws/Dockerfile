# An unofficial image of AWS Emulator (https://github.com/aws/aws-emulator) for use
# inside a test cluster or a CI job: the emulator plus a launcher that offers it as
# plain HTTP. See aws/launcher/main.go for why.
#
# Build from the repository root, with AWS's packages already downloaded into aws/
# (aws/build.sh does both):
#   docker build -f aws/Dockerfile --build-arg VERSION=<version> .
#
# Every base image is pinned by digest; Dependabot proposes the updates.

# The launcher is a static Go binary, cross-compiled on the build machine.
FROM --platform=$BUILDPLATFORM golang:1.26-trixie@sha256:cff05dd6b81c901f2e8249eb6f91c69a60451d0f12c26bfef08e476a85b32d4d AS launcher
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY aws/launcher ./aws/launcher
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/launcher ./aws/launcher

# AWS's package is unpacked on the build machine; nothing from it runs during the build.
FROM --platform=$BUILDPLATFORM debian:trixie-slim@sha256:8aa5a7bf5a1955bddbe7d74fcdd0b91ab30e74362a8b73af672b0481b35b44a0 AS unpack
ARG TARGETARCH
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends rpm2cpio cpio xz-utils zstd \
    && rm -rf /var/lib/apt/lists/*
COPY aws/aws-emulator-${TARGETARCH}.rpm /tmp/aws-emulator.rpm
WORKDIR /unpacked
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
RUN rpm2cpio /tmp/aws-emulator.rpm | cpio -idm --quiet

# The final image has no shell and no package manager: the C library the emulator
# needs, CA certificates, the emulator and the launcher. It runs as a non-root user.
FROM gcr.io/distroless/base-debian13:nonroot@sha256:a08d9054afbaf1af9b90fd02cdc7b32236319b5376d374505a41548d42b0c5db
ARG VERSION=unknown
LABEL org.opencontainers.image.title="aws-emulator" \
      org.opencontainers.image.description="Unofficial build of AWS Emulator with a plain-HTTP launcher, for test clusters and CI" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/kubevela/cloud-emulators" \
      org.opencontainers.image.url="https://github.com/aws/aws-emulator" \
      org.opencontainers.image.licenses="CC0-1.0 AND Apache-2.0"
COPY --from=unpack /unpacked/opt/aws-emulator /opt/aws-emulator
# The fidelity report AWS published for this release, and the packages' checksums.
COPY aws/release-info/ /opt/aws-emulator/release-info/
COPY --from=launcher /out/launcher /usr/local/bin/launcher
USER 65532:65532
ENV HOME=/home/nonroot \
    EMULATOR_TELEMETRY=off \
    EMULATOR_AUTH=off \
    EMULATOR_HTTP_PORT=4566
EXPOSE 4566
ENTRYPOINT ["/usr/local/bin/launcher"]

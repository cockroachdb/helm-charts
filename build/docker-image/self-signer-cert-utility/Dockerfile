FROM golang:1.18.2 as base

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG COCKROACH_VERSION

WORKDIR /

# Environment to build the go binary
ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=$TARGETOS \
    GOARCH=$TARGETARCH

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# Download the go dependencies
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

# Build the binary self-signer utility
RUN go build -o self-signer cmd/main.go

# Install the cockroach binary
RUN if [ "$TARGETPLATFORM" = "linux/amd64" ]; then GOARCH=amd64; elif [ "$TARGETPLATFORM" = "linux/arm64" ]; then GOARCH=arm64; else GOARCH=amd64; fi && \
    curl -sS -L -O https://binaries.cockroachdb.com/cockroach-v${COCKROACH_VERSION}.linux-${GOARCH}.tgz && \
    tar -zxvf cockroach-v${COCKROACH_VERSION}.linux-${GOARCH}.tgz && \
    # Renaming the folder to common name so that the final image doesn't have folder name based on architecture and version
    mv cockroach-v${COCKROACH_VERSION}.linux-${GOARCH} cockroach-binary

FROM registry.access.redhat.com/ubi8/ubi-minimal:latest as final
LABEL name=self-signer
LABEL vendor="Cockroach Labs"
LABEL summary="CockroachDB is a distributed SQL database"
LABEL description="CockroachDB is a PostgreSQL wire-compatible distributed SQL database"

WORKDIR /

COPY --from=base /self-signer /self-signer
COPY --from=base /cockroach-binary/cockroach /usr/local/bin/
RUN chmod +x /self-signer
USER 1001
ENTRYPOINT ["/self-signer"]

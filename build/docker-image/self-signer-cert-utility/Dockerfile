FROM golang:1.18.2 as base

WORKDIR /

# Environment to build the go binary
ENV GO111MODULE=on \
    CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# Download the go dependencies
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

# Build the binary self-signer utility
RUN go build -a -installsuffix cgo -o self-signer cmd/main.go

# Install the cockroach CLI
ADD https://binaries.cockroachdb.com/cockroach-v23.1.2.linux-amd64.tgz .
RUN tar -zxvf cockroach-v23.1.2.linux-amd64.tgz

FROM registry.access.redhat.com/ubi7/ubi-minimal:latest as final
LABEL name=self-signer
LABEL vendor="Cockroach Labs"
LABEL summary="CockroachDB is a distributed SQL database"
LABEL description="CockroachDB is a PostgreSQL wire-compatible distributed SQL database"

WORKDIR /

COPY --from=base /self-signer /self-signer
COPY --from=base /cockroach-v23.1.2.linux-amd64/cockroach /usr/local/bin/
RUN chmod +x /self-signer
USER 1001
ENTRYPOINT ["/self-signer"]

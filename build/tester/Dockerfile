FROM quay.io/helmpack/chart-testing:v3.3.1

COPY --from=golang:1.15-alpine /usr/local/go/ /usr/local/go/
ENV PATH="/usr/local/go/bin:${PATH}"

# Install gcc.
RUN apk add build-base

RUN mkdir -p /charts

WORKDIR /charts/

COPY ./go.mod ./go.sum .
RUN go mod download

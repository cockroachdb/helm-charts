# syntax=docker/dockerfile:1
FROM --platform=linux/amd64 quay.io/operator-framework/helm-operator:v1.27

ENV HOME=/opt/helm
COPY ./build/olm-catalog/watches.yaml ${HOME}/watches.yaml
COPY ./cockroachdb/ ${HOME}/helm-charts
WORKDIR ${HOME}
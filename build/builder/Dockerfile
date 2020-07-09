FROM debian:10.4-slim

RUN apt-get update && \
        apt-get install --yes curl gnupg awscli

# Install helm
RUN curl https://helm.baltorepo.com/organization/signing.asc | apt-key add - && \
        apt-get install apt-transport-https --yes && \
        echo "deb https://baltocdn.com/helm/stable/debian/ all main" | tee /etc/apt/sources.list.d/helm-stable-debian.list && \
        apt-get update && \
        apt-get install --yes helm

RUN mkdir -p /charts

WORKDIR /charts/

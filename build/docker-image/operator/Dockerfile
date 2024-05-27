FROM quay.io/operator-framework/helm-operator:v1.27

ENV HOME=/opt/helm
COPY ./build/docker-image/operator/watches.yaml ${HOME}/watches.yaml

# temporarily set user as 0 for the mkdir command to run and then set back to default user
USER 0
RUN mkdir -p ${HOME}/helm-charts/cockroachdb
USER $CONTAINER_USER_ID

COPY ./cockroachdb/ ${HOME}/helm-charts/cockroachdb
WORKDIR ${HOME}

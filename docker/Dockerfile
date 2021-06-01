FROM docker.io/fluxcd/helm-operator:1.2.0
COPY ./helm-operator /usr/local/bin/
ENTRYPOINT [ "/sbin/tini", "--", "helm-operator" ]

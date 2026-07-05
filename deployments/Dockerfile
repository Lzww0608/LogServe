# syntax=docker/dockerfile:1.7

# Shared runtime image for logd, control, workers, and logservectl. The first
# stage builds Go entrypoints, while the final Python image carries the
# executor runtime needed by worker processes.
FROM golang:1.25-bookworm AS build

WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=$GOPROXY
ENV GOSUMDB=$GOSUMDB

COPY go.mod go.sum ./
# BuildKit cache mounts keep module downloads out of the final image and make
# repeated local Compose builds fast without changing the image contents.
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -o /out/logserve-logd ./cmd/logserve-logd
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -o /out/logserve-control ./cmd/logserve-control
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -o /out/logserve-worker ./cmd/logserve-worker
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -o /out/logservectl ./cmd/logservectl

FROM python:3.11-slim

# The runtime runs as a fixed non-root uid shared with the Kubernetes manifests;
# writable directories are created explicitly because services later use a
# read-only root filesystem in Compose and k8s.
RUN adduser --system --uid 10001 --group logserve \
    && mkdir -p /app /var/lib/logserve/logstore /tmp/logserve-objectstore \
    && chown -R logserve:logserve /app /var/lib/logserve /tmp/logserve-objectstore

WORKDIR /app
COPY --from=build /out/logserve-logd /usr/local/bin/logserve-logd
COPY --from=build /out/logserve-control /usr/local/bin/logserve-control
COPY --from=build /out/logserve-worker /usr/local/bin/logserve-worker
COPY --from=build /out/logservectl /usr/local/bin/logservectl
COPY executor/python/requirements.txt /tmp/logserve-executor-requirements.txt
RUN pip install --no-cache-dir -r /tmp/logserve-executor-requirements.txt \
    && rm /tmp/logserve-executor-requirements.txt
COPY executor /app/executor
COPY sdk /app/sdk
RUN chown -R logserve:logserve /app

ENV PYTHONPATH=/app/sdk/python
# No CMD is declared here because Compose and Kubernetes choose the specific
# logd/control/worker binary through service-level commands or args.
USER logserve
# syntax=docker/dockerfile:1.7

FROM golang:1.25-bookworm AS build

WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=$GOPROXY
ENV GOSUMDB=$GOSUMDB

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -o /out/logserve-logd ./cmd/logserve-logd
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -o /out/logserve-control ./cmd/logserve-control
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -o /out/logserve-worker ./cmd/logserve-worker
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build go build -o /out/logservectl ./cmd/logservectl

FROM python:3.11-slim

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
USER logserve
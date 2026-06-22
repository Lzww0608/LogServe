FROM golang:1.25-bookworm AS build

WORKDIR /src
COPY . .
RUN go build -o /out/logserve-logd ./cmd/logserve-logd
RUN go build -o /out/logserve-control ./cmd/logserve-control
RUN go build -o /out/logserve-worker ./cmd/logserve-worker
RUN go build -o /out/logservectl ./cmd/logservectl

FROM python:3.11-slim

RUN adduser --system --uid 10001 --group logserve \
    && mkdir -p /app /var/lib/logserve/logstore /tmp/logserve-objectstore \
    && chown -R logserve:logserve /app /var/lib/logserve /tmp/logserve-objectstore

WORKDIR /app
COPY --from=build /out/logserve-logd /usr/local/bin/logserve-logd
COPY --from=build /out/logserve-control /usr/local/bin/logserve-control
COPY --from=build /out/logserve-worker /usr/local/bin/logserve-worker
COPY --from=build /out/logservectl /usr/local/bin/logservectl
COPY executor /app/executor
COPY sdk /app/sdk
RUN chown -R logserve:logserve /app

ENV PYTHONPATH=/app/sdk/python
USER logserve

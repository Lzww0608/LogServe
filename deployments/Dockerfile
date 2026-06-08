FROM golang:1.25-bookworm AS build

WORKDIR /src
COPY . .
RUN go build -o /out/logserve-logd ./cmd/logserve-logd
RUN go build -o /out/logserve-control ./cmd/logserve-control
RUN go build -o /out/logserve-worker ./cmd/logserve-worker
RUN go build -o /out/logservectl ./cmd/logservectl

FROM python:3.11-slim

WORKDIR /app
COPY --from=build /out/logserve-logd /usr/local/bin/logserve-logd
COPY --from=build /out/logserve-control /usr/local/bin/logserve-control
COPY --from=build /out/logserve-worker /usr/local/bin/logserve-worker
COPY --from=build /out/logservectl /usr/local/bin/logservectl
COPY executor /app/executor
COPY sdk /app/sdk
ENV PYTHONPATH=/app/sdk/python

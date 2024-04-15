FROM golang:1.19 AS builder

COPY . /builder
WORKDIR /builder

RUN make build

FROM alpine:3

WORKDIR /app
RUN ls 

EXPOSE 8080
COPY --from=builder /builder/bin .
COPY config/config.yaml /app/config.yaml

ENTRYPOINT ["/app/azure-openai-proxy"]
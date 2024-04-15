# Build stage
FROM golang:1.19 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build

# Final stage
FROM alpine:3
WORKDIR /app
COPY --from=builder /app/bin/azure-openai-proxy /app/
COPY --from=builder /app/config/config.yaml /app/config.yaml
EXPOSE 8080
ENTRYPOINT ["/app/azure-openai-proxy"]
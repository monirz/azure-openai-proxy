# Use a multi-stage build for efficiency
FROM golang:1.19 AS builder

# Set working directory and copy project code
WORKDIR /app
COPY go.mod go.sum ./

# Install dependencies (assuming go.mod is present)
RUN go mod download

# Build the binary
RUN go build -o azure-openai-proxy .

# Switch to a lightweight base image
FROM alpine:3

# Set working directory for the final image
WORKDIR /app

# Copy the binary and configuration file
COPY --from=builder /app/azure-openai-proxy .
COPY config/config.yaml ./config.yaml

# Expose port for the application
EXPOSE 8080

# Entrypoint for the application
ENTRYPOINT ["/app/azure-openai-proxy"]
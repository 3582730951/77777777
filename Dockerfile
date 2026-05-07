FROM node:22-bookworm-slim AS autoreg-spa
WORKDIR /src/services/autoreg/frontend
COPY services/autoreg/frontend/package*.json ./
RUN npm ci
COPY services/autoreg/frontend/ ./
RUN VITE_API_BASE=/api/autoreg npx vite build --base=/autoreg/ --outDir /out/autoreg_spa

FROM golang:1.25-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=autoreg-spa /out/autoreg_spa ./internal/admin/autoreg_spa
RUN CGO_ENABLED=1 go build -o /gateway ./cmd/gateway/

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git jq nodejs npm python3 python3-pip python3-venv rsync tar unzip wget \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /gateway /usr/local/bin/gateway
COPY --from=builder /src /workspace
WORKDIR /workspace
EXPOSE 8787 8788
CMD ["gateway"]

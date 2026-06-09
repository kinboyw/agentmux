FROM node:22-bookworm AS web
WORKDIR /src/web/control
COPY web/control/package*.json ./
RUN npm ci
COPY web/control ./
RUN npm run build

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/control/dist ./internal/hub/webdist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/agentmux ./cmd/agentmux

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /var/lib/agentmux
COPY --from=build /out/agentmux /usr/local/bin/agentmux
EXPOSE 8080
VOLUME ["/var/lib/agentmux"]
ENTRYPOINT ["/usr/local/bin/agentmux"]
CMD ["hub", "--addr", "0.0.0.0:8080", "--data", "/var/lib/agentmux/agentmux.db"]

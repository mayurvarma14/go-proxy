# syntax=docker/dockerfile:1.7

FROM golang:1.24-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/go-proxy ./cmd/proxy

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=build /out/go-proxy /app/go-proxy
COPY config/dev.yaml /app/config/dev.yaml

USER nonroot:nonroot
EXPOSE 8080 8443 9901

ENTRYPOINT ["/app/go-proxy"]
CMD ["--config", "/app/config/dev.yaml"]

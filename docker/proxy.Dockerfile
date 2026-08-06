# syntax=docker/dockerfile:1
#
# Build with the repository root as context.

FROM golang:1.26-alpine AS build
WORKDIR /src

# Dependency layer: rebuilt only when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/proxy ./cmd/proxy

FROM alpine:3.22
RUN adduser -D -u 10001 app
COPY --from=build /out/proxy /usr/local/bin/proxy

USER app
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/proxy"]

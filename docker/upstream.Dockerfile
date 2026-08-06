# syntax=docker/dockerfile:1
#
# Build with the repository root as context.

FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/upstream ./cmd/upstream

FROM alpine:3.22
RUN adduser -D -u 10001 app
COPY --from=build /out/upstream /usr/local/bin/upstream

USER app
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/upstream"]

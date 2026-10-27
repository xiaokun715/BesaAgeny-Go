# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/besa ./cmd/besa

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 besa
WORKDIR /app
COPY --from=build /out/besa /usr/local/bin/besa
COPY configs /app/configs
USER besa
EXPOSE 8080 9001 9002 9003 9004 9005 9006 9007 9008
ENTRYPOINT ["/usr/local/bin/besa"]
CMD ["--env", "prod", "serve", "--role", "all"]

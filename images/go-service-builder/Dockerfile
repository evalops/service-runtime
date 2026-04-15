FROM golang:1.26-alpine

RUN apk add --no-cache ca-certificates git

ENV GOPRIVATE=github.com/evalops/* \
    GONOPROXY=github.com/evalops/* \
    GONOSUMDB=github.com/evalops/* \
    GOPROXY=https://proxy.golang.org,direct

WORKDIR /src

FROM golang:1.26-alpine

RUN apk add --no-cache ca-certificates git

ENV GOPRIVATE=github.com/evalops/* \
    GONOSUMDB=github.com/evalops/* \
    GOPROXY=direct

WORKDIR /src

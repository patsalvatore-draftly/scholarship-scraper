# syntax=docker/dockerfile:1
FROM golang:alpine AS builder

WORKDIR /opt/app
# add gcc for CGO

RUN apk add build-base
COPY . .

RUN go build -o main

FROM alpine:latest
WORKDIR /opt
# copy over the binaries we built in the last step
COPY --from=builder /opt/app ./
CMD ["./main"]
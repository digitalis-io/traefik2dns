FROM golang:1.20-buster as builder

RUN mkdir -p $GOPATH/src/github.com/digitalis-io/traefik2dns
ADD . $GOPATH/src/github.com/digitalis-io/traefik2dns
WORKDIR $GOPATH/src/github.com/digitalis-io/traefik2dns

RUN GO111MODULE=on go build main.go && ls -l && echo $GOPATH

FROM debian:buster as ca-store
RUN apt-get update && apt-get install -y ca-certificates

FROM debian:buster
RUN mkdir /app
COPY --from=builder /go/src/github.com/digitalis-io/traefik2dns/main /app/
COPY --from=ca-store /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
WORKDIR /app
CMD ["./main"]
FROM golang:1.20-bookworm as builder

WORKDIR /opt/

COPY go.mod go.sum ./
RUN go mod download

COPY ./src ./

ENV GOOS linux
ENV CGO_ENABLED=0

RUN mkdir /opt/goiardi
RUN go build -v -o goiardi .

FROM golang:1.20-bookworm

RUN mkdir -p /go/src/github.com/ctdk/goiardi && \
    mkdir -p /etc/goiardi && \
    mkdir -p /var/lib/goiardi/lfs

RUN addgroup --gid 1000 goiardi && \
    adduser -u 1000 -D -G goiardi goiardi -h /goiardi

WORKDIR /goiardi/

#RUN apk --no-cache add ca-certificates
COPY --from=builder /opt/goiardi /usr/local/bin/
COPY ./etc/docker-goiardi.conf /etc/goiardi/goiardi.conf

#USER goiardi
EXPOSE 4545
CMD ["/usr/local/bin/goiardi", "-c", "/etc/goiardi/goiardi.conf"]


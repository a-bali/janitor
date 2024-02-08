FROM golang:alpine as builder

COPY . /build/janitor
WORKDIR /build/janitor

RUN apk --no-cache add make git
RUN make static_build

FROM alpine:latest
RUN apk --no-cache add tzdata
WORKDIR /janitor
COPY --from=builder /build/janitor/janitor ./
EXPOSE 8080
ENTRYPOINT ["/janitor/janitor", "/janitor/config.yml"]

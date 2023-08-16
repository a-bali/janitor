FROM golang:latest as builder

COPY . /build/janitor
WORKDIR /build/janitor

RUN make static_build

FROM alpine:latest
RUN apk --no-cache add tzdata
WORKDIR /janitor
COPY --from=builder /build/janitor/janitor ./
EXPOSE 8080
ENTRYPOINT ["/janitor/janitor", "/janitor/config.yml"]

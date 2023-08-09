FROM golang:latest as builder

WORKDIR /app
COPY main.go ./
COPY go.mod ./
COPY go.sum ./
COPY templates ./templates

RUN CGO_ENABLED=0 GOOS=linux go build

FROM alpine:latest
RUN apk --no-cache add tzdata
WORKDIR /janitor
COPY --from=builder /app/janitor ./
EXPOSE 8080
ENTRYPOINT ["/janitor/janitor", "/janitor/config.yml"]

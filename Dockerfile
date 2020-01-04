FROM golang:latest as builder

RUN go get github.com/eclipse/paho.mqtt.golang
RUN go get github.com/go-telegram-bot-api/telegram-bot-api
RUN go get gopkg.in/yaml.v2
RUN go get github.com/gobuffalo/packr/packr
RUN go get github.com/gobuffalo/packr

WORKDIR /app
COPY main.go ./
COPY templates ./templates

RUN packr
RUN CGO_ENABLED=0 GOOS=linux go build

FROM alpine:latest
WORKDIR /janitor
COPY --from=builder /app/app ./
ENTRYPOINT ["/janitor/app", "/janitor/config.yml"]

FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /arr-telegram-bot .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /arr-telegram-bot /arr-telegram-bot
USER 65532:65532
ENTRYPOINT ["/arr-telegram-bot"]

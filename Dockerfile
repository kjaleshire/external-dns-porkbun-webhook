FROM golang:1.22-alpine3.20 as builder
WORKDIR /app
COPY . /app

RUN apk --no-cache add make git && make build

FROM alpine:3.20

COPY --from=builder /app/external-dns-porkbun-webhook /
COPY --from=builder /app/docker/entrypoint.sh /
ENTRYPOINT ["/entrypoint.sh"]

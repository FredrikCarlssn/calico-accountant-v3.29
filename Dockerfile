FROM golang:1.22-alpine as builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o calico-accountant .

FROM alpine:latest
LABEL maintainer="Fredrik Carlsson"
WORKDIR /root/
RUN apk --no-cache add iptables ip6tables iptables-legacy
COPY --from=builder /app/calico-accountant /calico-accountant
ENTRYPOINT ["/calico-accountant"]

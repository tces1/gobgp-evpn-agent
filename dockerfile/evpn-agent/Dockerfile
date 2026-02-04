# ---- deps: download modules with network access once
FROM golang:1.21-alpine AS deps
WORKDIR /src
RUN apk add --no-cache \
    --repository=https://mirrors.aliyun.com/alpine/v3.20/main \
    --repository=https://mirrors.aliyun.com/alpine/v3.20/community \
    git
ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=sum.golang.google.cn \
    GOWORK=off
COPY go.mod go.sum ./
RUN go mod download

# ---- builder: compile
FROM deps AS builder
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/evpn-agent ./cmd/evpn-agent

# ---- runtime
FROM alpine:3.20
RUN apk add --no-cache \
    --repository=https://dl-cdn.alpinelinux.org/alpine/v3.20/main \
    --repository=https://dl-cdn.alpinelinux.org/alpine/v3.20/community \
    iproute2
COPY --from=builder /out/evpn-agent /usr/local/bin/evpn-agent
ENTRYPOINT ["/usr/local/bin/evpn-agent"]
CMD ["-config=/etc/evpn-agent/config.yaml"]

FROM golang:1.22-alpine AS builder
WORKDIR /src
ENV GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
COPY go.mod main.go index.html ./
RUN go mod tidy && CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /app/server .

FROM alpine:3.20
WORKDIR /app
# 安装时区数据与开源中文字体（如果自己挂载 ./fonts/*.ttf，可删掉 font-noto-cjk 让镜像缩小到 ~15MB）
RUN apk add --no-cache tzdata font-noto-cjk
COPY --from=builder /app/server /app/server
ENV TZ=Asia/Shanghai
ENV GOMEMLIMIT=128MiB
ENV ADDR=:8080
ENV OUTPUT_DIR=/output
ENV DATA_DIR=/data
ENV FONTS_DIR=/fonts
EXPOSE 8080
VOLUME ["/output", "/data", "/fonts"]
ENTRYPOINT ["/app/server"]

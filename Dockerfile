# 构建：纯 Go（三种数据库驱动都不需要 CGO）
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ccd .

# 运行
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/ccd /usr/local/bin/ccd
# 默认以 root 运行：用 SQLite 时 ccd 要能写 new-api 数据目录里的 -shm 文件（只读查询也需要）。
ENV CCD_LISTEN=0.0.0.0:3140 \
    CCD_STATE=/var/lib/ccd
VOLUME /var/lib/ccd
EXPOSE 3140
ENTRYPOINT ["/usr/local/bin/ccd"]

# --- Сборка ---
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
# Резолвим зависимости и генерируем go.sum (требуется сеть при сборке).
RUN go mod tidy
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- Рантайм ---
FROM alpine:3.20
# git — для синхронизации, ca-certificates — HTTPS, openssh — SSH-репо,
# su-exec — сброс привилегий с root до app в entrypoint.
RUN apk add --no-cache git ca-certificates openssh-client tzdata su-exec
RUN adduser -D -u 10001 app
COPY --from=build /out/server /usr/local/bin/server
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV OSA_DATA_DIR=/data \
    OSA_WORK_DIR=/work \
    OSA_ADDR=:8080
RUN mkdir -p /data /work && chown app:app /data /work
EXPOSE 8080
# Стартуем под root (для подготовки ключа и прав), entrypoint сам сбрасывает в app.
ENTRYPOINT ["/entrypoint.sh"]

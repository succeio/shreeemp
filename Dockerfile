# --- Этап 1: Сборка приложения ---
FROM golang:1.26-alpine AS builder

WORKDIR /build

# Копируем файлы зависимостей
COPY go.mod go.sum ./
RUN go mod download

# Копируем весь исходный код
COPY . .

# Собираем бинарник для Linux без CGO (чтобы работал в чистом alpine)
# Флаг -o указывает имя выходного файла
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o shreeemp-server .

# --- Этап 2: Финальный легковесный образ ---
FROM alpine:3.19

WORKDIR /app

# Устанавливаем таймзоны и базовые сертификаты (на случай работы с внешними API)
RUN apk --no-cache add ca-certificates tzdata

# Копируем собранный бинарник из предыдущего этапа
COPY --from=builder /build/shreeemp-server .

# Создаем папки для монтирования volumes (база данных sqlite, если используется, и сертификаты)
RUN mkdir -p certs data

# Открываем порт, который слушает ваш TLS сервер (например, 8443)
EXPOSE 8443

# Запускаем приложение с флагом сервера
ENTRYPOINT ["./shreeemp-server", "-mode", "server"]

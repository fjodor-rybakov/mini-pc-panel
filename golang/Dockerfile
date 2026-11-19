# syntax=docker/dockerfile:1
#
# Docker-сборка Go-демона s1panel: два этапа — сборка на golang (cgo +
# hidapi/udev/libusb dev-пакеты) и рантайм на debian-slim (hidraw-библиотеки,
# шрифты DejaVu — на них ссылается config.json).
#
#   docker build -t s1panel .
#   docker run -d --name s1panel --restart unless-stopped \
#       --device /dev/hidraw0 --device /dev/ttyUSB0 \
#       -v ./panel-data:/etc/s1panel -p 8686:8686 s1panel
#
# Вариант с compose — docker-compose.yml рядом.
# Контекст сборки — каталог golang/ (go.mod находится здесь).

########## build stage ##########
FROM golang:1.27-bookworm AS build
ENV CGO_ENABLED=1

# hidraw — бэкенд по умолчанию; libusb-1.0-0-dev нужен для -tags libusb (§1)
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc libc6-dev pkg-config \
        libhidapi-dev libudev-dev libusb-1.0-0-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# слой зависимостей раньше исходников: правка кода не перекачивает модули
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/s1panel ./cmd/s1panel

########## runtime stage ##########
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        libhidapi-hidraw0 \
        libusb-1.0-0 \
        libudev1 \
        fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/s1panel /usr/local/bin/s1panel

# дефолтные конфиг/темы: entrypoint засеет их в том, что смонтировано в /etc/s1panel
COPY config.example.json /opt/s1panel/config.example.json
COPY themes/ /opt/s1panel/themes/
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# относительные пути конфигов (theme_file, картинки виджетов) — отсюда
WORKDIR /etc/s1panel

VOLUME /etc/s1panel
EXPOSE 8686

ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["-config", "/etc/s1panel/config.json"]

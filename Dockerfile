# syntax=docker/dockerfile:1
#
# Docker-сборка Go-демона s1panel: два этапа — сборка на golang (cgo +
# libusb dev-пакеты) и рантайм на debian-slim (libusb-1.0, шрифты DejaVu —
# на них ссылается config.json).
#
# Бэкенд — libusb (-tags libusb), как в оригинальном Node.js-приложении
# (setDriverType('libusb')). Прошивки новых ревизий объявляют в
# report-дескрипторе output-отчёт меньше кадра (наблюдалось 512 бит вместо
# 4104 байт), и ядро через hidraw такую запись не пропускает — libusb
# работает с USB-эндпоинтом напрямую, минуя hidraw. Поэтому hidraw-узлы
# (--device /dev/hidrawN) контейнеру не нужны, нужен доступ к USB-шине:
#
#   docker build -t s1panel .
#   docker run -d --name s1panel --restart unless-stopped \
#       -v /dev/bus/usb:/dev/bus/usb --device-cgroup-rule 'c 189:* rwm' \
#       --device /dev/ttyUSB0 \
#       -v ./panel-data:/etc/s1panel -p 8686:8686 s1panel
#
# Вариант с compose — docker-compose.yml рядом.
# Контекст сборки — корень репозитория (go.mod лежит в нём).

########## build stage ##########
FROM golang:1.27-bookworm AS build
ENV CGO_ENABLED=1

# libusb — бэкенд этой сборки (-tags libusb, см. шапку); dev-пакеты hidapi/udev
# остаются, чтобы в этом же образце можно было собрать и hidraw-вариант (§1)
RUN apt-get update && apt-get install -y --no-install-recommends \
        gcc libc6-dev pkg-config \
        libhidapi-dev libudev-dev libusb-1.0-0-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# слой зависимостей раньше исходников: правка кода не перекачивает модули
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -tags libusb -trimpath -ldflags="-s -w" -o /out/s1panel ./cmd/s1panel
# ledctl — чистый Go (без cgo): диагностика ленты прямо внутри контейнера,
# docker compose run --rm --entrypoint ledctl s1panel list|off
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ledctl ./cmd/ledctl

########## runtime stage ##########
FROM debian:bookworm-slim

# go-hid компилирует hidapi (hid.c) внутрь бинарника; libusb-сборке из
# внешних библиотек нужен только libusb-1.0
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        libusb-1.0-0 \
        fonts-dejavu-core \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/s1panel /usr/local/bin/s1panel
COPY --from=build /out/ledctl /usr/local/bin/ledctl

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

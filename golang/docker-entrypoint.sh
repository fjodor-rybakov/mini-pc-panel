#!/bin/sh
# Первый запуск: если /etc/s1panel (том) пуст, засеять конфиг и темы из
# эталонных файлов образа; дальше — штатный запуск демона.
set -e

if [ ! -f /etc/s1panel/config.json ]; then
    mkdir -p /etc/s1panel
    cp /opt/s1panel/config.example.json /etc/s1panel/config.json
    cp -r /opt/s1panel/themes /etc/s1panel/themes
fi

exec /usr/local/bin/s1panel "$@"

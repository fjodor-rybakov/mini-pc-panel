# s1panel — переписано с Node.js на Go

Go-демон для AceMagic S1 LED-TFT панели (VID `0x04D9`, PID `0xFD01`): экран 320×170
по HID (интерфейс MI_01) + LED-подсветка по CH340 (последовательный порт).
Переписан с оригинального Node.js-проекта `mini-pc-panel` строго по протоколу
из [`doc/revrite.md`](doc/revrite.md) (§2), плюс браузерный редактор виджетов
телеметрии. Оригинальное Node.js-приложение, snap- и Node-Docker-сборки и
картинки удалены из репозитория 2026-11-19 — проект существует только как это
Go-переписывание (всё удалённое — в истории git).

## Структура

```
./ (корень репозитория)
├── cmd/s1panel/main.go      демон: флаги, сигнал SIGINT/SIGTERM, запуск цикла и админки
├── internal/config/         config.json + тема экранов (валидация, дефолты, сохранение)
├── internal/display/        протокол HID (пакеты) + очередь/один воркер (10 мс пауза)
├── internal/led/            CH340: пакет FA|theme|intensity|speed|checksum
├── internal/metrics/        сенсоры: gopsutil + /sys/class/hwmon (temp/power)
├── internal/render/         tdewolff/canvas → RGB565 big-endian, виджеты, PNG превью
├── internal/app/            склейка: жизненный цикл, ротация экранов, превью на реальный экран
├── internal/server/         HTTP-админка: редактор + API (embed web/)
├── Dockerfile               docker-сборка: 2 этапа (cgo+hidapi → debian-slim)
├── docker-entrypoint.sh     засев config.json/тем в том при первом запуске
├── docker-compose.yml       запуск с --device панели и CH340
├── config.example.json      пример конфигурации (значения = дефолты кода)
└── themes/demo.json         демо-тема (2 экрана, сеточные координаты)
```

## Сборка (Linux — целевая платформа)

Требуется cgo: `gcc`, `libhidapi-dev` (backend hidraw) или `libusb-1.0-0-dev`
(backend libusb), `make`.

```sh
make build         # backend hidraw (по умолчанию), bin/s1panel
make build-libusb  # -tags libusb (обязательная проверка по §1)
make test vet fmt  # тесты, vet, gofmt
```

`github.com/sstallion/go-hid` — cgo-библиотека, поэтому кросс-сборка с Windows
не работает (на Windows собирается только stub-транспорт, остальные пакеты и
тесты проходят: `go test ./...`).

## Запуск

```sh
cp config.example.json config.json   # поправить sensors/fonts под машину
./bin/s1panel -config config.json              # адрес админки — из конфига "listen"
./bin/s1panel -config config.json -admin :8080 # -admin перебивает "listen"; -admin "" — выкл
```

### Docker

```sh
docker build -t s1panel .
docker compose up -d --build   # готовый вариант с devices/volumes
```

При первом запуске в том `/etc/s1panel` засевается `config.json` (из
`config.example.json`) и каталог `themes/`. Контейнеру нужны устройства
панели и подсветки: `--device /dev/hidraw0 --device /dev/ttyUSB0`
(лучше передавать **все** hidraw-узлы панели — `grep -l 4d9
/sys/class/hidraw/*/device/uevent`: экранный узел зависит от ревизии
прошивки, демон выберет его сам, см. примечание в udev-разделе). Хостовые
датчики видны как есть (`/sys/class/hwmon` в контейнере read-only), а
`sensors.mount_points` меряет монтирования внутри контейнера — конфиг под
контейнер обычно стоит поправить.

Жизненный цикл (spec §5.3): `set_orientation` → `set_time` → полный `redraw`,
далее heartbeat по правилу оригинала (не чаще раза в секунду: занятая очередь —
порог `heartbeat` (по умолчанию 60000 мс), простой — порог `redraw_delay`) и
опрос тикером: шлются только изменившиеся регионы (`update` 0xA2, до
~255×255 пикселей на пакет) либо весь кадр.

### udev (доступ без root)

`/etc/udev/rules.d/99-s1panel.rules`:

```
SUBSYSTEM=="hidraw", ATTRS{idVendor}=="04d9", ATTRS{idProduct}=="fd01", MODE="0660", GROUP="plugdev"
KERNEL=="ttyUSB*", ATTRS{idVendor}=="1a86", ATTRS{idProduct}=="7523", MODE="0666"
```

```sh
sudo udevadm control --reload-rules && sudo udevadm trigger
sudo usermod -aG plugdev $USER   # перелогиниться
```

Прошивки панелей различаются: у оригинальных экран сидит на HID-интерфейсе
MI_01 (`:1.1` — так и было в оригинальном `config.json`: `"device":
"1-8:1.1"`), у других ревизий hidraw-узлы панели — это `:1.0` и `:1.2`
(узла MI_01 нет вовсе), а hidapi иногда не может прочитать номера
интерфейсов вообще. Демон справляется сам: если заданного `interface` нет в
перечислении, он выбирает из hidraw-узлов панели тот, у которого в
report-дескрипторе output-отчёт на 4104 байта — такой только у экранного
интерфейса (`internal/display/report_desc.go`). Вручную узел закрепляется
прямо в конфиге: `"device": { "path": "/dev/hidrawN" }` (перебором узлов —
экранный тот, от которого панель оживает); при заданном `path`
`vid`/`pid`/`interface` в поиске не участвуют.

### systemd

`/etc/systemd/system/s1panel.service`:

```ini
[Unit]
Description=AceMagic S1 panel daemon
After=multi-user.target

[Service]
ExecStart=/opt/s1panel/bin/s1panel -config /etc/s1panel/config.json -admin 127.0.0.1:8080
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Протокол (коротко, полностью — doc/revrite.md §2)

Каждый HID-командный пакет ровно **4104 байта** (8 заголовков + 4096 payload,
обнулённый хвост); на `dev.Write` уходит **4105** — ведущий `0x00` Report ID
добавляет только транспорт (`transport_linux.go`), внутри пакет остаётся 4104.

| команда | заголовок |
|---|---|
| orientation | `55 A1 F1 o` (01 = 320×170, 02 = 170×320) |
| heartbeat / set time | `55 A1 F2|F3 h m s` |
| redraw | `55 A3 sub seq off len` (заголовок в двух раскладках — см. ниже), 27 чанков × 4096 (последний 2304) |
| update | `55 A2 x_lo x_hi y_lo y_hi w h`, `w*h*2 ≤ 4096` |

### ⚠ Дуальность заголовка redraw (важно)

Оригинальный JS (`lcd_device.js:103-105`) писал offset/length **big-endian** в
байты 5..6/7..8, а копирование payload в `p[8]` затирало младший байт длины.
Спецификация §2 описывает «правильный» little-endian вариант. Реализованы оба:

- `DefaultRedrawLayout = RedrawOriginalLayout` — байт-в-байт как в JS
  (включая затирание `p[8]`); это раскладка по умолчанию — оригинал доказал её
  на реальном железе;
- `RedrawSpecLayout` — «правильный» little-endian вариант из §2 (длина в
  `p[6:8]`), тоже реализован.

Если доверяете §2, поменяйте константу в `internal/display/protocol.go` —
тесты `TestRedrawPacketsSpecLayout` и `TestRedrawOriginalLayoutQuirk`
фиксируют обе раскладки.

## Редактор виджетов (PART B)

Открыть `http://<host>:8686` — адрес берётся из `"listen"` в конфиге (флаг
`-admin :PORT` перебивает его, `-admin ""` отключает редактор). Vanilla JS,
без сборки.

- **Сетка**: мелкие линии каждые 10 px, крупные каждые 50 px, координаты под
  курсором; привязка к сетке (шаг 5/10/20) включается кнопкой «привязка к сетке».
- Виджеты: `text`, `doughnut`, `doughnut_chart`, `line_chart`, `bar_chart`,
  `custom_bar`, `image`, `spacer`; у экрана — поле `wallpaper` (фон-картинка
  на весь экран, режим cover); drag — мышью, resize — поля свойств.
- Цифры в редакторе **реальные**: `GET /api/poll` раз в секунду тянет те же
  показания, что рисует демон.
- **«Примерить на экран»** — `POST /api/preview?screen=N`: черновик валидируется
  (`config.ParseTheme`), рендерится и отправляется **на физический экран**
  (полный redraw + LED экрана), в ответ приходит PNG того, что теперь показывает
  панель; тишина опроса на время показа превью.
- **«Применить»** — хот-своп живой темы (`POST /api/apply`), `?save=1` пишет
  `theme_file`; «Сохранить и применить» — то же с сохранением.
- LED вручную: тема/яркость/скорость 1..5 → `POST /api/led` (CH340, контрольная
  сумма = сумма первых 4 байт & 0xFF).
- **Живое переключение тем** (как дропдаун оригинального GUI): `GET /api/themes`
  отдаёт `theme_list` + текущий `theme_file`, `POST /api/theme {"file":…}` или
  `{"name":…}` грузит тему из списка на живую; `config.json` на диске не
  трогается. В редакторе — селектор «тема» в шапке.
- **Экраны**: `POST /api/next_screen` — сразу следующий включённый экран, тело
  `{"id":N}` прыгает на экран с этим id; `POST /api/pause?on=true|false` —
  пауза/возобновление ротации. В редакторе — кнопки «Следующий экран» и
  «пауза ротации» в футере.

## Тесты

`go test ./...` — 7 пакетов, всё зелёное: байтовые спецификации всех четырёх
команд (обе раскладки redraw), целостность redraw перед heartbeat в одном
воркере, clamps и контрольные суммы LED, дефолты/отказы конфига и темы, hwmon
на фейковой sysfs, RGB565/format-токены/регионы и пиксельные замки виджетов,
весь HTTP-редактор (включая `/api/themes`, `/api/theme`, `/api/next_screen`,
`/api/pause`) через `httptest` и фейковый `Panel` (без железа).

## Отличия от оригинального JS (осознанно)

- Погодная подсистема вычеркнута целиком: оригинальное Node.js-приложение
  `s1panel/` (виджеты `weather`/`weather_icon`/`winddirection`, иконки
  iconify) удалено из репозитория, а сами эти типы виджетов больше не
  принимаются валидацией темы.
- Оригинальный массив `sensors` в `config.json` игнорируется: набор сенсоров
  Go собирает сам из блоков `sensors.*` (clock, calendar, cpu, memory,
  space_<mount>, net_<iface>_rx/tx, temp, power).
- Heartbeat — как в оригинале, по порогам (`heartbeat` при занятой очереди,
  `redraw_delay` при простое) и не чаще раза в секунду (§5.2); «строго раз
  в 1 с» не реализовано намеренно.
- Стартовый `set_time` (F3) демон шлёт по §5.3 — оригинал F3 не использовал.
- LED при смене экрана: оригинал откатывался к `theme||4`; здесь нулевые поля
  `led_config` экрана наследуют глобальные настройки конфига (5/3/3), режим 6
  («не трогать») пакет не шлёт — как в `led_thread.js`.
- Линейные графики — ломаные без слаживания (в оригинале canvas tension 0.1);
  `image`/`wallpaper` обрезаются геометрически вместо canvas-клипа; пути
  картинок разрешаются относительно файла темы.
- Переключение темы из редактора живое и не пишет `config.json`; сохранение
  темы в файл — только «Применить» с галкой.
- Нет инвертированного UI `fix_value`.
- API админки своё (см. выше); форма оригинального Electron-UI не
  воспроизводилась — тема, экраны и пауза ротации доступны прямо в редакторе.

# syntax=docker/dockerfile:1
#
# Build the s1panel image the same way the snap build does:
# ubuntu 24.04 (same base as the snap's core24) + Node LTS + native deps
# compiled with --build-from-source, then a slim runtime stage.
#
#   docker build -t s1panel .
#   docker run -d --name s1panel --restart unless-stopped \
#       --device /dev/bus/usb --device /dev/ttyUSB0 \
#       -v s1panel-data:/data -p 8686:8686 s1panel
#
# Note: the Node tarball below is linux-x64 (the S1 Mini PC arch); build on amd64.

ARG NODE_VERSION=26.13.1

########## build stage ##########
FROM ubuntu:24.04 AS build
ARG NODE_VERSION
ENV DEBIAN_FRONTEND=noninteractive

# same dev packages the snap's override-build installs
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl xz-utils pkg-config python3 build-essential \
        libcairo2-dev \
        libgdk-pixbuf-2.0-dev \
        libgif-dev \
        libjpeg-turbo8-dev \
        libpango1.0-dev \
        librsvg2-dev \
        libudev-dev \
        libusb-1.0-0-dev \
        libx11-dev \
        poppler-data \
        fonts-noto-color-emoji \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz" \
    | tar -xJf - -C /usr/local --strip-components=1

WORKDIR /build

# dependency layers first so source edits don't recompile node_modules
COPY s1panel/package.json s1panel/package-lock.json ./
COPY s1panel/gui/package.json s1panel/gui/package-lock.json ./gui/
RUN npm ci --build-from-source && (cd gui && npm ci)

COPY s1panel/ ./
RUN (cd gui && npm run build)

########## runtime stage ##########
FROM ubuntu:24.04 AS runtime
ARG NODE_VERSION
ENV DEBIAN_FRONTEND=noninteractive \
    NODE_ENV=production \
    S1PANEL_CONFIG=/data

# same runtime libraries the snap's stage-packages pull in
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        libatomic1 \
        libudev1 \
        libusb-1.0-0 \
        libx11-6 \
        libcairo2 \
        libpangocairo-1.0-0 \
        libjpeg-turbo8 \
        libgif7 \
        libcairo-gobject2 \
        libgdk-pixbuf-2.0-0 \
        librsvg2-2 \
        fontconfig \
        fontconfig-config \
        fonts-dejavu-core \
        fonts-liberation \
        fonts-freefont-ttf \
        fonts-texgyre \
        fonts-ebgaramond \
        fonts-noto-color-emoji \
        poppler-data \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /usr/local /usr/local

# app layout mirrors the snap's $SNAP/s1panel/ (modules load from app dir,
# config + themes live in $S1PANEL_CONFIG)
WORKDIR /app
COPY --from=build /build/*.js /build/config.json ./
COPY --from=build /build/sensors ./sensors/
COPY --from=build /build/widgets ./widgets/
COPY --from=build /build/themes ./themes/
COPY --from=build /build/node_modules ./node_modules/
COPY --from=build /build/gui/dist ./gui/dist/

COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

VOLUME /data
EXPOSE 8686

ENTRYPOINT ["docker-entrypoint.sh"]

#!/bin/sh
# mirrors launch-s1panel.sh from the snap: seed the writable config
# directory on first run, then start the panel
set -e

if [ ! -f "$S1PANEL_CONFIG/config.json" ]; then
    mkdir -p "$S1PANEL_CONFIG"
    cp /app/config.json "$S1PANEL_CONFIG/config.json"
    cp -r /app/themes "$S1PANEL_CONFIG/"
fi

cd /app
exec node ./main.js "$@"

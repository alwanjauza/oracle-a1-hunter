#!/system/bin/sh
# Oracle A1 Hunter — module installer (runs inside the KernelSU/Magisk
# manager with MODPATH pointing at the staged module dir).

DATADIR=/data/adb/oracle-hunter

ui_print "*******************************"
ui_print "  Oracle A1 Hunter installer"
ui_print "*******************************"

# --- ABI guard: the bundled binary is arm64 only ---
ARCH=$(uname -m)
if [ "$ARCH" != "aarch64" ]; then
  abort "This module bundles an arm64 binary, but this device is $ARCH. Use the manual ADB install with a self-built binary instead."
fi

# --- data dir: create, never overwrite existing identity/history ---
mkdir -p "$DATADIR/bin" "$DATADIR/logs"
if [ ! -f "$DATADIR/config.env" ]; then
  ui_print "- first install: config.env will be generated on first start"
  ui_print "  (fill credentials via the WebUI Config tab)"
else
  ui_print "- existing config.env preserved"
fi
[ -f "$DATADIR/stats.json" ] && ui_print "- existing stats & battery history preserved"

# --- install / refresh the binary ---
cp -f "$MODPATH/bin/hunter" "$DATADIR/bin/hunter" || abort "failed to copy binary"
chmod 755 "$DATADIR/bin/hunter"

# --- migration from the manual/ADB install: kill the legacy boot script ---
if [ -f /data/adb/service.d/99-oracle-hunter.sh ]; then
  rm -f /data/adb/service.d/99-oracle-hunter.sh
  ui_print "- removed legacy service.d autostart (module handles it now)"
fi

# --- start now, no reboot needed ---
pkill -f "$DATADIR/bin/hunter" 2>/dev/null
sleep 1
nohup "$DATADIR/bin/hunter" -config "$DATADIR" >> "$DATADIR/logs/stdout.log" 2>&1 &
sleep 2

TOKEN=$(sed -n 's/^AUTH_TOKEN=//p' "$DATADIR/config.env" 2>/dev/null | head -1 | tr -d '\r')

ui_print "- hunter started"
ui_print ""
ui_print "  Next steps:"
ui_print "  1. Open http://localhost:8080 on the phone"
ui_print "     (or http://<phone-ip>:8080 from another device)"
if [ -n "$TOKEN" ]; then
  ui_print "  2. Login token: $TOKEN"
  ui_print "     (also in $DATADIR/config.env — AUTH_TOKEN)"
else
  ui_print "  2. Token: see AUTH_TOKEN in $DATADIR/config.env"
fi
ui_print "  3. Fill OCI credentials on the Config tab — hunting"
ui_print "     starts automatically once they are complete."
ui_print ""
ui_print "  Config/keys/history live in $DATADIR"
ui_print "  (module uninstall keeps them; remove manually if desired)"

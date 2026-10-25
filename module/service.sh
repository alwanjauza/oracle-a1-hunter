#!/system/bin/sh
# Oracle A1 Hunter — boot service (runs from the module dir).
# Data dir is intentionally OUTSIDE the module dir so module updates and
# uninstalls never touch config.env, the OCI key, stats.json or history.
DATADIR=/data/adb/oracle-hunter
MODDIR=${0%/*}

# Refresh the installed binary from this module (no-op when identical).
if [ -x "$MODDIR/bin/hunter" ]; then
  mkdir -p "$DATADIR/bin" "$DATADIR/logs"
  cmp -s "$MODDIR/bin/hunter" "$DATADIR/bin/hunter" 2>/dev/null || \
    cp "$MODDIR/bin/hunter" "$DATADIR/bin/hunter" && chmod 755 "$DATADIR/bin/hunter"
fi

# Only one instance: a previous boot's process may still be alive.
if pgrep -f "$DATADIR/bin/hunter" >/dev/null 2>&1; then
  exit 0
fi

sleep 25
nohup "$DATADIR/bin/hunter" -config "$DATADIR" >> "$DATADIR/logs/stdout.log" 2>&1 &

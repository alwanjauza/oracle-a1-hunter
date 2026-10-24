#!/system/bin/sh
# Oracle A1 Hunter v2 — autostart via KernelSU service.d
MODDIR=/data/adb/oracle-hunter
mkdir -p "$MODDIR/logs"
sleep 25
nohup "$MODDIR/bin/hunter" -config "$MODDIR" >> "$MODDIR/logs/stdout.log" 2>&1 &

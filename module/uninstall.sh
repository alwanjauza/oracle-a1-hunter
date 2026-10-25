#!/system/bin/sh
# Oracle A1 Hunter — uninstall. Stops the hunt; user data (config.env,
# OCI key, stats.json, battery history, logs) is intentionally KEPT so a
# reinstall continues where it left off.
DATADIR=/data/adb/oracle-hunter
pkill -f "$DATADIR/bin/hunter" 2>/dev/null
ui_print "- hunter stopped"
ui_print "- data preserved in $DATADIR"
ui_print "  (delete that folder manually for a full wipe)"

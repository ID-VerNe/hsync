#!/bin/bash
# hsync verification suite (Windows Git Bash)
set -u
HSYNC="$HOME/.local/bin/hsync.exe"
export HSYNC_HOME="$(mktemp -d /tmp/hsync-registry.XXXXXX)"
echo "=== registry home: $HSYNC_HOME ==="

FAIL=0
check() { # check <desc> <cond>
  if [ "$2" = "1" ]; then echo "PASS: $1"; else echo "FAIL: $1"; FAIL=1; fi
}

# --- 1. smoke: add creates hardlink, same inode ---
WORK="$(mktemp -d /tmp/hsync-work.XXXXXX)"
SRC="$WORK/src"; DST="$WORK/dst"
mkdir -p "$SRC"
echo "hello" > "$SRC/a.txt"
"$HSYNC" add "$SRC" "$DST" >/dev/null 2>&1
INODE_A=$(stat -c %i "$SRC/a.txt"); INODE_B=$(stat -c %i "$DST/a.txt")
check "add mirrors file with hardlink (same inode)" "$([ "$INODE_A" = "$INODE_B" ] && echo 1 || echo 0)"

# --- 2. recursive: nested dirs ---
mkdir -p "$SRC/skills/utils" "$SRC/skills/tools"
echo "api" > "$SRC/skills/utils/api.js"
echo "tool" > "$SRC/skills/tools/run.py"
"$HSYNC" sync >/dev/null 2>&1
check "nested dir created as physical dir (not symlink)" "$([ -d "$DST/skills/utils" ] && echo 1 || echo 0)"
INODE_X=$(stat -c %i "$SRC/skills/utils/api.js"); INODE_Y=$(stat -c %i "$DST/skills/utils/api.js")
check "nested file hardlinked" "$([ "$INODE_X" = "$INODE_Y" ] && echo 1 || echo 0)"

# --- 3. delete propagates: file + zombie dir ---
rm "$SRC/a.txt"
rm -rf "$SRC/skills/tools"
"$HSYNC" sync >/dev/null 2>&1
check "deleted file removed from target" "$([ ! -e "$DST/a.txt" ] && echo 1 || echo 0)"
check "zombie dir recursively removed" "$([ ! -e "$DST/skills/tools" ] && echo 1 || echo 0)"
check "surviving dir kept" "$([ -d "$DST/skills/utils" ] && echo 1 || echo 0)"

# --- 4. broken-link relink (atomic-save simulation) ---
echo "v1" > "$SRC/b.txt"
"$HSYNC" sync >/dev/null 2>&1
rm "$DST/b.txt"                       # break the link: target gone
echo "v2-atomic" > "$DST/b.txt"       # atomic-save writes a NEW inode in target
"$HSYNC" sync >/dev/null 2>&1
INODE_B1=$(stat -c %i "$SRC/b.txt"); INODE_B2=$(stat -c %i "$DST/b.txt")
check "broken link re-hardlinked (source wins)" "$([ "$INODE_B1" = "$INODE_B2" ] && echo 1 || echo 0)"
CONTENT_B=$(cat "$DST/b.txt")
check "re-linked target reads source content" "$([ "$CONTENT_B" = "v1" ] && echo 1 || echo 0)"

# --- 5. blocklist: dot + node_modules ignored ---
mkdir -p "$SRC/.git" "$SRC/node_modules"
echo "git" > "$SRC/.git/config"
echo "nm" > "$SRC/node_modules/x.js"
echo ".env" > "$SRC/.env"
"$HSYNC" sync >/dev/null 2>&1
check ".git not mirrored" "$([ ! -e "$DST/.git" ] && echo 1 || echo 0)"
check "node_modules not mirrored" "$([ ! -e "$DST/node_modules" ] && echo 1 || echo 0)"
check "dotfile .env not mirrored" "$([ ! -e "$DST/.env" ] && echo 1 || echo 0)"
# and blocklisted target-side leftovers are NOT removed either
mkdir -p "$DST/.git"; echo "old" > "$DST/.git/old"
"$HSYNC" sync >/dev/null 2>&1
check "target dot-dir left untouched (not removed)" "$([ -e "$DST/.git/old" ] && echo 1 || echo 0)"

# --- 6. fault tolerance: bad target, exit 0 ---
"$HSYNC" add "$SRC" "$WORK/no-such-target" >/dev/null 2>&1
"$HSYNC" sync >/dev/null 2>&1
EC=$?
check "sync with missing target exits 0" "$([ "$EC" = "0" ] && echo 1 || echo 0)"

# --- 7. concurrency + timing: 3 pairs x 1200 files ---
TIMEWORK="$(mktemp -d /tmp/hsync-time.XXXXXX)"
for p in 1 2 3; do
  mkdir -p "$TIMEWORK/s$p"
  for i in $(seq 1 400); do echo "x$i" > "$TIMEWORK/s$p/f$i.txt"; done
  "$HSYNC" add "$TIMEWORK/s$p" "$TIMEWORK/d$p" >/dev/null 2>&1
done
START=$(date +%s%N)
"$HSYNC" sync >/dev/null 2>&1
END=$(date +%s%N)
MS=$(( (END - START) / 1000000 ))
echo "=== timing: sync 3 pairs x 400 files = ${MS}ms ==="
COUNT=$(ls "$TIMEWORK/d1" | wc -l)
check "first pair mirrored all files" "$([ "$COUNT" = "400" ] && echo 1 || echo 0)"

# --- 8. remove + list ---
REMOVE_TARGET="$("$HSYNC" list 2>/dev/null | grep no-such-target | awk '{print $2}')"
"$HSYNC" remove "$REMOVE_TARGET" >/dev/null 2>&1
check "remove by source path works" "$([ "$("$HSYNC" list 2>/dev/null | grep -c no-such-target)" = "0" ] && echo 1 || echo 0)"

echo "=== registry: $HSYNC_HOME/registry.json ==="
cat "$HSYNC_HOME/registry.json"

echo
if [ "$FAIL" = "1" ]; then echo "RESULT: FAILURES PRESENT"; else echo "RESULT: ALL PASS"; fi
exit $FAIL

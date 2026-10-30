#!/usr/bin/env bash
# Switch baselines for the paper (#73): turning a node's copy of variant A into variant B with rsync and xdelta3.
#
#   bench/switch_baselines.sh <build dir> <work dir> <variant A file> <variant B file> [xdelta3 binary] [window]
#
# rsync: the node's copy of A is placed under B's name and rsync --no-whole-file brings it to B (local transfer,
# so sender and receiver talk through a pipe; "Total bytes sent" is what would cross the network), once without
# and once with -z (zstd level 3). xdelta3: encode B against A with a source window of [window] bytes
# (default 2 GiB), decode it, compare. Every result is checked against B's sha256. Needs GNU time at
# /usr/bin/time and about 3 × the size of B free in the work dir. Prints TSV rows:
#   tool, variant, bytes (crossing the network), seconds, peak RSS KiB, identical
# and keeps the logs in the work dir. Everything runs under nice; nothing outside the work dir is written.
set -euo pipefail
src=$1 work=$2 va=$3 vb=$4 xd=${5:-xdelta3} win=${6:-2147483648}
mkdir -p "$work"
N="nice -n 19"
row() { printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$@"; }
tv() { # tv <log> cmd...: run under GNU time; the log gets time's report
	local log=$1
	shift
	/usr/bin/time -v -o "$log.time" $N "$@" > "$log" 2>&1
}
wall() { python3 - "$1" << 'EOF'
import sys
for l in open(sys.argv[1]):
    if "Elapsed (wall clock)" in l:
        t = l.rsplit(" ", 1)[1].strip().split(":")
        s = 0.0
        for p in t:
            s = s * 60 + float(p)
        print(round(s, 2))
EOF
}
rss() { awk -F': ' '/Maximum resident set size/ {print $2}' "$1"; }
want=$(sha256sum "$src/$vb" | cut -c1-64)
same() { [ "$(sha256sum "$1" | cut -c1-64)" = "$want" ] && echo yes || echo NO; }

# rsync, without and with compression
for z in plain:"" zstd:"-z --compress-choice=zstd --compress-level=3"; do
	name=${z%%:*} flags=${z#*:}
	rm -rf "$work/rs" && mkdir -p "$work/rs"
	cp "$src/$va" "$work/rs/$vb"
	log=$work/rsync-$name.log
	# shellcheck disable=SC2086
	tv "$log" rsync --no-whole-file --ignore-times --stats $flags "$src/$vb" "$work/rs/"
	sent=$(awk -F': ' '/^Total bytes sent/ {gsub(/,/, "", $2); print $2}' "$log")
	row "rsync-$name" "$vb" "$sent" "$(wall "$log.time")" "$(rss "$log.time")" "$(same "$work/rs/$vb")"
	grep -E '^(Literal data|Matched data|Total bytes (sent|received))' "$log" | sed "s/^/# rsync-$name: /"
	rm -rf "$work/rs"
done

# xdelta3: encode against A with a large source window, then decode
log=$work/xdelta3-encode.log
tv "$log" "$xd" -e -f -B "$win" -s "$src/$va" "$src/$vb" "$work/delta.vcdiff"
row xdelta3-encode "$vb" "$(stat -c %s "$work/delta.vcdiff")" "$(wall "$log.time")" "$(rss "$log.time")" -
log=$work/xdelta3-decode.log
tv "$log" "$xd" -d -f -B "$win" -s "$src/$va" "$work/delta.vcdiff" "$work/out"
row xdelta3-decode "$vb" "$(stat -c %s "$work/delta.vcdiff")" "$(wall "$log.time")" "$(rss "$log.time")" "$(same "$work/out")"
rm -f "$work/delta.vcdiff" "$work/out"

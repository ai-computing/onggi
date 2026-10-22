#!/usr/bin/env bash
# Baselines on one build set (#51): what storing the set and switching a node between two variants
# cost with common methods and with onggi, on the same files.
#
#   bench/baselines.sh <onggi binary> <build dir> <work dir> <variant A file> <variant B file>
#
# The work dir needs room for two onggi stores of the set (about 2 × 50 GB for the ATOM
# Llama-3.1-8B set). Needs zstd >= 1.4, tar, python3. Prints TSV rows: section, method, bytes,
# seconds. Everything runs under nice; nothing outside the work dir is written.
set -euo pipefail
onggi=$1 src=$2 work=$3 va=$4 vb=$5
mkdir -p "$work"
row() { printf '%s\t%s\t%s\t%s\n' "$@"; }
now() { date +%s.%N; }
since() { python3 -c "import sys;print(round($(now)-$1,1))"; }
json() { python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(eval(sys.argv[2]))" "$@"; }
N="nice -n 19"

raw=$(find "$src" -type f -printf '%s\n' | awk '{s+=$1} END {print s}')
row storage raw "$raw" 0

# OCI-style layers: one layer per distinct file (by sha256), each compressed with zstd -3
t=$(now)
oci=$(cd "$src" && find . -type f -print0 | sort -z | xargs -0 sha256sum | sort -u -k1,1 | cut -c67- |
	while read -r f; do $N zstd -q -3 -T0 -c "$f" | wc -c; done | awk '{s+=$1} END {print s}')
row storage oci-layers-zstd3 "$oci" "$(since "$t")"

# the whole set as one tar, zstd with a 2 GiB long-range window
t=$(now)
lr=$(cd "$src" && tar cf - . | $N zstd -q -3 -T0 --long=31 -c | wc -c)
row storage tar-zstd3-long31 "$lr" "$(since "$t")"

# fixed blocks and FastCDC 64 KiB, dedup only
for b in 64KiB 1MiB; do
	t=$(now)
	$N "$onggi" dedup-report --quiet --json --fixed-block "$b" "$src" > "$work/dr-$b.json"
	row storage "fixed-$b-dedup" "$(json "$work/dr-$b.json" "d['fixed_block']['unique_bytes']")" "$(since "$t")"
done
row storage cdc64k-dedup "$(json "$work/dr-1MiB.json" "d['unique_bytes']")" -

# Xet-style: CDC 64 KiB with per-chunk compression (onggi without sub-chunks), and onggi
for m in xet:--sub=0 onggi:; do
	name=${m%%:*} flag=${m#*:}
	rm -rf "$work/$name"
	t=$(now)
	$N "$onggi" push --store "$work/$name" $flag --quiet --json --tmp "$work" "$src" > "$work/push-$name.json"
	row storage "$name" "$(du -sb "$work/$name" | cut -f1)" "$(since "$t")"
done

# switching a node from variant A to B: what crosses the network
row transfer oci-layer "$($N zstd -q -3 -T0 -c "$src/$vb" | wc -c)" -
for m in xet:--sub=0 onggi:; do
	name=${m%%:*} flag=${m#*:}
	for v in a b; do
		rm -rf "$work/v$v" && mkdir -p "$work/v$v"
		f=$va; [ $v = b ] && f=$vb
		ln "$src/$f" "$work/v$v/" 2>/dev/null || cp "$src/$f" "$work/v$v/"
	done
	rm -rf "$work/t-$name"
	"$onggi" push --store "$work/t-$name" $flag --quiet --json --tmp "$work" "$work/va" > /dev/null
	"$onggi" push --store "$work/t-$name" $flag --quiet --json --tmp "$work" "$work/vb" > "$work/t-$name.json"
	row transfer "$name" "$(json "$work/t-$name.json" "d['stats']['pack_bytes'] if 'stats' in d else d['pack_bytes']")" -
	rm -rf "$work/t-$name" "$work/va" "$work/vb"
done

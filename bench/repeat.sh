#!/usr/bin/env bash
# Repeated onggi measurements and an ablation for the paper (#73), on one build set and one variant switch.
#
#   bench/repeat.sh repeat   <onggi binary> <build dir> <work dir> <variant A file> <variant B file> [n]
#   bench/repeat.sh ablation <onggi binary> <build dir> <work dir> <variant A file> <variant B file>
#
# repeat, n times (default 5), interleaved in each round:
#   push-set       push the whole build dir into a new store: stored bytes (du, with indexes), time, peak RSS
#   transfer-xet   push A, then B, into a new store with --sub 0 (Xet-style): B's pack bytes and push time
#   transfer-onggi the same with onggi's defaults
#   pull-plain     pull B into an empty dir from the second store (no cache): bytes read, time
#   pull-delta     pull A, then B with --base A --base-dir: bytes read, bytes left in place, time
# plain and delta pulls alternate which goes first. ablation runs push-set and the A → B transfer once for each of:
# default, --sub 0, --frame 0, --compress none.
#
# Prints one JSON object per measurement (run, method, flags, bytes, seconds, peak RSS KiB, and the onggi JSON).
# Pulled files are checked against the original's sha256. Needs GNU time at /usr/bin/time and about
# 2 × |A| + 3 × |B| + one store of the set free in the work dir. Everything runs under nice; nothing outside the
# work dir is written. The page cache cannot be dropped without root: the build set is read in every round.
set -euo pipefail
mode=$1 onggi=$2 src=$3 work=$4 va=$5 vb=$6 n=${7:-5}
N="nice -n 19"
mkdir -p "$work/tmp"
t0() { /usr/bin/time -v -o "$work/time.txt" $N "$@"; }
emit() { # emit <run> <method> <flags> <bytes> <json file|-> [extra json]
	python3 - "$work/time.txt" "$@" << 'EOF'
import json, sys
tf, run, method, flags, nbytes, jf = sys.argv[1:7]
extra = json.loads(sys.argv[7]) if len(sys.argv) > 7 else {}
wall = rss = None
for l in open(tf):
    if "Elapsed (wall clock)" in l:
        wall = 0.0
        for p in l.rsplit(" ", 1)[1].strip().split(":"):
            wall = wall * 60 + float(p)
    if "Maximum resident set size" in l:
        rss = int(l.split(":")[1])
d = {"run": int(run), "method": method, "flags": flags, "bytes": int(nbytes), "seconds": round(wall, 2), "peak_rss_kib": rss}
if jf != "-":
    d["onggi"] = json.load(open(jf))
d.update(extra)
print(json.dumps(d), flush=True)
EOF
}
jget() { python3 -c "import json,sys;d=json.load(open(sys.argv[1]));d=d.get('stats',d);print(d[sys.argv[2]])" "$@"; }
want=$(sha256sum "$src/$vb" | cut -c1-64)
same() { [ "$(sha256sum "$1" | cut -c1-64)" = "$want" ] && echo true || echo false; }

# the two variants, each alone in a directory (copies: the build dir is only read)
for v in a b; do
	f=$va
	[ $v = b ] && f=$vb
	if [ ! -f "$work/v$v/$f" ]; then
		rm -rf "$work/v$v" && mkdir -p "$work/v$v" && cp "$src/$f" "$work/v$v/"
	fi
done

push_set() { # push_set <run> <name> [flags]
	local run=$1 name=$2
	shift 2
	rm -rf "$work/set"
	t0 "$onggi" push --store "$work/set" "$@" --quiet --json --tmp "$work/tmp" "$src" > "$work/push-set.json"
	emit "$run" push-set "$name" "$(du -sb "$work/set" | cut -f1)" "$work/push-set.json"
	rm -rf "$work/set"
}

transfer() { # transfer <run> <name> <store> [flags]: push A, then B; B's packs are what crosses the network
	local run=$1 name=$2 st=$3
	shift 3
	rm -rf "$st"
	$N "$onggi" push --store "$st" "$@" --ref a --quiet --json --tmp "$work/tmp" "$work/va" > /dev/null
	t0 "$onggi" push --store "$st" "$@" --ref b --quiet --json --tmp "$work/tmp" "$work/vb" > "$work/push-b.json"
	emit "$run" "transfer-$name" "$name" "$(jget "$work/push-b.json" pack_bytes)" "$work/push-b.json"
}

pull_plain() {
	local run=$1 st=$2
	rm -rf "$work/out"
	t0 "$onggi" pull --store "$st" --json b "$work/out" > "$work/pull.json"
	emit "$run" pull-plain default "$(jget "$work/pull.json" read_bytes)" "$work/pull.json" \
		"{\"identical\": $(same "$work/out/$vb")}"
	rm -rf "$work/out"
}

pull_delta() {
	local run=$1 st=$2
	rm -rf "$work/base" "$work/out"
	$N "$onggi" pull --store "$st" a "$work/base" > /dev/null
	t0 "$onggi" pull --store "$st" --json --base a --base-dir "$work/base" b "$work/out" > "$work/pull.json"
	emit "$run" pull-delta default "$(jget "$work/pull.json" read_bytes)" "$work/pull.json" \
		"{\"identical\": $(same "$work/out/$vb")}"
	rm -rf "$work/base" "$work/out"
}

case $mode in
repeat)
	for i in $(seq 1 "$n"); do
		push_set "$i" default
		transfer "$i" xet "$work/t-xet" --sub 0
		rm -rf "$work/t-xet"
		transfer "$i" onggi "$work/t-onggi"
		if [ "$i" = 1 ]; then
			$N "$onggi" diff --store "$work/t-onggi" --json --ranges a b > "$work/diff.json"
			python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(json.dumps({'run':0,'method':'diff','diff':d}))" "$work/diff.json"
		fi
		if [ $((i % 2)) = 1 ]; then
			pull_plain "$i" "$work/t-onggi"
			pull_delta "$i" "$work/t-onggi"
		else
			pull_delta "$i" "$work/t-onggi"
			pull_plain "$i" "$work/t-onggi"
		fi
		rm -rf "$work/t-onggi"
	done
	;;
ablation)
	for m in default: sub0:--sub=0 frame0:--frame=0 compress-none:--compress=none; do
		name=${m%%:*} flag=${m#*:}
		# shellcheck disable=SC2086
		push_set 1 "$name" $flag
		# shellcheck disable=SC2086
		transfer 1 "$name" "$work/t-$name" $flag
		rm -rf "$work/t-$name"
	done
	;;
*)
	echo "usage: bench/repeat.sh repeat|ablation <onggi> <build dir> <work dir> <A> <B> [n]" >&2
	exit 2
	;;
esac
rm -rf "$work/va" "$work/vb" "$work/tmp"

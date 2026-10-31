set -eu
cd /home/gspark/workspace/codex_e11_20261031
if [ ! -d source ]; then git clone --no-checkout https://github.com/ai-computing/onggi.git source; fi
cd source
git checkout --detach 05c97605b07882f9ceaaeb90b94878f8923f81cf
export GOMAXPROCS=4 CGO_ENABLED=0 GOCACHE=/home/gspark/workspace/codex_e11_20261031/go-cache
/home/gspark/.cache/onggi-build/go/bin/go version
/home/gspark/.cache/onggi-build/go/bin/go test ./pkg/peer ./pkg/store
/home/gspark/.cache/onggi-build/go/bin/go build -trimpath -ldflags '-s -w' -o ../onggi ./cmd/onggi
cd ..
mkdir -p gateway
curl -fsSL https://github.com/versity/versitygw/releases/download/v1.8.0/versitygw_v1.8.0_Linux_x86_64.tar.gz -o gateway.tar.gz
tar -xzf gateway.tar.gz -C gateway
find gateway -type f -name versitygw -exec cp {} ./versitygw \;
sha256sum onggi versitygw gateway.tar.gz
./onggi version
printf '0
' > prepare-tools.exit

#!/bin/sh
# Builds the hello binaries for every platform into bin/ (stdlib only, no cgo).
set -eu
cd "$(dirname "$0")/src"
go vet ./... && go test ./... -count=1
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64; do
	os=${target%/*}
	arch=${target#*/}
	ext=""
	[ "$os" = windows ] && ext=".exe"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w" -o "../bin/hello-$os-$arch$ext" .
	echo "built bin/hello-$os-$arch$ext"
done
# Windows variant for the V2 test: a real hello.exe next to the sh launcher.
cp ../bin/hello-windows-amd64.exe ../bin/hello.exe
chmod 755 ../bin/hello ../bin/hello-darwin-* ../bin/hello-linux-*

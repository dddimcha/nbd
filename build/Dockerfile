# Reproducible build & test environment. Run from the repo root:
#   docker build -f build/Dockerfile --target test .        # full gate suite
#   docker build -f build/Dockerfile --target verify .      # fast gates only
FROM golang:1.25 AS base
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

# Fast gates: formatting, vet, race tests.
FROM base AS verify
RUN test -z "$(gofmt -l .)" \
 && go vet ./... \
 && go test -race ./...

# Full gates: fast gates + fuzz smoke + bench smoke.
FROM verify AS test
RUN go test -run='^$' -fuzz=FuzzDeserialize -fuzztime=30s ./blockdevice \
 && go test -run='^$' -bench=. -benchtime=1x ./blockdevice

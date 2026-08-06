.PHONY: verify test fuzz bench docker-verify docker-test

verify:            ## fast gates: fmt, vet, race tests
	scripts/gates.sh

test:              ## full gates: + 30s fuzz + bench smoke
	scripts/gates.sh --full

fuzz:              ## extended fuzzing
	go test -run='^$$' -fuzz=FuzzDeserialize -fuzztime=5m ./blockdevice

bench:             ## benchmarks with allocation stats
	go test -run='^$$' -bench=. -benchmem ./blockdevice

docker-verify:     ## fast gates in a clean container
	docker build -f build/Dockerfile --target verify .

docker-test:       ## full gates in a clean container
	docker build -f build/Dockerfile --target test .

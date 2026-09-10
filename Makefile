# finops-proxy — common development targets.
# `make build` produces the three binaries under bin/ (gitignored).

.PHONY: build test race vet bench lint run docker-up docker-down release clean

build:
	go build -o bin/finops-proxy ./cmd/proxy
	go build -o bin/finops-run ./cmd/finops-run
	go build -o bin/mockserver ./cmd/mockserver

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

bench:
	go test -bench=. -benchmem -benchtime=1s ./pkg/circuitbreaker/

lint:
	golangci-lint run

run:
	go run ./cmd/proxy

docker-up:
	docker compose up --build -d

docker-down:
	docker compose down

# make release v=0.1.0 — tag the current commit and push the tag; CI then runs
# the tests, builds the binaries, and publishes the GitHub release automatically.
release:
	@test -n "$(v)" || (echo "usage: make release v=0.1.0" && exit 1)
	git tag v$(v)
	git push origin v$(v)

clean:
	rm -rf bin/ *.db *.db-journal *.db-wal *.db-shm coverage.out *.prof

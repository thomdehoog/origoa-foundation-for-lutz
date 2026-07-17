.PHONY: build test run web clean

build: web
	go build -o bin/origoad ./cmd/origoad

test:
	go vet ./...
	go test -race ./...

web:
	cd web && npm install --no-audit --no-fund && npm run build

run: build
	./bin/origoad -repo data/origoa.git -web web/dist

clean:
	rm -rf bin web/dist web/node_modules

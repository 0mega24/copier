.PHONY: build test check
build:
	go build -o copier .
test:
	go test -race ./...
	python3 -m unittest discover -s tests -v
check: test
	go vet ./...

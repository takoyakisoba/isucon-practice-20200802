GO=go

.PHONY: build setup run fmt

build:
	cd ./src && ${GO} build -o ./app app.go trace.go

run:
	cd ./src && ./app

fmt:
	go fmt ./...
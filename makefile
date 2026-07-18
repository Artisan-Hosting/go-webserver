.PHONY:
	build

build:
	go build -o ./bin

run: build
	./bin

.PHONY:
	build

build:
	cd server/ && \
	go build -o ./bin 

run: build
	server/bin

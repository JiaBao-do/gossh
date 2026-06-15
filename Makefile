BINARY = gossh

.PHONY: build run install clean

build:
	go build -o $(BINARY).exe .

run:
	go run .

install:
	go install .

clean:
	go clean
	rm -f $(BINARY) $(BINARY).exe

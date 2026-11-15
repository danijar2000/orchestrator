build:
	cd web && npm run build
	go build -o bin/ ./cmd/...

server: build
	./bin/server

agent:
	go build -o bin/ ./cmd/agent && ./bin/agent

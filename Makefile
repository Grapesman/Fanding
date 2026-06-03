.PHONY: run build docker-up docker-down fmt
run:
	go run ./cmd/bot -env .env
build:
	go build ./cmd/bot
fmt:
	gofmt -w cmd internal
docker-up:
	docker compose up --build
docker-down:
	docker compose down

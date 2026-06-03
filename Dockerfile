FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /funding-bot ./cmd/bot

FROM alpine:3.20
RUN adduser -D appuser
USER appuser
WORKDIR /app
COPY --from=build /funding-bot /app/funding-bot
COPY --from=build /app/web /app/web
CMD ["/app/funding-bot", "-env", "/app/.env"]

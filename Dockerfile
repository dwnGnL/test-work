# syntax=docker/dockerfile:1

# --- сборка ---
FROM golang:1.25-alpine AS build
WORKDIR /src

# Сначала зависимости: слой кэшируется, пока go.mod и go.sum не менялись.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Статический бинарник без cgo; миграции вшиты в него через embed.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# --- итоговый образ ---
# static: только корневые сертификаты и /etc/passwd, без shell и пакетного менеджера.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]

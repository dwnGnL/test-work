// Package migrations содержит SQL-миграции, вшитые в бинарник.
//
// Файлы применяются по порядку имён (0001_..., 0002_...), по одному файлу
// на модуль. Раннер лежит в internal/platform/postgres.
package migrations

import "embed"

// FS — все файлы *.sql из этого каталога.
//
//go:embed *.sql
var FS embed.FS

-- Модуль scanner: курсор обработанных блоков.
CREATE SCHEMA scanner;

CREATE TABLE scanner.blocks (                       -- обработанные блоки: курсор и поиск общего предка при реорге
  number      BIGINT PRIMARY KEY,
  hash        TEXT NOT NULL,
  parent_hash TEXT NOT NULL,
  scanned_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

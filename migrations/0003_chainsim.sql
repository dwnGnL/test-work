-- Симулятор сети (модуль chain): внешняя система, состояние хранит в своей схеме.
CREATE SCHEMA chainsim;

CREATE TABLE chainsim.blocks (
  number      BIGINT PRIMARY KEY,
  hash        TEXT NOT NULL UNIQUE,
  parent_hash TEXT NOT NULL
);

CREATE TABLE chainsim.transfers (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  tx_hash      TEXT NOT NULL,
  log_index    INT NOT NULL,                       -- позиция события внутри транзакции
  token        TEXT NOT NULL,                      -- адрес контракта токена
  from_address TEXT NOT NULL,
  to_address   TEXT NOT NULL,
  amount       BIGINT NOT NULL CHECK (amount >= 0),
  block_hash   TEXT REFERENCES chainsim.blocks(hash) ON DELETE SET NULL,  -- NULL: транзакция в мемпуле
  UNIQUE (tx_hash, log_index)
);

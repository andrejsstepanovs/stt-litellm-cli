-- +goose Up
CREATE TABLE transcriptions (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	created_at    INTEGER NOT NULL,
	audio_ms      INTEGER NOT NULL,
	transcribe_ms INTEGER NOT NULL,
	model         TEXT    NOT NULL,
	chars         INTEGER NOT NULL,
	text          TEXT    NOT NULL
);
CREATE INDEX idx_transcriptions_model ON transcriptions(model);

-- +goose Down
DROP INDEX idx_transcriptions_model;
DROP TABLE transcriptions;

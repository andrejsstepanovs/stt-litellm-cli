-- +goose Up
ALTER TABLE transcriptions ADD COLUMN status TEXT NOT NULL DEFAULT 'success';
ALTER TABLE transcriptions ADD COLUMN path   TEXT NOT NULL DEFAULT '';
ALTER TABLE transcriptions ADD COLUMN error  TEXT NOT NULL DEFAULT '';
ALTER TABLE transcriptions ADD COLUMN audio  BLOB;
CREATE INDEX idx_transcriptions_status ON transcriptions(status);

-- +goose Down
DROP INDEX idx_transcriptions_status;
ALTER TABLE transcriptions DROP COLUMN audio;
ALTER TABLE transcriptions DROP COLUMN error;
ALTER TABLE transcriptions DROP COLUMN path;
ALTER TABLE transcriptions DROP COLUMN status;

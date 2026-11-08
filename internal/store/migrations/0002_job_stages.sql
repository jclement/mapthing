-- Per-stage progress of a job (JSON array) and the generation a build produced.
ALTER TABLE jobs ADD COLUMN stages TEXT NOT NULL DEFAULT '[]';
ALTER TABLE jobs ADD COLUMN generation TEXT NOT NULL DEFAULT '';

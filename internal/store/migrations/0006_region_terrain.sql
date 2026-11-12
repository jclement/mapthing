-- Per-region terrain: built ahead of time (prebuilt, as before) or read on demand from the
-- public DEM hosts when a tile or an elevation is first asked for (on_demand). Additive, so
-- an older version still reads the table (and builds every region's terrain, as it did).
ALTER TABLE regions ADD COLUMN terrain_mode TEXT NOT NULL DEFAULT 'prebuilt';

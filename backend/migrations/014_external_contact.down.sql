-- Migration: 014_external_contact (down)
-- Description: Drop external contact tables

DROP TRIGGER IF EXISTS update_external_contact_updated_at ON external_contact;

DROP TABLE IF EXISTS contact_enrichment;
DROP TABLE IF EXISTS external_contact;

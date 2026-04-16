-- Modify "campaigns" table
ALTER TABLE "campaigns" ADD COLUMN "scope" character varying NOT NULL DEFAULT 'tenant';
-- Modify "marketflow_roles" table
ALTER TABLE "marketflow_roles" ADD COLUMN "scope" character varying NOT NULL DEFAULT 'tenant';
-- Modify "nurture_sequences" table
ALTER TABLE "nurture_sequences" ADD COLUMN "scope" character varying NOT NULL DEFAULT 'tenant';

-- Modify "campaigns" table
ALTER TABLE "campaigns" ADD COLUMN "channel" character varying NOT NULL DEFAULT 'email';

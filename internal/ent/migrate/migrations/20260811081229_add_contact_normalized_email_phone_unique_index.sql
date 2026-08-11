-- Modify "contacts" table
ALTER TABLE "contacts" ADD COLUMN "email_normalized" character varying NULL, ADD COLUMN "phone_subscriber_digits" character varying NULL;
-- Backfill the normalized columns for every EXISTING contact from its raw email/phone — without
-- this, Create's dedup check (which now queries email_normalized/phone_subscriber_digits instead
-- of the raw columns) would find nothing for any pre-existing contact and start creating fresh
-- duplicates for them on their very next interaction. Confirmed via a live prod scan (2026-08-11)
-- that zero tenants have any pre-existing (tenant_id, normalized value) collision, so it is safe
-- to add the unique indexes in the SAME migration as this backfill.
UPDATE "contacts" SET "email_normalized" = lower(trim("email")) WHERE "email" IS NOT NULL AND trim("email") <> '';
UPDATE "contacts" SET "phone_subscriber_digits" = right(regexp_replace("phone", '[^0-9]', '', 'g'), 9) WHERE "phone" IS NOT NULL AND regexp_replace("phone", '[^0-9]', '', 'g') <> '';
-- Create index "contact_tenant_id_email_normalized" to table: "contacts"
CREATE UNIQUE INDEX "contact_tenant_id_email_normalized" ON "contacts" ("tenant_id", "email_normalized") WHERE ((email_normalized)::text <> ''::text);
-- Create index "contact_tenant_id_phone_subscriber_digits" to table: "contacts"
CREATE UNIQUE INDEX "contact_tenant_id_phone_subscriber_digits" ON "contacts" ("tenant_id", "phone_subscriber_digits") WHERE ((phone_subscriber_digits)::text <> ''::text);

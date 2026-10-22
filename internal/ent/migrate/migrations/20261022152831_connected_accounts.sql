-- Create "connected_accounts" table
CREATE TABLE "connected_accounts" ("id" uuid NOT NULL, "platform" character varying NOT NULL, "account_type" character varying NOT NULL, "external_id" character varying NOT NULL, "display_name" character varying NULL, "scopes" jsonb NULL, "token_ciphertext" text NULL, "refresh_ciphertext" text NULL, "expires_at" timestamptz NULL, "status" character varying NOT NULL DEFAULT 'active', "connected_by" uuid NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "connected_accounts_tenants_connected_accounts" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "connectedaccount_external_id_account_type" to table: "connected_accounts"
CREATE INDEX "connectedaccount_external_id_account_type" ON "connected_accounts" ("external_id", "account_type");
-- Create index "connectedaccount_status_expires_at" to table: "connected_accounts"
CREATE INDEX "connectedaccount_status_expires_at" ON "connected_accounts" ("status", "expires_at");
-- Create index "connectedaccount_tenant_id_platform_account_type_external_id" to table: "connected_accounts"
CREATE UNIQUE INDEX "connectedaccount_tenant_id_platform_account_type_external_id" ON "connected_accounts" ("tenant_id", "platform", "account_type", "external_id");
-- Create index "connectedaccount_tenant_id_status" to table: "connected_accounts"
CREATE INDEX "connectedaccount_tenant_id_status" ON "connected_accounts" ("tenant_id", "status");

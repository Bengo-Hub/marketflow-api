-- Drop index "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa" from table: "provider_settings"
DROP INDEX "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa";
-- Create index "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa" to table: "provider_settings"
CREATE UNIQUE INDEX "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa" ON "provider_settings" ("tenant_id", "environment", "provider_type", "provider_name", "key");
-- Create index "tenant_fbo_distrib_id" to table: "tenants"
CREATE UNIQUE INDEX "tenant_fbo_distrib_id" ON "tenants" ("fbo_distrib_id") WHERE ((fbo_distrib_id IS NOT NULL) AND ((fbo_distrib_id)::text <> ''::text));
-- Create index "tenant_whatsapp_number" to table: "tenants"
CREATE UNIQUE INDEX "tenant_whatsapp_number" ON "tenants" ("whatsapp_number") WHERE ((whatsapp_number IS NOT NULL) AND ((whatsapp_number)::text <> ''::text));

-- Modify "scheduled_meetings" table: rename cal_booking_id -> external_booking_id
ALTER TABLE "scheduled_meetings" ADD COLUMN "external_booking_id" character varying NULL;
-- Create index "scheduledmeeting_external_booking_id" to table: "scheduled_meetings"
CREATE INDEX "scheduledmeeting_external_booking_id" ON "scheduled_meetings" ("external_booking_id");
-- Drop index "scheduledmeeting_cal_booking_id" from table: "scheduled_meetings"
DROP INDEX "scheduledmeeting_cal_booking_id";
-- Drop column "cal_booking_id" from table: "scheduled_meetings"
ALTER TABLE "scheduled_meetings" DROP COLUMN "cal_booking_id";

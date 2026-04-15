-- Modify "scheduled_meetings" table
ALTER TABLE "scheduled_meetings" ADD COLUMN "google_event_id" character varying NULL, ADD COLUMN "google_calendar_id" character varying NULL, ADD COLUMN "google_event_link" character varying NULL, ADD COLUMN "attendee_email" character varying NULL, ADD COLUMN "attendee_name" character varying NULL, ADD COLUMN "organizer_email" character varying NULL;
-- Create index "scheduledmeeting_google_event_id" to table: "scheduled_meetings"
CREATE INDEX "scheduledmeeting_google_event_id" ON "scheduled_meetings" ("google_event_id");

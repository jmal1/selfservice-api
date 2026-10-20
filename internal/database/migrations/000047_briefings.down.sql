BEGIN;

ALTER TABLE briefings DROP CONSTRAINT briefings_published_fk;
DROP TABLE briefing_revisions;
DROP TABLE briefings;

COMMIT;

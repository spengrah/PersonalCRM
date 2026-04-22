-- 044_drop_event_shadow_observations.up.sql
-- Retires the three event-bus shadow-mode observation tables
-- (event_shadow_observation, event_shadow_cadence_observation,
-- event_shadow_followup_observation) introduced in migrations 038, 039, 042.
-- All consumers ran their post-bake divergence analysis in earlier PRs and
-- are now in cutover mode; the shadow tables carry no operational value.
--
-- Forward-only: the .down.sql re-creates the tables empty for rollback
-- safety only. Historical observation rows are not restored.
DROP TABLE IF EXISTS event_shadow_followup_observation;
DROP TABLE IF EXISTS event_shadow_cadence_observation;
DROP TABLE IF EXISTS event_shadow_observation;

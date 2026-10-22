-- Allow autopilots to deliver their output as a message in an existing
-- conversation instead of creating an issue.
ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_execution_mode_check;

ALTER TABLE autopilot
    ADD CONSTRAINT autopilot_execution_mode_check
    CHECK (execution_mode IN ('create_issue', 'run_only', 'message'));

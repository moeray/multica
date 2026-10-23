-- Revert message-mode autopilots to the closest existing behavior. The
-- constraint cannot be restored while rows still carry the new value.
UPDATE autopilot SET execution_mode = 'create_issue' WHERE execution_mode = 'message';

ALTER TABLE autopilot DROP CONSTRAINT IF EXISTS autopilot_execution_mode_check;

ALTER TABLE autopilot
    ADD CONSTRAINT autopilot_execution_mode_check
    CHECK (execution_mode IN ('create_issue', 'run_only'));

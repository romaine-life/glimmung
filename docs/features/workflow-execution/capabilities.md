# Workflow Execution Capabilities

## operator-control-pins

- **Status:** active
- **Intent:** Operator-chosen control values (recycle policies, budget) must
  survive agent re-registrations. Observed failure this exists to prevent:
  `ambience.default` `llm-verify` `max_attempts` was deliberately set to 1 by
  the operator on 2026-06-11 and flipped back to 3 by a session
  re-registration the same afternoon (schema `wfs_bb87d34023e3bb4c`); earlier,
  an explicit 3 first materialized as a side effect of an unrelated migration
  re-registration (2026-06-08, `wfs_1ae90a036a8066b7`) with no record of
  anyone deciding it. Workflow documents are replaced wholesale on register,
  so every re-registration silently re-decides every control value unless the
  control plane defends them.
- **Mechanism:** operator-owned `workflows.control_pins` column enforced at
  the single registration choke point; append-only `workflow_control_events`
  attribution ledger (actor + control diff per write); explicit
  `recycle_policy` required on verification phases; `X-Glimmung-Actor`
  forwarded caller detail on service writes.
- **Affected contracts:** Workflow Execution (pins, ledger, validation);
  Auth And API Surface (three new routes, MCP rollout in `mcp-glimmung`);
  Dashboard And Styleguide (pin state + control history on the workflow
  page).
- **Evidence:** `TestEnforceControlPins*`, `TestControlChanges*`,
  `TestPinWorkflowControl*`, `TestValidateWorkflowRegisterRejectsVerificationPhaseWithoutRecyclePolicy`,
  route inventory update, `RecyclePolicyPanel` pin-flow tests, and the live
  post-deploy check: a re-registration attempting `max_attempts: 3` against
  the pinned `ambience.default` verify lane is held at 1 with a
  `pin_enforced` ledger entry.

package db

import "testing"

func TestAgentTriggersOnlyScheduleExistingEnabledAgents(t *testing.T) {
	_, dsn := agentModeTestDB(t)
	d := agentModeOpen(t, dsn)
	migrationExec(t, d, `DELETE FROM agent_triggers`)
	migrationExec(t, d, `UPDATE agents SET enabled=false WHERE key='worker'`)
	migrationExec(t, d, `INSERT INTO agent_triggers (agent_key, enabled, interval_sec)
VALUES ('auto', true, 60), ('worker', true, 60), ('agent', true, 60)`)
	active, err := d.ListEnabledTriggers()
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].AgentKey != "agent" {
		t.Fatalf("retired or disabled agents entered the schedule: %+v", active)
	}
	archived, err := d.ListTriggersFor("auto")
	if err != nil || len(archived) != 1 || !archived[0].Enabled {
		t.Fatalf("the historical trigger was changed or discarded: %+v, %v", archived, err)
	}
}

package agent

import actool "github.com/Autumn-27/norma/tool"

// AvailableDomainTools shares operations according to their actual execution
// dependencies. A session without a task never receives a fabricated task store.
// goal_met is a planner-local completion signal and is added by its caller.
func (t *ToolSet) AvailableDomainTools() []actool.CoreTool {
	var tools []actool.CoreTool
	if t.as != nil {
		tools = append(tools, t.insertAssets(), t.listAssets())
		if t.cs != nil {
			tools = append(tools, t.addCompanyScope(), t.listCompanies())
		}
		if t.taskID > 0 && !t.CoverageDisabled() {
			tools = append(tools, t.addTaskScope(), t.listUntestedAssets())
		}
	}
	if t.ts != nil {
		tools = append(tools,
			t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
			t.expandDigest(), t.getWorkerOutput(), t.getWorkerTrace(),
			t.searchAllWorkerTraces(), t.listWorkerTraces(), t.listGoals(),
			t.addHint(), t.addIntent(), t.setGoals(), t.setConstraints(),
			t.proveGoal(), t.recordFact(), t.addFinding(),
		)
		if t.killWork != nil {
			tools = append(tools, t.killWorkTool())
		}
		if t.steerWork != nil {
			tools = append(tools, t.steerWorkTool())
		}
	}
	return tools
}

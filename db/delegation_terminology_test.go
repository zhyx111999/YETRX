package db

import "testing"

func TestExplorationRoleTerminologyPreservesUserEdits(t *testing.T) {
	d, dsn := nativeMigrationDB(t, true)
	nativeOpen(t, dsn)
	for _, role := range []struct{ key, name, description string }{
		{"planner", "规划", "读取态势、判定目标，只在确有未覆盖的新方向时补充探索意图（每任务一个规划循环）。"},
		{"worker", "执行", "领取一条意图执行，把发现的事实/漏洞写回知识图谱后停止。"},
	} {
		nativeExec(t, d, `UPDATE agents SET name=$2,description=$3,enabled=false,run_seconds=77 WHERE key=$1`, role.key, role.name, role.description)
	}
	prompts := nativeSnapshot(t, d, `SELECT * FROM agent_prompts ORDER BY agent_id,version`)
	nativeOpen(t, dsn)
	for key, name := range map[string]string{"planner": "子任务规划", "worker": "子任务执行"} {
		a, err := d.GetAgentByKey(key)
		if err != nil || a == nil || a.Name != name || a.Enabled || a.RunSecs != 77 {
			t.Fatalf("role defaults or preferences mismatch: %+v, %v", a, err)
		}
		nativeExec(t, d, `UPDATE agents SET name=$2,description=$3 WHERE key=$1`, key, "我的"+key, "自定义说明")
	}
	for range 2 {
		nativeOpen(t, dsn)
	}
	for _, key := range []string{"planner", "worker"} {
		a, err := d.GetAgentByKey(key)
		if err != nil || a == nil || a.Name != "我的"+key || a.Description != "自定义说明" || a.Enabled || a.RunSecs != 77 {
			t.Fatalf("user role preferences changed: %+v, %v", a, err)
		}
	}
	if after := nativeSnapshot(t, d, `SELECT * FROM agent_prompts ORDER BY agent_id,version`); prompts != after {
		t.Fatal("terminology changed prompt history")
	}
}

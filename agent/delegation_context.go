package agent

// These runtime labels identify the execution mechanism independently of the
// editable mode prompt. They never replace stored user prompts or transcripts.
const ExplorationSubtaskContext = `【执行定位：子任务（探索）】
当前属于探索任务的规划与执行流程：围绕任务目标，由规划者持续规划，执行器领取探索意图、执行并将结果写回探索记录。每个执行器只完成当前领取的意图；持续推进由探索引擎协调。task_id 和 intent_id 标识探索任务与执行事项。`

const SubagentContext = `【执行定位：子代理（对话）】
你是主对话按需启动的独立子代理。只完成本次明确委派的求证、审查或小范围修改，报告实际操作、证据和结果后返回，由主对话决定是否采纳或合并。需要持续规划的探索目标应交回主对话决定是否建立探索子任务，不自行扩展工作范围。`
